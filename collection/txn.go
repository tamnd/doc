package collection

import (
	"bytes"

	"github.com/tamnd/doc/bson"
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

// valuesEqual reports whether two BSON values are equal for M2-c's exact-match
// filter and the conformance corpus: identical type and identical payload bytes.
// Cross-type numeric equality (5 == 5.0) and the full comparison order arrive
// with the query engine in M3 (spec 2061 doc 02 §7); the corpus stays within
// same-type equality until then.
func valuesEqual(a, b bson.RawValue) bool {
	return a.Type == b.Type && bytes.Equal(a.Data, b.Data)
}
