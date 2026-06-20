// Package collection is doc's document-collection layer: the M2 surface that
// turns the durable record store (heap), the unique _id index, and the MVCC
// oracle into ordered, snapshot-isolated InsertOne / FindOne / DeleteOne /
// CountDocuments operations over BSON documents (spec 2061 doc 04 §2, doc 06).
//
// A collection keeps an in-memory version overlay: the heap durably holds the
// latest committed version of every document, while the overlay holds, per _id,
// the newest-first chain of versions a live snapshot might still need to read.
// Reads are served from the overlay, so the durable heap is touched only on
// commit and on Open (to rebuild the overlay). M2-c has no update; document
// updates and secondary-index find paths arrive in M3.
package collection

import (
	"errors"
	"sync"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/heap"
	"github.com/tamnd/doc/index"
	"github.com/tamnd/doc/mvcc"
	"github.com/tamnd/doc/pager"
	"github.com/tamnd/doc/storage"
	"github.com/tamnd/doc/sys"
	"github.com/tamnd/doc/vfs"
)

// collID is the owning collection id stamped into every heap and index page. M2-c
// is one collection per file; the catalog that multiplexes collections arrives in
// M3 (spec 2061 doc 19 §22 M3).
const collID = 1

// ErrDuplicateKey reports an InsertOne whose _id already identifies a live
// document. It unwraps to storage.ErrDuplicateKey so callers can match either,
// and mirrors MongoDB's E11000 duplicate-key error.
var ErrDuplicateKey = storage.ErrDuplicateKey

// Options configures a collection at Open time.
type Options struct {
	// Pager is passed through to the underlying pager (page size, sync level,
	// read-only, pool size). Zero values select the pager's defaults.
	Pager pager.Options
	// Clock stamps generated ObjectIds; nil uses the system clock. Ignored when
	// IDGen is set.
	Clock sys.Clock
	// IDGen mints _id values for documents inserted without one; nil builds an
	// ObjectId generator over Clock. Tests inject a deterministic generator.
	IDGen sys.IDGenerator
}

// docVersion is one version of a document in the overlay chain. A nil Doc marks a
// delete tombstone: the _id existed and was removed at CommitVer, so a snapshot at
// or after CommitVer sees no document while an older snapshot still sees the prior
// version below it in the chain.
type docVersion struct {
	commitVer uint64
	rid       storage.RID
	doc       bson.Raw // nil => the document was deleted at commitVer
}

// chain is the newest-first list of versions for one _id.
type chain struct {
	versions []*docVersion
}

// visibleAt returns the document visible to a snapshot reading at ver, or nil if
// the _id has no version at or below ver or its newest such version is a delete.
func (c *chain) visibleAt(ver uint64) bson.Raw {
	for _, v := range c.versions {
		if v.commitVer <= ver {
			return v.doc
		}
	}
	return nil
}

// Collection is a single document collection over one .doc file. It is safe for
// concurrent use: reads take the collection mutex to consult the overlay, and the
// single-writer commit path is serialized by the oracle.
type Collection struct {
	pgr *pager.Pager
	hp  *heap.Heap
	idx *index.BTree
	orc *mvcc.Oracle
	gen sys.IDGenerator

	mu    sync.Mutex
	byID  map[string]*chain   // overlay key (encoded _id) -> version chain
	order []string            // overlay keys in first-insert order, for natural scan
	dirty map[string]struct{} // keys whose chain holds more than one version
}

// Open opens or creates the collection stored at path on fs, recovering the
// durable heap and _id index and rebuilding the in-memory overlay from them.
func Open(fs vfs.FS, path string, opts Options) (*Collection, error) {
	pgr, err := pager.Open(fs, path, opts.Pager)
	if err != nil {
		return nil, err
	}
	c, err := newCollection(pgr, opts)
	if err != nil {
		_ = pgr.Close()
		return nil, err
	}
	return c, nil
}

// newCollection wires the heap, index, generator, and overlay over an open pager,
// then seeds the oracle at the highest version it recovered so new commits get
// strictly greater versions (spec 2061 doc 06 §4.6).
func newCollection(pgr *pager.Pager, opts Options) (*Collection, error) {
	hp, err := heap.Open(pgr, collID)
	if err != nil {
		return nil, err
	}
	idx, err := index.Open(pgr, collID, true)
	if err != nil {
		return nil, err
	}
	gen := opts.IDGen
	if gen == nil {
		clock := opts.Clock
		if clock == nil {
			clock = sys.SystemClock{}
		}
		gen = sys.NewObjectIDGenerator(clock)
	}
	c := &Collection{
		pgr:   pgr,
		hp:    hp,
		idx:   idx,
		gen:   gen,
		byID:  make(map[string]*chain),
		dirty: make(map[string]struct{}),
	}
	maxVer, err := c.rebuild()
	if err != nil {
		return nil, err
	}
	c.orc = mvcc.NewOracle(maxVer)
	return c, nil
}

