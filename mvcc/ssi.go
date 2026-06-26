package mvcc

import "slices"

// This file implements serializable snapshot isolation (SSI) over the snapshot-
// isolation oracle, following Cahill et al. (2008) as used in PostgreSQL (spec 2061
// doc 06 §10). SI already prevents every anomaly except write skew: two concurrent
// transactions each read an overlapping set and each write a disjoint part of it, so
// both commit and produce a state no serial order could. SSI catches that by tracking
// read-write antidependency edges among concurrent serializable transactions and
// aborting the pivot of a dangerous structure.
//
// An edge T1 -> T2 (a read-write antidependency) means T1 read a version of some
// document that T2 then overwrote with a version T1 could not see (T2 committed after
// T1's snapshot began, and the two overlap in time). In any equivalent serial order
// T1 must precede T2. A transaction with both an incoming and an outgoing such edge,
// with a committed neighbor on at least one side, is the pivot of a dangerous
// structure and is aborted. Over-aborting is always safe for serializability, so the
// detection here is an over-approximation that never misses a real anomaly.
//
// Detection is split between read time and commit time. At read time a serializable
// transaction that reads a key a concurrent committed transaction already wrote gains
// an outgoing edge. At commit time the committing writer checks, against every
// concurrent serializable transaction still live or recently committed, whether some
// of them read a key it is writing (an incoming edge) or wrote a key it read (an
// outgoing edge). Because the oracle serializes commits under its mutex, the last
// transaction of any antidependency cycle to commit always finds both its neighbors
// committed and aborts, which breaks the cycle.

// ssiTxn is the live tracking state of a serializable transaction: the set of keys it
// has read and the two conflict flags. doomed records that a committed neighbor became
// a pivot because of an edge this transaction formed, so this transaction must abort at
// commit to break that edge even if its own two flags are not both set.
type ssiTxn struct {
	startVer    uint64
	reads       map[uint64]struct{}
	inConflict  bool // some concurrent transaction read a key this one writes
	outConflict bool // this one read a key some concurrent transaction wrote
	doomed      bool // forced abort: an edge this txn formed completed a pivot elsewhere
}

// ssiCommitted is the retained record of a committed serializable transaction, kept in
// the oracle's ssiRecent window so a later concurrent transaction can form edges
// against it. It is pruned once no live transaction can still be concurrent with it.
type ssiCommitted struct {
	commitVer   uint64
	startVer    uint64
	reads       []uint64 // sorted
	writes      []uint64 // sorted
	inConflict  bool
	outConflict bool
}

// RegisterSSI starts tracking a serializable transaction. It is called at begin for a
// transaction whose isolation is Serializable; a snapshot-isolation transaction never
// calls it and so never appears in the SSI structures.
func (o *Oracle) RegisterSSI(txnID, startVer uint64) {
	o.mu.Lock()
	o.ssiLive[txnID] = &ssiTxn{startVer: startVer, reads: make(map[uint64]struct{})}
	o.mu.Unlock()
}

// ReleaseSSI stops tracking a serializable transaction that ended without committing a
// write (a read-only commit or a rollback). A committing writer goes through
// CommitSerializable, which removes the live entry itself.
func (o *Oracle) ReleaseSSI(txnID uint64) {
	o.mu.Lock()
	delete(o.ssiLive, txnID)
	o.mu.Unlock()
}

// RecordRead notes that a serializable transaction read a key and detects the read-time
// outgoing edge: if a concurrent committed transaction already wrote that key (a version
// this reader cannot see), this reader gains an outgoing antidependency to it. Should
// that complete a pivot at the committed neighbor, this reader is doomed to abort at its
// own commit, since the neighbor can no longer be aborted.
func (o *Oracle) RecordRead(txnID, startVer, key uint64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	t := o.ssiLive[txnID]
	if t == nil {
		return
	}
	t.reads[key] = struct{}{}
	for i := range o.ssiRecent {
		c := &o.ssiRecent[i]
		if c.commitVer <= startVer {
			continue // committed at or before our snapshot: visible to us, not an antidependency
		}
		if _, found := slices.BinarySearch(c.writes, key); found {
			t.outConflict = true
			c.inConflict = true
			if c.inConflict && c.outConflict {
				t.doomed = true
			}
		}
	}
}

