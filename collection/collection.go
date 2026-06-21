// Package collection is doc's document-collection layer: the M2 surface that
// turns the durable record store (heap), the unique _id index, and the MVCC
// oracle into ordered, snapshot-isolated InsertOne / FindOne / DeleteOne /
// CountDocuments operations over BSON documents (spec 2061 doc 04 §2, doc 06).
//
// A collection keeps an in-memory version overlay: the heap durably holds the
// latest committed version of every document, while the overlay holds, per _id,
// the newest-first chain of versions a live snapshot might still need to read.
// Reads are served from the overlay, so the durable heap is touched only on
// commit and on Open (to rebuild the overlay). M3-b adds the update, replace,
// findAndModify, and distinct write paths over this same overlay (see update.go);
// secondary-index find paths arrive in M3-c.
package collection

import (
	"errors"
	"sync"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/catalog"
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

// secondaryCollIDBase is the first collection id handed to a secondary index's
// B-tree pages. The document heap and _id index use collID (1), the catalog uses
// catalog.CatalogCollID (2), so secondary indexes start above both. The id is a
// page-ownership tag only: a secondary B-tree navigates from its catalog-recorded
// root, never by scanning pages by collID (spec 2061 doc 07 §5.1).
const secondaryCollIDBase = 16

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
	clk sys.Clock

	cat          *catalog.Store
	sidx         map[string]*index.BTree // secondary index name -> live B-tree
	catalogDirty bool                    // a secondary index root changed this commit

	// secondaryBase is the first page-ownership collID handed to a secondary
	// index B-tree; it defaults to secondaryCollIDBase for the single-collection
	// path and is set per collection by the multi-collection engine.
	secondaryBase uint32
	// idRootDirty is set when the _id index root changed this commit, so the
	// engine path can persist the new root into the master catalog. persistExtra,
	// when set, stages that external catalog write into the same pager commit.
	idRootDirty  bool
	persistExtra func() error

	mu       sync.Mutex
	byID     map[string]*chain      // overlay key (encoded _id) -> version chain
	order    []string               // overlay keys in first-insert order, for natural scan
	dirty    map[string]struct{}    // keys whose chain holds more than one version
	ridOwner map[storage.RID]string // latest committed RID -> overlay key, for index fetch
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
	clock := opts.Clock
	if clock == nil {
		clock = sys.SystemClock{}
	}
	gen := opts.IDGen
	if gen == nil {
		gen = sys.NewObjectIDGenerator(clock)
	}
	cat, err := catalog.OpenStore(pgr)
	if err != nil {
		return nil, err
	}
	c := &Collection{
		pgr:           pgr,
		hp:            hp,
		idx:           idx,
		gen:           gen,
		clk:           clock,
		cat:           cat,
		secondaryBase: secondaryCollIDBase,
		sidx:          make(map[string]*index.BTree),
		byID:          make(map[string]*chain),
		dirty:         make(map[string]struct{}),
		ridOwner:      make(map[storage.RID]string),
	}
	if err := c.openSecondaryIndexes(); err != nil {
		return nil, err
	}
	maxVer, err := c.rebuild()
	if err != nil {
		return nil, err
	}
	c.orc = mvcc.NewOracle(maxVer)
	return c, nil
}

// Deps carries the shared resources a collection binds to when the multi-collection
// engine multiplexes many collections in one file (spec 2061 doc 09 §2). The engine
// owns the pager and a single oracle shared across every collection, assigns each
// collection a distinct heap collID block, and persists the collection's _id index
// root and secondary-index catalog through the master catalog rather than the file
// header. The single-collection Open path keeps its own pager, oracle, and header
// catalog-root slot and does not use this.
type Deps struct {
	Pager *pager.Pager
	Clock sys.Clock
	IDGen sys.IDGenerator
	// HeapCollID tags this collection's document heap and _id index pages.
	HeapCollID uint32
	// SecondaryCatalogCollID is the heap id of this collection's secondary-index
	// registry; SecondaryIndexBase is the first id handed to a secondary B-tree.
	SecondaryCatalogCollID uint32
	SecondaryIndexBase     uint32
	// IDIndexRoot is the persisted _id index root, NullPage when none yet.
	IDIndexRoot uint32
	// OnIDIndexRoot records a changed _id index root onto the catalog record;
	// PersistCatalog stages that record into the pager commit at durable time.
	OnIDIndexRoot  func(uint32)
	PersistCatalog func() error
}

