package index

import "errors"

// ErrRollbackUnsupported reports a Rollback in M1, matching the heap layer: M1
// has no per-transaction undo log, so the unit of durability is an operation
// followed by Commit. Transactional rollback arrives with the MVCC layer in M2
// (spec 2061 doc 06).
var ErrRollbackUnsupported = errors.New("index: rollback is not supported in M1")

// Tx is the M1 index transaction handle; it satisfies storage.Txn. Like the
// heap's Tx it is a thin wrapper over the pager's group commit, carrying only a
// monotonic version. In M2 a single unified MVCC transaction will drive both the
// heap and the index over the same pager; the two M1 handles collapse into it.
type Tx struct {
	t        *BTree
	version  uint64
	readOnly bool
}

func (x *Tx) Snapshot() uint64     { return x.version }
func (x *Tx) WriteVersion() uint64 { return x.version }
func (x *Tx) IsReadOnly() bool     { return x.readOnly }

// LogRecord is a no-op: the pager logs full page images on commit, so there is no
// separate logical record to append (spec 2061 doc 05 - physical redo).
func (x *Tx) LogRecord(pageNo uint32, offset uint16, before, after []byte) error { return nil }

// Commit makes index writes durable through the pager's WAL group commit.
func (x *Tx) Commit() error {
	if x.readOnly {
		return nil
	}
	return x.t.pgr.Commit()
}

// Rollback always errors for a writable M1 transaction; see ErrRollbackUnsupported.
func (x *Tx) Rollback() error {
	if x.readOnly {
		return nil
	}
	return ErrRollbackUnsupported
}