// rebuild populates the overlay from the durable heap, one version per live _id,
// and returns the highest version it saw to seed the oracle.
func (c *Collection) rebuild() (uint64, error) {
	recs, err := c.hp.Records()
	if err != nil {
		return 0, err
	}
	var maxVer uint64
	for _, r := range recs {
		idv, ok := bson.IDOf(r.Doc)
		if !ok {
			return 0, errMissingID
		}
		key, err := overlayKey(idv)
		if err != nil {
			return 0, err
		}
		if _, seen := c.byID[key]; !seen {
			c.order = append(c.order, key)
		}
		c.byID[key] = &chain{versions: []*docVersion{{
			commitVer: r.Version,
			rid:       r.RID,
			doc:       r.Doc,
		}}}
		if r.Version > maxVer {
			maxVer = r.Version
		}
	}
	return maxVer, nil
}

// errMissingID reports a stored document without an _id, which the write path
// makes impossible; it is a corruption guard on the recovery path.
var errMissingID = errors.New("collection: stored document has no _id")

// overlayKey returns the overlay and conflict key for an _id value: its order-
// preserving index encoding, which is injective across BSON types and so
// distinguishes _ids exactly (spec 2061 doc 07 §3).
func overlayKey(id bson.RawValue) (string, error) {
	k, err := index.EncodeValue(id)
	if err != nil {
		return "", err
	}
	return string(k), nil
}

// Close flushes and releases the collection's resources.
func (c *Collection) Close() error { return c.pgr.Close() }

// Begin starts a read-write transaction; BeginReadOnly starts a read-only one.
// Reads see the snapshot taken at Begin plus the transaction's own buffered
// writes; writes become visible to later snapshots only at Commit.
func (c *Collection) Begin() *Txn {
	startVer, txnID := c.orc.Acquire()
	return &Txn{c: c, startVer: startVer, txnID: txnID, writable: true}
}

func (c *Collection) BeginReadOnly() *Txn {
	startVer, txnID := c.orc.Acquire()
	return &Txn{c: c, startVer: startVer, txnID: txnID, writable: false}
}

// InsertOne inserts a single document in its own transaction, returning the stored
// _id. It is a convenience wrapper over Begin/insert/Commit.
func (c *Collection) InsertOne(d bson.Raw) (bson.RawValue, error) {
	t := c.Begin()
	id, err := t.InsertOne(d)
	if err != nil {
		_ = t.Rollback()
		return bson.RawValue{}, err
	}
	if err := t.Commit(); err != nil {
		return bson.RawValue{}, err
	}
	return id, nil
}

// FindOne returns the first document matching filter, or nil if none match.
func (c *Collection) FindOne(filter bson.Raw) (bson.Raw, error) {
	t := c.BeginReadOnly()
	defer func() { _ = t.Rollback() }()
	return t.FindOne(filter)
}

// Find returns every document matching filter in natural order.
func (c *Collection) Find(filter bson.Raw) ([]bson.Raw, error) {
	t := c.BeginReadOnly()
	defer func() { _ = t.Rollback() }()
	return t.Find(filter)
}

// FindWith returns every document matching filter, shaped by opts (projection,
// sort, skip, limit), in its own read-only transaction.
func (c *Collection) FindWith(filter bson.Raw, opts FindOptions) ([]bson.Raw, error) {
	t := c.BeginReadOnly()
	defer func() { _ = t.Rollback() }()
	return t.FindWith(filter, opts)
}

// DeleteOne deletes the first document matching filter, returning the number
// deleted (0 or 1).
func (c *Collection) DeleteOne(filter bson.Raw) (int64, error) {
	t := c.Begin()
	n, err := t.DeleteOne(filter)
	if err != nil {
		_ = t.Rollback()
		return 0, err
	}
	if err := t.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// CountDocuments returns the number of documents matching filter.
func (c *Collection) CountDocuments(filter bson.Raw) (int64, error) {
	t := c.BeginReadOnly()
	defer func() { _ = t.Rollback() }()
	return t.CountDocuments(filter)
}

// snapshotOrder returns a copy of the overlay keys in natural (first-insert)
// order, for a scan that must not hold c.mu while it inspects each chain. The
// caller holds c.mu.
func (c *Collection) snapshotOrder() []string {
	out := make([]string, len(c.order))
	copy(out, c.order)
	return out
}
