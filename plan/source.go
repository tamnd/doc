package plan

import (
	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/catalog"
	"github.com/tamnd/doc/storage"
)

// Source is the storage view the execution engine reads through. The collection
// layer implements it over its MVCC overlay so every read sees one consistent
// snapshot: a collection scan and an index fetch resolve to the same document
// versions, and the engine never touches the pager or the overlay directly.
type Source interface {
	// ScanDocuments returns the documents visible at the read's snapshot in natural
	// (first-insert) order, each with its heap RID. A buffered, not-yet-committed
	// insert carries a null RID, which is fine because a collection scan never
	// fetches by RID.
	ScanDocuments() ([]Row, error)
	// OpenIndexCursor opens a forward cursor over the named secondary index between
	// the encoded field-key bounds lo and hi, honoring opts.
	OpenIndexCursor(name string, lo, hi storage.IndexKey, opts storage.ScanOpts) (storage.IndexCursor, error)
	// LookupRID resolves an index entry's RID to the document visible at the read's
	// snapshot, reporting false if no visible document owns that RID.
	LookupRID(rid storage.RID) (bson.Raw, bool)
}

// Row is one document a collection scan yields: the document and the heap RID it
// lives at (null for a buffered insert).
type Row struct {
	RID storage.RID
	Doc bson.Raw
}

// IndexDesc is the planner's read-only view of one available index: its name,
// ordered key, and the option flags that bear on planning. The collection builds
// the slice from its catalog before each plan, with the _id index first.
type IndexDesc struct {
	Name     string
	Key      []catalog.KeyPart
	Unique   bool
	Sparse   bool
	Multikey bool
	Partial  bool
}