// CommitSerializable commits a serializable write transaction. It first runs the same
// first-committer-wins write-write check as the snapshot-isolation path, then the SSI
// pivot detection, and only if both pass does it apply the durable write and publish the
// versions. A detected pivot aborts with a SerializationFailureError; the write-write
// check aborts with a ConflictError. Both are retriable.
func (o *Oracle) CommitSerializable(startVer, txnID uint64, writeSet []uint64, durable func(commitVer uint64) error, publish func(commitVer uint64)) (uint64, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	// (1) First-committer-wins: a concurrent transaction that already committed a
	// write to one of our documents wins, and we abort retriably.
	for i := len(o.recent) - 1; i >= 0; i-- {
		c := o.recent[i]
		if c.commitVer <= startVer {
			break
		}
		if intersects(c.writeSet, writeSet) {
			o.abortSSILocked(startVer, txnID)
			return 0, &ConflictError{ConflictVer: c.commitVer}
		}
	}

	// (2) SSI pivot detection. Start from any flags already set at read time.
	self := o.ssiLive[txnID]
	var selfReads map[uint64]struct{}
	inConflict, outConflict, doomed := false, false, false
	if self != nil {
		selfReads = self.reads
		inConflict, outConflict, doomed = self.inConflict, self.outConflict, self.doomed
	}

	// Incoming edges from live readers: a concurrent live transaction read a key we
	// are writing, so reader -> us. If that reader already had an incoming edge it
	// becomes a pivot, and since it is live we abort ourselves to break the edge.
	for id, t := range o.ssiLive {
		if id == txnID {
			continue
		}
		if intersectsMapSorted(t.reads, writeSet) {
			inConflict = true
			t.outConflict = true
			if t.inConflict && t.outConflict {
				doomed = true
			}
		}
	}

	// Edges against recently committed concurrent transactions.
	for i := range o.ssiRecent {
		c := &o.ssiRecent[i]
		if c.commitVer <= startVer {
			continue // not concurrent: committed before our snapshot began
		}
		// Incoming: the committed transaction read a key we are writing.
		if intersects(c.reads, writeSet) {
			inConflict = true
			c.outConflict = true
			if c.inConflict && c.outConflict {
				doomed = true
			}
		}
		// Outgoing: the committed transaction wrote a key we read.
		if selfReads != nil && intersectsMapSorted(selfReads, c.writes) {
			outConflict = true
			c.inConflict = true
			if c.inConflict && c.outConflict {
				doomed = true
			}
		}
	}

	if doomed || (inConflict && outConflict) {
		o.abortSSILocked(startVer, txnID)
		return 0, &SerializationFailureError{}
	}

	// (3) Durability before visibility, identical to the SI commit path.
	cv := o.commitVer + 1
	if durable != nil {
		if err := durable(cv); err != nil {
			o.abortSSILocked(startVer, txnID)
			return 0, err
		}
	}
	if publish != nil {
		publish(cv)
	}
	o.commitVer = cv
	o.recent = append(o.recent, committedTxn{commitVer: cv, writeSet: writeSet})
	o.ssiRecent = append(o.ssiRecent, ssiCommitted{
		commitVer:   cv,
		startVer:    startVer,
		reads:       sortedKeys(selfReads),
		writes:      append([]uint64(nil), writeSet...),
		inConflict:  inConflict,
		outConflict: outConflict,
	})
	delete(o.ssiLive, txnID)
	o.releaseLocked(startVer)
	o.pruneLocked()
	o.pruneSSILocked()
	return cv, nil
}

// abortSSILocked tears down a serializable transaction that is aborting, mirroring the
// snapshot-release and pruning a normal commit or rollback would do.
func (o *Oracle) abortSSILocked(startVer, txnID uint64) {
	delete(o.ssiLive, txnID)
	o.releaseLocked(startVer)
	o.pruneLocked()
	o.pruneSSILocked()
}

// pruneSSILocked drops committed-serializable records no live transaction can still be
// concurrent with: any record whose commit version is at or below the watermark, since
// every live transaction started at or after it and only forms edges against records
// committed strictly after its own snapshot.
func (o *Oracle) pruneSSILocked() {
	wm := o.minLive
	cut := 0
	for cut < len(o.ssiRecent) && o.ssiRecent[cut].commitVer <= wm {
		cut++
	}
	if cut > 0 {
		o.ssiRecent = append(o.ssiRecent[:0], o.ssiRecent[cut:]...)
	}
}

// intersectsMapSorted reports whether any key in the sorted set s is present in the map
// set m. Membership in m is O(1), so this is O(len(s)).
func intersectsMapSorted(m map[uint64]struct{}, s []uint64) bool {
	for _, k := range s {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}
