package mvcc

import (
	"errors"
	"slices"
	"sync"
)

// ErrTxnDone reports a Commit or Rollback on a transaction that already ended.
var ErrTxnDone = errors.New("mvcc: transaction already finished")

// committedTxn is one entry in the oracle's committed-since index: the commit
// version a write transaction was assigned and the sorted set of record keys it
// wrote, used for write-write conflict detection (spec 2061 doc 06 §8.2).
type committedTxn struct {
	commitVer uint64
	writeSet  []uint64 // sorted storage.RID.Encode() values
}

// Oracle assigns versions and tracks live snapshots (spec 2061 doc 06 §4). It is
// a small, low-contention structure consulted only at transaction begin and
// commit, never on the per-record read or write path.
type Oracle struct {
	mu        sync.Mutex
	commitVer uint64         // latest assigned commit version (0 = none yet)
	nextTxn   uint64         // monotonic transaction-id source
	live      map[uint64]int // start version -> count of live snapshots at it
	minLive   uint64         // cached watermark: min live start version, or commitVer if none
	recent    []committedTxn // committed-since index, ascending by commit version
}

// NewOracle returns an oracle whose commit-version counter starts at
// startCommitVer. On a fresh database this is 0 (version 0 means "no committed
// version"); on recovery it is the maximum commit version found in the WAL, so
// post-recovery transactions get versions strictly above any stored one (spec
// 2061 doc 06 §4.1, §4.6).
func NewOracle(startCommitVer uint64) *Oracle {
	return &Oracle{
		commitVer: startCommitVer,
		live:      make(map[uint64]int),
		minLive:   startCommitVer,
	}
}

// Begin registers a new snapshot at the current commit version and returns the
// transaction reading from it. A read-only transaction never writes; a writable
// one accumulates a write set the oracle validates at commit. The returned start
// version is held in the live set until the transaction commits or rolls back,
// pinning the watermark for GC (spec 2061 doc 06 §5.1).
func (o *Oracle) Begin(writable bool) *Txn {
	startVer, tid := o.Acquire()
	return &Txn{
		o:        o,
		startVer: startVer,
		txnID:    tid,
		writable: writable,
	}
}

// Acquire registers a snapshot at the current commit version and returns its
// start version and a fresh transaction id. It is the lower-level primitive
// behind Begin, exposed so a storage engine that manages its own version data
// (the durable collection layer) can drive the oracle without an mvcc.Txn. The
// caller must eventually Release the start version, or pass it to Commit.
func (o *Oracle) Acquire() (startVer, txnID uint64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	startVer = o.commitVer
	o.nextTxn++
	if len(o.live) == 0 {
		o.minLive = startVer
	}
	o.live[startVer]++
	return startVer, o.nextTxn
}

// commit runs first-committer-wins conflict detection over the committed-since
// index, then (durability before visibility) invokes durable, installs the
// transaction's versions through publish, and only then advances the commit
// version that future snapshots read. Doing the install and the version bump
// under the same lock makes a commit atomic: a transaction that begins at version
// C sees every write committed at or before C fully installed, never a half-
// published one (spec 2061 doc 06 §7.5). The snapshot is released and the index
// pruned whether the commit succeeds or aborts, since the transaction is over
// either way. A read-only or no-write transaction never reaches here.
func (o *Oracle) Commit(startVer uint64, writeSet []uint64, durable func(commitVer uint64) error, publish func(commitVer uint64)) (uint64, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	for i := len(o.recent) - 1; i >= 0; i-- {
		c := o.recent[i]
		if c.commitVer <= startVer {
			break // older entries committed at or before the snapshot cannot conflict
		}
		if intersects(c.writeSet, writeSet) {
			o.releaseLocked(startVer)
			o.pruneLocked()
			return 0, &ConflictError{ConflictVer: c.commitVer}
		}
	}

	// The commit version is settled before the durable write so the durability
	// layer can stamp it onto the records it persists; a durable failure aborts
	// before o.commitVer advances, so the version is reused by the next commit.
	cv := o.commitVer + 1
	if durable != nil {
		if err := durable(cv); err != nil {
			o.releaseLocked(startVer)
			o.pruneLocked()
			return 0, err
		}
	}

	// Install the versions stamped with cv before advancing o.commitVer, so no
	// concurrent Begin can take a snapshot at cv until its data is in place.
	if publish != nil {
		publish(cv)
	}
	o.commitVer = cv
	o.recent = append(o.recent, committedTxn{commitVer: cv, writeSet: writeSet})
	o.releaseLocked(startVer)
	o.pruneLocked()
	return cv, nil
}

// Release deregisters a snapshot whose transaction ended without an assigned
// commit version (a read-only commit or any rollback).
func (o *Oracle) Release(startVer uint64) {
	o.mu.Lock()
	o.releaseLocked(startVer)
	o.pruneLocked()
	o.mu.Unlock()
}

// releaseLocked removes one snapshot at startVer and advances the watermark if
// the released snapshot held it. The watermark only ever rises: a new snapshot's
// start version is never below an existing one, so Begin cannot lower it; only a
// release can, by retiring the current minimum.
func (o *Oracle) releaseLocked(startVer uint64) {
	n := o.live[startVer]
	if n <= 1 {
		delete(o.live, startVer)
	} else {
		o.live[startVer] = n - 1
	}
	if startVer != o.minLive {
		return
	}
	if len(o.live) == 0 {
		o.minLive = o.commitVer
		return
	}
	min := ^uint64(0)
	for s := range o.live {
		if s < min {
			min = s
		}
	}
	o.minLive = min
}

// pruneLocked drops committed-since entries no live or future transaction can be
// validated against: any entry whose commit version is at or below the watermark,
// since every live transaction has a start version at or above it and only checks
// entries committed strictly after its start (spec 2061 doc 06 §8.2).
func (o *Oracle) pruneLocked() {
	wm := o.minLive
	cut := 0
	for cut < len(o.recent) && o.recent[cut].commitVer <= wm {
		cut++
	}
	if cut > 0 {
		o.recent = append(o.recent[:0], o.recent[cut:]...)
	}
}

// Watermark returns the oldest live snapshot's start version, or the current
// commit version when no snapshot is live. Versions below it that are superseded
// by a version at or below it are invisible to every live snapshot and eligible
// for GC (spec 2061 doc 06 §4.4, §14.2).
func (o *Oracle) Watermark() uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.minLive
}

// CommitVersion returns the latest assigned commit version.
func (o *Oracle) CommitVersion() uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.commitVer
}

// LiveSnapshots reports how many snapshots are currently registered. It is a test
// and observability hook, not a hot-path call.
func (o *Oracle) LiveSnapshots() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, c := range o.live {
		n += c
	}
	return n
}

// intersects reports whether two sorted uint64 sets share an element. Both write
// sets are small (a few RIDs for a typical transaction), so the merge walk is
// cheap (spec 2061 doc 06 §8.2).
func intersects(a, b []uint64) bool {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			return true
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return false
}

// sortedKeys returns the keys of a RID-set map as a sorted slice.
func sortedKeys(m map[uint64]struct{}) []uint64 {
	if len(m) == 0 {
		return nil
	}
	out := make([]uint64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
