package collection

import (
	"github.com/tamnd/doc/storage"
)

// writeTxn is the storage.Txn the collection hands to the heap and index while it
// applies a commit's buffered writes. It carries only the commit version the MVCC
// oracle has settled, which the heap stamps onto each cell so a reopened database
// recovers the right version. It is never the unit of durability itself: the
// collection drives the pager's group commit once, after all writes are applied,
// so Commit and Rollback here are inert.
type writeTxn struct {
	version uint64
}

func (w writeTxn) Snapshot() uint64                                                   { return w.version }
func (w writeTxn) WriteVersion() uint64                                               { return w.version }
func (w writeTxn) IsReadOnly() bool                                                   { return false }
func (w writeTxn) LogRecord(pageNo uint32, offset uint16, before, after []byte) error { return nil }
func (w writeTxn) Commit() error                                                      { return nil }
func (w writeTxn) Rollback() error                                                    { return nil }

var _ storage.Txn = writeTxn{}
