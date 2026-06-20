// Package storage defines the storage SPI seam: the narrow set of interfaces
// between doc's document-level logic and the page-level substrate (pager, WAL,
// MVCC, B-tree core) reused from kv (spec 2061 doc 04 §2). In M0 these are
// definitions only — no implementation. The slotted-page record store (M1), the
// _id and secondary indexes (M2, M3), and the MVCC transaction (M2) implement
// them above the verified kv substrate.
package storage

import (
	"errors"

	"github.com/tamnd/doc/bson"
)

// Sentinel errors shared by SPI implementations.
var (
	// ErrNotFound reports a RID or key not visible at the transaction's
	// snapshot.
	ErrNotFound = errors.New("storage: not found")
	// ErrDuplicateKey reports a unique-index Put colliding with a
	// snapshot-visible entry.
	ErrDuplicateKey = errors.New("storage: duplicate key")
	// ErrConflict reports a write-write conflict detected at commit or on a
	// conflicting write under snapshot isolation.
	ErrConflict = errors.New("storage: write-write conflict")
	// ErrReadOnly reports a write attempted in a read-only transaction.
	ErrReadOnly = errors.New("storage: read-only transaction")
)

// RID is a record identifier: the (page, slot) address of a stored document
// within the .doc file (spec 2061 doc 04 §2.2, §3.2). PageNo is a 32-bit page
// number; Slot is a 16-bit index into that page's slot directory. A RID is
// stable while the document stays at the same slot; a growing update that
// relocates a document leaves a forwarding tombstone at the old RID so indexes
// pointing at it still resolve.
type RID struct {
	PageNo uint32
	Slot   uint16
}

const (
	// nullPageNo is the page number of the null RID; page 0 is the header page,
	// so a RID with PageNo 0 is also invalid as a record address.
	nullPageNo = 0xFFFFFFFF
)

// NullRID is the sentinel "no record" identifier.
var NullRID = RID{PageNo: nullPageNo, Slot: 0xFFFF}

// IsNull reports whether r is the null RID.
func (r RID) IsNull() bool { return r == NullRID }

// IsValid reports whether r can address a real record: a non-null RID whose page
// is not the header page.
func (r RID) IsValid() bool { return !r.IsNull() && r.PageNo != 0 }

// Encode packs the RID into a uint64 for storage in index entries and forwarding
// tombstones: the high 32 bits are the page number, the low 16 the slot (the
// middle 16 bits are reserved zero). The layout is order-preserving on page then
// slot, which keeps heap-order scans monotonic.
func (r RID) Encode() uint64 {
	return uint64(r.PageNo)<<32 | uint64(r.Slot)
}

// DecodeRID is the inverse of Encode.
func DecodeRID(v uint64) RID {
	return RID{PageNo: uint32(v >> 32), Slot: uint16(v)}
}

// RecordStore is the per-collection heap-file record store (spec 2061 doc 04
// §2.2). All operations take a Txn carrying the MVCC snapshot and write set.
type RecordStore interface {
	// Insert encodes doc, assigns a new RID, writes the record into a page with
	// sufficient free space, and returns the RID. The record is visible only to
	// the inserting transaction until commit.
	Insert(txn Txn, doc bson.Raw) (RID, error)

	// Lookup returns the snapshot-visible BSON for rid, following a forwarding
	// tombstone transparently. It returns ErrNotFound if the document is not
	// visible at txn's snapshot.
	Lookup(txn Txn, rid RID) (bson.Raw, error)

	// Update replaces the document at rid with newDoc, in place when it fits,
	// otherwise relocating it and leaving a forwarding tombstone. It returns the
	// final RID, which may differ from rid.
	Update(txn Txn, rid RID, newDoc bson.Raw) (RID, error)

	// Delete tombstones the record at rid at txn's version. Slot reclamation
	// happens later, once no live snapshot can see the record.
	Delete(txn Txn, rid RID) error

	// Scan returns a cursor over all snapshot-visible records in heap order.
	Scan(txn Txn) (RecordCursor, error)

	// FreeSpaceStats reports per-page free-space data for the planner and the
	// space-reclamation subsystem.
	FreeSpaceStats() FreeSpaceStats
}

// RecordCursor iterates records in heap order.
type RecordCursor interface {
	Next() bool
	RID() RID
	Doc() bson.Raw
	Err() error
	Close() error
}

// FreeSpaceStats summarizes a collection's heap occupancy for the planner.
type FreeSpaceStats struct {
	PageCount     uint64 // heap pages allocated to the collection
	LiveRecords   uint64 // live (non-tombstone) records
	DeadRecords   uint64 // dead tombstones awaiting reclamation
	FreeBytes     uint64 // total free bytes across heap pages
	OverflowPages uint64 // pages consumed by overflow chains
}

// IndexKey is an order-preserving byte encoding of one or more field values
// (spec 2061 doc 07). It compares correctly with bytes.Compare.
type IndexKey []byte

// ScanOpts controls an index scan.
type ScanOpts struct {
	Reverse   bool // scan high→low instead of low→high
	IncludeLo bool // include the lower bound (default exclusive-of-nothing semantics set by caller)
	IncludeHi bool // include the upper bound
}

// IndexStats summarizes an index for the planner.
type IndexStats struct {
	Entries      uint64 // total entries
	DistinctKeys uint64 // approximate distinct key count
	Height       int    // B-tree height
}

// IndexStore is one B-tree index on a collection (spec 2061 doc 04 §2.3). Keys
// are order-preserving-encoded field values; values are RIDs.
type IndexStore interface {
	// Put inserts or replaces (key, rid) at txn's version. A unique index returns
	// ErrDuplicateKey on a snapshot-visible collision.
	Put(txn Txn, key IndexKey, rid RID) error
	// Delete removes (key, rid) at txn's version.
	Delete(txn Txn, key IndexKey, rid RID) error
	// Get returns the RID for the exact key visible at txn's snapshot.
	Get(txn Txn, key IndexKey) (RID, error)
	// Scan returns a cursor over entries in [lo, hi) key order (subject to opts).
	Scan(txn Txn, lo, hi IndexKey, opts ScanOpts) (IndexCursor, error)
	// Stats returns cardinality and selectivity estimates for the planner.
	Stats() IndexStats
}

// IndexCursor iterates index entries in key order.
type IndexCursor interface {
	Next() bool
	Key() IndexKey
	RID() RID
	Err() error
	Close() error
}

// Txn is the transaction and snapshot handle passed to all storage operations
// (spec 2061 doc 04 §2.4). It is created and managed above the seam by the MVCC
// layer (spec 2061 doc 06), itself built on kv's MVCC substrate.
type Txn interface {
	// Snapshot returns the read-point version: reads see versions committed at or
	// before it.
	Snapshot() uint64
	// WriteVersion returns the version stamped on records written in this txn.
	WriteVersion() uint64
	// IsReadOnly reports a read-only snapshot transaction.
	IsReadOnly() bool
	// LogRecord appends a WAL record for a page mutation. doc code calls it
	// indirectly through page writes.
	LogRecord(pageNo uint32, offset uint16, before, after []byte) error
	// Commit makes all writes visible to future snapshots.
	Commit() error
	// Rollback discards all writes.
	Rollback() error
}
