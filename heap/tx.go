package heap

import "errors"

// ErrRollbackUnsupported reports a Rollback in M1. M1 is a single-writer durable
// record store with no per-transaction undo log; transactional rollback,
// snapshot isolation, and write-conflict detection arrive with the MVCC layer in
// M2 (spec 2061 doc 06, roadmap M2). Until then the unit of durability is an
// operation followed by Commit.
var ErrRollbackUnsupported = errors.New("heap: rollback is not supported in M1")

// Tx is the M1 transaction handle. It satisfies storage.Txn. Because M1 is
// single-writer and the pager logs whole dirty pages (full-image redo) rather
// than logical records, the handle carries only a monotonic version used to
// stamp cell headers; LogRecord is a no-op and Commit defers to the pager's
// group commit.
type Tx struct {
	h        *Heap
	version  uint64
	readOnly bool
}

// Snapshot returns the read-point version. In M1 (single-version) every committed
// record is visible, so the snapshot is advisory; it becomes load-bearing in M2.
func (t *Tx) Snapshot() uint64 { return t.version }

// WriteVersion returns the version stamped on records written in this transaction.
func (t *Tx) WriteVersion() uint64 { return t.version }

// IsReadOnly reports a read-only transaction.
func (t *Tx) IsReadOnly() bool { return t.readOnly }

// LogRecord is a no-op in M1: the pager logs full page images on commit, so there
// is no separate logical record to append (spec 2061 doc 05 — physical redo).
func (t *Tx) LogRecord(pageNo uint32, offset uint16, before, after []byte) error { return nil }

// Commit makes all writes durable through the pager's WAL group commit.
func (t *Tx) Commit() error {
	if t.readOnly {
		return nil
	}
	return t.h.pgr.Commit()
}

// Rollback always errors in M1; see ErrRollbackUnsupported.
func (t *Tx) Rollback() error {
	if t.readOnly {
		return nil
	}
	return ErrRollbackUnsupported
}
