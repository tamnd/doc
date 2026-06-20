package mvcc

import (
	"fmt"

	"github.com/tamnd/doc/storage"
)

// ConflictError reports a first-committer-wins write-write conflict: a concurrent
// transaction committed a write to one of this transaction's records after its
// snapshot began (spec 2061 doc 06 §8.4). It is retriable, and unwraps to
// storage.ErrConflict so callers can match it with errors.Is.
type ConflictError struct {
	ConflictVer uint64 // commit version of the transaction that won the race
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("mvcc: write-write conflict with commit version %d", e.ConflictVer)
}

// Unwrap lets errors.Is(err, storage.ErrConflict) match a ConflictError.
func (e *ConflictError) Unwrap() error { return storage.ErrConflict }

// engine is the seam between the transaction and the store that holds the
// committed version chains. Commit calls publish after the oracle assigns the
// commit version; Rollback and a failed commit call discard. The in-memory Store
// here and the durable heap-plus-index engine in M2-c both implement it, so the
// transaction logic is identical over both.
type engine interface {
	publish(t *Txn)
	discard(t *Txn)
}

// Txn is the unified MVCC transaction handle (spec 2061 doc 06 §5, §7). It
// satisfies storage.Txn and additionally exposes the start version, commit
// version, and transaction id the version layer stamps. A write transaction
// buffers its versions in pending until commit, giving read-your-writes, a free
// abort, and full isolation of in-flight state from every other transaction.
type Txn struct {
	o       *Oracle
	eng     engine
	durable func() error // WAL fsync hook, run before the commit version is assigned

	startVer  uint64
	commitVer uint64
	txnID     uint64
	writable  bool
	done      bool

	pending  map[uint64]*version // record key -> this txn's uncommitted version
	writeSet map[uint64]struct{} // record keys written, for conflict detection
}

// Snapshot returns the start version: reads see versions committed at or before
// it, plus this transaction's own pending writes.
func (t *Txn) Snapshot() uint64 { return t.startVer }

// WriteVersion returns the in-flight stamp the version layer puts on records this
// transaction writes: the transaction id while in flight, rewritten to the commit
// version when the transaction publishes (spec 2061 doc 06 §3.1). It is distinct
// from the commit version, which is unknown until commit.
func (t *Txn) WriteVersion() uint64 { return t.txnID }

// StartVersion, CommitVersion, and TxnID expose the MVCC stamps for the version
// layer and tests. CommitVersion is 0 until the transaction commits.
func (t *Txn) StartVersion() uint64  { return t.startVer }
func (t *Txn) CommitVersion() uint64 { return t.commitVer }
func (t *Txn) TxnID() uint64         { return t.txnID }

// IsReadOnly reports a read-only snapshot transaction.
func (t *Txn) IsReadOnly() bool { return !t.writable }

// LogRecord is a no-op: the pager logs full page images on commit, so there is no
// separate logical record to append (spec 2061 doc 05, physical redo). It is here
// to satisfy storage.Txn.
func (t *Txn) LogRecord(pageNo uint32, offset uint16, before, after []byte) error { return nil }

// Commit ends the transaction, making its writes visible to future snapshots. A
// read-only or no-write transaction just releases its snapshot. A write
// transaction runs conflict detection and the durability hook through the oracle
// (durability before visibility), then publishes its versions at the assigned
// commit version. A detected conflict aborts the transaction and returns a
// retriable ConflictError.
func (t *Txn) Commit() error {
	if t.done {
		return ErrTxnDone
	}
	if !t.writable || len(t.writeSet) == 0 {
		t.o.release(t.startVer)
		t.done = true
		return nil
	}
	_, err := t.o.commit(t, t.durable, func(cv uint64) {
		t.commitVer = cv
		if t.eng != nil {
			t.eng.publish(t)
		}
	})
	if err != nil {
		if t.eng != nil {
			t.eng.discard(t)
		}
		t.done = true
		return err
	}
	t.done = true
	return nil
}

// Rollback discards the transaction's writes and releases its snapshot. Abort is
// free: nothing durable was written, so there is nothing to undo (spec 2061 doc
// 06 §7.6). Unlike the M1 storage transactions, an MVCC transaction always
// supports rollback.
func (t *Txn) Rollback() error {
	if t.done {
		return ErrTxnDone
	}
	if t.eng != nil {
		t.eng.discard(t)
	}
	t.o.release(t.startVer)
	t.done = true
	return nil
}

// sortedWriteSet returns the transaction's written record keys sorted, for the
// oracle's conflict check.
func (t *Txn) sortedWriteSet() []uint64 { return sortedKeys(t.writeSet) }

// note records that the transaction wrote record key and buffers its version.
func (t *Txn) note(key uint64, v *version) {
	if t.pending == nil {
		t.pending = make(map[uint64]*version)
		t.writeSet = make(map[uint64]struct{})
	}
	t.pending[key] = v
	t.writeSet[key] = struct{}{}
}

var _ storage.Txn = (*Txn)(nil)