// NewWithDeps opens a collection bound to the engine's shared resources, returning
// the highest commit version recovered from its heap so the engine can seed the
// one shared oracle at the global maximum (spec 2061 doc 06 §4.6). The oracle is
// not set here; the engine calls SetOracle once it has opened every collection.
func NewWithDeps(d Deps) (*Collection, uint64, error) {
	hp, err := heap.Open(d.Pager, d.HeapCollID)
	if err != nil {
		return nil, 0, err
	}
	clock := d.Clock
	if clock == nil {
		clock = sys.SystemClock{}
	}
	gen := d.IDGen
	if gen == nil {
		gen = sys.NewObjectIDGenerator(clock)
	}
	cat, err := catalog.OpenStoreWithCollID(d.Pager, d.SecondaryCatalogCollID)
	if err != nil {
		return nil, 0, err
	}
	c := &Collection{
		pgr:           d.Pager,
		hp:            hp,
		gen:           gen,
		clk:           clock,
		cat:           cat,
		secondaryBase: d.SecondaryIndexBase,
		persistExtra:  d.PersistCatalog,
		sidx:          make(map[string]*index.BTree),
		byID:          make(map[string]*chain),
		dirty:         make(map[string]struct{}),
		ridOwner:      make(map[storage.RID]string),
	}
	onRoot := func(root uint32) {
		if d.OnIDIndexRoot != nil {
			d.OnIDIndexRoot(root)
		}
		c.idRootDirty = true
	}
	idx, err := index.OpenWithRoot(d.Pager, d.HeapCollID, true, d.IDIndexRoot, onRoot)
	if err != nil {
		return nil, 0, err
	}
	c.idx = idx
	if err := c.openSecondaryIndexes(); err != nil {
		return nil, 0, err
	}
	maxVer, err := c.rebuild()
	if err != nil {
		return nil, 0, err
	}
	return c, maxVer, nil
}

// SetOracle binds the collection to the engine's shared MVCC oracle. It must be
// called before any transaction begins on a collection opened with NewWithDeps.
func (c *Collection) SetOracle(o *mvcc.Oracle) { c.orc = o }

// CloseShared releases the collection's per-collection state without closing the
// shared pager, which the engine owns and closes once for the whole file.
func (c *Collection) CloseShared() {}

// openSecondaryIndexes opens a live B-tree for each spec the catalog recovered,
// rooted at the spec's persisted root page. The onRoot hook records a later root
// change (a first insert or a root split) back onto the spec so it is persisted on
// the next catalog stage.
func (c *Collection) openSecondaryIndexes() error {
	for i, sp := range c.cat.Specs() {
		bt, err := c.openIndexTree(c.secondaryBase+uint32(i), sp)
		if err != nil {
			return err
		}
		c.sidx[sp.Name] = bt
	}
	return nil
}

// openIndexTree opens one secondary B-tree for spec sp under the given page-
// ownership collID. The onRoot callback persists root changes through the spec.
func (c *Collection) openIndexTree(treeCollID uint32, sp *catalog.IndexSpec) (*index.BTree, error) {
	spec := sp
	return index.OpenWithRoot(c.pgr, treeCollID, spec.Unique, spec.Root, func(root uint32) {
		spec.Root = root
		c.catalogDirty = true
	})
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
		c.ridOwner[r.RID] = key
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

// BeginTx starts a read-write transaction under the given options. It is the
// session layer's entry point: the isolation level is carried on the transaction so
// commit chooses snapshot-isolation or serializable validation (spec 2061 doc 06
// §7.2, §10).
func (c *Collection) BeginTx(opts TransactionOptions) *Txn {
	startVer, txnID := c.orc.Acquire()
	if opts.Isolation == Serializable {
		c.orc.RegisterSSI(txnID, startVer)
	}
	return &Txn{c: c, startVer: startVer, txnID: txnID, writable: true, iso: opts.Isolation}
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

// Explain returns the planner's explain document for a find, in its own read-only
// transaction, at the given verbosity ("queryPlanner", "executionStats", or
// "allPlansExecution").
func (c *Collection) Explain(filter bson.Raw, opts FindOptions, verbosity string) (bson.Raw, error) {
	t := c.BeginReadOnly()
	defer func() { _ = t.Rollback() }()
	return t.Explain(filter, opts, verbosity)
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

// DeleteMany deletes every document matching filter, returning the number
// deleted, in its own transaction.
func (c *Collection) DeleteMany(filter bson.Raw) (int64, error) {
	t := c.Begin()
	n, err := t.DeleteMany(filter)
	if err != nil {
		_ = t.Rollback()
		return 0, err
	}
	if err := t.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// InsertMany inserts a batch of documents in one transaction. When some inserts
// fail, the successful ones are still committed and a *BulkWriteException carrying
// the partial result is returned (spec 2061 doc 13 §3.3).
func (c *Collection) InsertMany(docs []bson.Raw, ordered bool) (InsertManyResult, error) {
	t := c.Begin()
	res, bwErr := t.InsertMany(docs, ordered)
	if err := t.Commit(); err != nil {
		return res, err
	}
	return res, bwErr
}

// BulkWrite executes a mixed batch of write operations in one transaction. When
// some operations fail, the successful ones are still committed and a
// *BulkWriteException carrying the partial result is returned (spec 2061 doc 13
// §14).
func (c *Collection) BulkWrite(ops []BulkOp, ordered bool) (BulkWriteResult, error) {
	t := c.Begin()
	res, bwErr := t.BulkWrite(ops, ordered)
	if err := t.Commit(); err != nil {
		return res, err
	}
	return res, bwErr
}

// CountDocuments returns the number of documents matching filter.
func (c *Collection) CountDocuments(filter bson.Raw) (int64, error) {
	t := c.BeginReadOnly()
	defer func() { _ = t.Rollback() }()
	return t.CountDocuments(filter)
}

// UpdateOne applies an update-operator document to the first matching document in
// its own transaction.
func (c *Collection) UpdateOne(filter, updateDoc bson.Raw) (UpdateResult, error) {
	return c.inWrite(func(t *Txn) (UpdateResult, error) { return t.UpdateOne(filter, updateDoc) })
}

// UpdateOneWith is UpdateOne with options (notably Upsert) in its own transaction.
func (c *Collection) UpdateOneWith(filter, updateDoc bson.Raw, opts UpdateOptions) (UpdateResult, error) {
	return c.inWrite(func(t *Txn) (UpdateResult, error) { return t.UpdateOneWith(filter, updateDoc, opts) })
}

// UpdateMany applies an update-operator document to every matching document in
// its own transaction.
func (c *Collection) UpdateMany(filter, updateDoc bson.Raw) (UpdateResult, error) {
	return c.inWrite(func(t *Txn) (UpdateResult, error) { return t.UpdateMany(filter, updateDoc) })
}

// UpdateManyWith is UpdateMany with options (notably Upsert) in its own
// transaction.
func (c *Collection) UpdateManyWith(filter, updateDoc bson.Raw, opts UpdateOptions) (UpdateResult, error) {
	return c.inWrite(func(t *Txn) (UpdateResult, error) { return t.UpdateManyWith(filter, updateDoc, opts) })
}

// ReplaceOne replaces the first matching document in its own transaction.
func (c *Collection) ReplaceOne(filter, replacement bson.Raw) (UpdateResult, error) {
	return c.inWrite(func(t *Txn) (UpdateResult, error) { return t.ReplaceOne(filter, replacement) })
}

// ReplaceOneWith is ReplaceOne with options (notably Upsert) in its own
// transaction.
func (c *Collection) ReplaceOneWith(filter, replacement bson.Raw, opts UpdateOptions) (UpdateResult, error) {
	return c.inWrite(func(t *Txn) (UpdateResult, error) { return t.ReplaceOneWith(filter, replacement, opts) })
}

// FindOneAndUpdate updates the first matching document and returns the before or
// after version, in its own transaction.
func (c *Collection) FindOneAndUpdate(filter, updateDoc bson.Raw, opts FindModifyOptions) (bson.Raw, error) {
	return c.inWriteDoc(func(t *Txn) (bson.Raw, error) { return t.FindOneAndUpdate(filter, updateDoc, opts) })
}

// FindOneAndReplace replaces the first matching document and returns the before
// or after version, in its own transaction.
func (c *Collection) FindOneAndReplace(filter, replacement bson.Raw, opts FindModifyOptions) (bson.Raw, error) {
	return c.inWriteDoc(func(t *Txn) (bson.Raw, error) { return t.FindOneAndReplace(filter, replacement, opts) })
}

// FindOneAndDelete deletes the first matching document and returns it, in its own
// transaction.
func (c *Collection) FindOneAndDelete(filter bson.Raw, opts FindModifyOptions) (bson.Raw, error) {
	return c.inWriteDoc(func(t *Txn) (bson.Raw, error) { return t.FindOneAndDelete(filter, opts) })
}

// Distinct returns the distinct values of field across the documents matching
// filter, in its own read-only transaction.
func (c *Collection) Distinct(field string, filter bson.Raw) ([]bson.RawValue, error) {
	t := c.BeginReadOnly()
	defer func() { _ = t.Rollback() }()
	return t.Distinct(field, filter)
}

// inWrite runs fn in a write transaction, committing on success and rolling back
// on error, for the count-returning write wrappers.
func (c *Collection) inWrite(fn func(*Txn) (UpdateResult, error)) (UpdateResult, error) {
	t := c.Begin()
	res, err := fn(t)
	if err != nil {
		_ = t.Rollback()
		return UpdateResult{}, err
	}
	if err := t.Commit(); err != nil {
		return UpdateResult{}, err
	}
	return res, nil
}

// inWriteDoc runs fn in a write transaction for the document-returning
// findAndModify wrappers, committing on success and rolling back on error.
func (c *Collection) inWriteDoc(fn func(*Txn) (bson.Raw, error)) (bson.Raw, error) {
	t := c.Begin()
	doc, err := fn(t)
	if err != nil {
		_ = t.Rollback()
		return nil, err
	}
	if err := t.Commit(); err != nil {
		return nil, err
	}
	return doc, nil
}

// snapshotOrder returns a copy of the overlay keys in natural (first-insert)
// order, for a scan that must not hold c.mu while it inspects each chain. The
// caller holds c.mu.
func (c *Collection) snapshotOrder() []string {
	out := make([]string, len(c.order))
	copy(out, c.order)
	return out
}
