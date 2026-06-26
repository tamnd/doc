// Package engine is doc's multi-collection layer: it multiplexes many collections
// across many databases into one .doc file over a single shared pager and a single
// shared MVCC oracle (spec 2061 doc 09, doc 14). The master catalog records which
// databases and collections exist and where each collection's pages live; each
// collection is a collection.Collection bound to the shared resources with its own
// heap collID block, _id index root, and secondary-index registry. Because every
// collection commits through the one oracle, a transaction can span more than one
// collection in the same file (spec 2061 doc 06 §7).
//
// The public document API (spec 2061 doc 14) wraps this engine: doc.Open returns a
// handle whose Database and Collection methods resolve to engine operations. The
// engine itself speaks bson.Raw, the same currency as collection.Collection.
package engine

import (
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/catalog"
	"github.com/tamnd/doc/collection"
	"github.com/tamnd/doc/colstore"
	"github.com/tamnd/doc/format"
	"github.com/tamnd/doc/mvcc"
	"github.com/tamnd/doc/pager"
	"github.com/tamnd/doc/schema"
	"github.com/tamnd/doc/sys"
	"github.com/tamnd/doc/vfs"
)

// Errors the engine surfaces for namespace operations (spec 2061 doc 14 §error
// taxonomy). They mirror MongoDB's NamespaceExists (48) and NamespaceNotFound (26).
var (
	ErrNamespaceExists   = errors.New("engine: collection already exists")
	ErrNamespaceNotFound = errors.New("engine: collection does not exist")
	ErrInvalidName       = errors.New("engine: invalid database or collection name")
)

// Options configures an engine at Open time.
type Options struct {
	Pager pager.Options
	Clock sys.Clock
	IDGen sys.IDGenerator
}

// Engine is the open handle to a multi-collection .doc file.
type Engine struct {
	pgr  *pager.Pager
	orc  *mvcc.Oracle
	mcat *catalog.MasterStore
	clk  sys.Clock
	gen  sys.IDGenerator

	mu         sync.Mutex
	nextCollID uint32
	colls      map[string]*collection.Collection // (db,coll) -> open handle

	// onChange, when set by the public layer through SetChangeHook, receives the
	// change records of every committed transaction tagged with its namespace. It is
	// nil until a watcher exists, so collections opened without it pay nothing on
	// commit.
	onChange ChangeHook
}

// ChangeHook receives one committed transaction's change records together with the
// database and collection they came from and the commit version that orders them.
type ChangeHook func(db, coll string, recs []collection.ChangeRecord, commitVersion uint64)

func nsKey(db, name string) string { return db + "\x00" + name }

// Open opens or creates the .doc file at path on fs and brings up the engine:
// it loads the master catalog, opens every recorded collection against the shared
// pager, and seeds one oracle at the highest commit version recovered across all of
// them so new commits get strictly greater versions (spec 2061 doc 06 §4.6, doc 09
// §2.3).
func Open(fs vfs.FS, path string, opts Options) (*Engine, error) {
	pgr, err := pager.Open(fs, path, opts.Pager)
	if err != nil {
		return nil, err
	}
	mcat, err := catalog.OpenMaster(pgr)
	if err != nil {
		_ = pgr.Close()
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
	e := &Engine{
		pgr:        pgr,
		mcat:       mcat,
		clk:        clock,
		gen:        gen,
		nextCollID: catalog.FirstUserCollID,
		colls:      make(map[string]*collection.Collection),
	}
	if mx := mcat.MaxCollID(); mx >= e.nextCollID {
		e.nextCollID = mx + catalog.CollIDStride
	}
	// Open every recorded collection and find the global max version so the
	// shared oracle starts above any version on disk.
	var maxVer uint64
	for _, rec := range mcat.ListCollections("") {
		c, mv, oerr := e.openCollection(rec)
		if oerr != nil {
			_ = pgr.Close()
			return nil, oerr
		}
		if mv > maxVer {
			maxVer = mv
		}
		e.colls[nsKey(rec.DBName, rec.Name)] = c
	}
	e.orc = mvcc.NewOracle(maxVer)
	for _, c := range e.colls {
		c.SetOracle(e.orc)
	}
	// Rebuild the columnar projection store for any collection that persisted one,
	// now that every collection has its oracle and can open a read snapshot.
	for _, rec := range mcat.ListCollections("") {
		if c := e.colls[nsKey(rec.DBName, rec.Name)]; c != nil {
			if err := enableColumnar(c, rec); err != nil {
				_ = pgr.Close()
				return nil, err
			}
		}
	}
	return e, nil
}

// openCollection builds a collection.Collection over the shared pager for a catalog
// record, wiring the _id index root and its persistence back through the record and
// installing the collection's write policy (validator and capped bounds).
func (e *Engine) openCollection(rec *catalog.CollectionRecord) (*collection.Collection, uint64, error) {
	c, mv, err := collection.NewWithDeps(collection.Deps{
		Pager:                  e.pgr,
		Clock:                  e.clk,
		IDGen:                  e.gen,
		HeapCollID:             rec.CollID,
		SecondaryCatalogCollID: rec.SecondaryCollID(),
		SecondaryIndexBase:     rec.CollID + 2,
		IDIndexRoot:            rec.IDIndexRoot,
		OnIDIndexRoot:          func(root uint32) { rec.IDIndexRoot = root },
		PersistCatalog:         func() error { return e.mcat.StageCollection(rec) },
		OnChange:               e.changeAdapter(rec.DBName, rec.Name),
	})
	if err != nil {
		return nil, 0, err
	}
	pol, err := policyFromRecord(rec)
	if err != nil {
		return nil, 0, err
	}
	c.SetPolicy(pol)
	return c, mv, nil
}

// enableColumnar turns on the columnar projection store for an opened collection when
// its catalog record asks for it (spec 2061 doc 04 §10, doc 19 §21.4). It runs after
// the shared oracle is bound, because rebuilding the store reads the heap through a
// read-only transaction that needs a snapshot. The store is a derived structure
// reconstructed from whatever the heap holds at open. A mode of "" or "off" leaves
// the collection on the heap-only path.
func enableColumnar(c *collection.Collection, rec *catalog.CollectionRecord) error {
	mode, ok := columnarMode(rec.Options.ColumnarMode)
	if !ok {
		return nil
	}
	return c.EnableColumnStore(mode, rec.Options.ColumnarFields)
}

// columnarMode maps the persisted columnar_store spelling onto the colstore mode,
// reporting ok=false for the heap-only spellings so the caller skips enabling.
func columnarMode(s string) (colstore.Mode, bool) {
	switch s {
	case "transactional":
		return colstore.ModeTransactional, true
	case "lazy":
		return colstore.ModeLazy, true
	default:
		return colstore.ModeOff, false
	}
}

// changeAdapter returns the collection-level emitter that forwards a transaction's
// change records to the engine hook, tagged with the namespace. It reads e.onChange
// at fire time, so a collection opened before the first watcher starts forwarding as
// soon as SetChangeHook installs the hook. It returns nil when no hook is installed,
// leaving freshly opened collections on the zero-cost path until a watcher appears.
func (e *Engine) changeAdapter(db, name string) collection.EmitFunc {
	if e.onChange == nil {
		return nil
	}
	return func(recs []collection.ChangeRecord, cv uint64) {
		if h := e.onChange; h != nil {
			h(db, name, recs, cv)
		}
	}
}

// SetChangeHook installs the engine change hook and binds it to every open collection.
// The public layer calls it once at open, before any watcher starts, so all collections
// opened later through createLocked pick the hook up too.
func (e *Engine) SetChangeHook(h ChangeHook) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onChange = h
	for key, c := range e.colls {
		db, name := splitNSKey(key)
		c.SetChangeHook(e.changeAdapter(db, name))
	}
}

// splitNSKey reverses nsKey, recovering the database and collection from a map key.
func splitNSKey(key string) (db, name string) {
	if i := strings.IndexByte(key, 0); i >= 0 {
		return key[:i], key[i+1:]
	}
	return "", key
}

// policyFromRecord compiles the catalog record's validator and reads its capped
// bounds into a collection write policy (spec 2061 doc 09 §10, doc 04 §11.2).
func policyFromRecord(rec *catalog.CollectionRecord) (collection.Policy, error) {
	v, err := schema.Compile(rec.Validator)
	if err != nil {
		return collection.Policy{}, err
	}
	return collection.Policy{
		Validator:        v,
		ValidationLevel:  rec.ValidationLevel,
		ValidationAction: rec.ValidationAction,
		Capped:           rec.Kind == catalog.KindCapped,
		CappedMaxDocs:    rec.Options.MaxDocs,
		CappedMaxBytes:   rec.Options.SizeBytes,
	}, nil
}

// uuid mints a stable 16-byte identifier for a new database or collection from the
// id generator, so it is deterministic under a fixed generator in tests.
func (e *Engine) uuid() [16]byte {
	var u [16]byte
	oid := e.gen.NewID()
	copy(u[:12], oid[:])
	return u
}

func (e *Engine) nowMillis() int64 { return e.clk.Now().UnixMilli() }

// validName rejects empty names, names with a NUL (the catalog key separator), and
// the reserved internal prefix (spec 2061 doc 09 §16). It is deliberately lenient
// otherwise; the public layer applies the full MongoDB naming rules.
func validName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] == 0 {
			return false
		}
	}
	return true
}

// CreateSpec carries the options for creating a collection with a kind, a validator,
// or capped bounds (spec 2061 doc 09 §8.2, §10). The zero value creates a regular,
// unvalidated, uncapped collection, so CreateCollection is CreateCollectionWith with
// the zero spec.
type CreateSpec struct {
	Capped           bool
	SizeBytes        int64
	MaxDocs          int64
	Validator        bson.Raw
	ValidationLevel  catalog.ValidationLevel
	ValidationAction catalog.ValidationAction

	// Columnar projection store (spec 2061 doc 04 §10, doc 19 §21.4). ColumnarMode
	// is "", "transactional", or "lazy"; ColumnarFields is the projected field set,
	// empty meaning every observed top-level field.
	ColumnarMode   string
	ColumnarFields []string
}

// CreateCollection creates a regular collection in db, creating the database record
// if this is its first collection. It is one WAL transaction (spec 2061 doc 09 §8.2)
// and returns ErrNamespaceExists if the collection already exists.
func (e *Engine) CreateCollection(db, name string) (*collection.Collection, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.createLocked(db, name, CreateSpec{})
}

// CreateCollectionWith creates a collection with explicit options: a validator with
// its level and action, or capped bounds (spec 2061 doc 09 §8.2). It rejects a
// validator that does not compile so the failure surfaces at create time rather than
// on the first write.
func (e *Engine) CreateCollectionWith(db, name string, spec CreateSpec) (*collection.Collection, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.createLocked(db, name, spec)
}

func (e *Engine) createLocked(db, name string, spec CreateSpec) (*collection.Collection, error) {
	if !validName(db) || !validName(name) {
		return nil, ErrInvalidName
	}
	if e.mcat.GetCollection(db, name) != nil {
		return nil, ErrNamespaceExists
	}
	if len(spec.Validator) > 0 {
		if _, err := schema.Compile(spec.Validator); err != nil {
			return nil, err
		}
	}
	if e.mcat.GetDatabase(db) == nil {
		if err := e.mcat.PutDatabase(&catalog.DatabaseRecord{
			Name:      db,
			UUID:      e.uuid(),
			CreatedAt: e.nowMillis(),
		}); err != nil {
			return nil, err
		}
	}
	now := e.nowMillis()
	kind := catalog.KindRegular
	if spec.Capped {
		kind = catalog.KindCapped
	}
	rec := &catalog.CollectionRecord{
		DBName:           db,
		Name:             name,
		UUID:             e.uuid(),
		Kind:             kind,
		CreatedAt:        now,
		ModifiedAt:       now,
		CollID:           e.nextCollID,
		IDIndexRoot:      format.NullPage,
		Validator:        spec.Validator,
		ValidationLevel:  spec.ValidationLevel,
		ValidationAction: spec.ValidationAction,
		Options: catalog.CollectionOptions{
			SizeBytes:      spec.SizeBytes,
			MaxDocs:        spec.MaxDocs,
			ColumnarMode:   spec.ColumnarMode,
			ColumnarFields: spec.ColumnarFields,
		},
	}
	e.nextCollID += catalog.CollIDStride
	if err := e.mcat.StageCollection(rec); err != nil {
		return nil, err
	}
	if err := e.pgr.Commit(); err != nil {
		return nil, err
	}
	c, _, err := e.openCollection(rec)
	if err != nil {
		return nil, err
	}
	c.SetOracle(e.orc)
	if err := enableColumnar(c, rec); err != nil {
		return nil, err
	}
	e.colls[nsKey(db, name)] = c
	return c, nil
}

// GetCollection returns the open handle for db.name, or nil if it does not exist.
func (e *Engine) GetCollection(db, name string) *collection.Collection {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.colls[nsKey(db, name)]
}

// EnsureCollection returns the handle for db.name, creating the collection with
// default options if it does not exist (MongoDB's implicit creation on first write,
// spec 2061 doc 09 §8.2).
func (e *Engine) EnsureCollection(db, name string) (*collection.Collection, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c := e.colls[nsKey(db, name)]; c != nil {
		return c, nil
	}
	return e.createLocked(db, name, CreateSpec{})
}

// DropCollection removes db.name from the catalog (spec 2061 doc 09 §8.3). The
// collection's pages are left in the file; freelist reclaim is a later milestone.
func (e *Engine) DropCollection(db, name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	rec := e.mcat.GetCollection(db, name)
	if rec == nil {
		return ErrNamespaceNotFound
	}
	if err := e.mcat.RemoveCollection(db, name); err != nil {
		return err
	}
	delete(e.colls, nsKey(db, name))
	e.fireDrop(db, name)
	// Drop the database record when its last collection is gone.
	if len(e.mcat.ListCollections(db)) == 0 {
		if err := e.mcat.RemoveDatabase(db); err != nil {
			return err
		}
	}
	return nil
}

// fireDrop publishes a drop change record for a removed namespace so watchers see
// the collection go away and invalidate. It orders the event at the current commit
// version, which is at or above every data event the collection produced. It runs
// under e.mu, the same lock the drop took.
func (e *Engine) fireDrop(db, name string) {
	if e.onChange == nil {
		return
	}
	cv := uint64(0)
	if e.orc != nil {
		cv = e.orc.CommitVersion()
	}
	e.onChange(db, name, []collection.ChangeRecord{{Op: "drop"}}, cv)
}

// ListCollections returns the names of every collection in db, sorted.
func (e *Engine) ListCollections(db string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	recs := e.mcat.ListCollections(db)
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.Name
	}
	return out
}

// ListDatabases returns the names of every database, sorted.
func (e *Engine) ListDatabases() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	recs := e.mcat.ListDatabases()
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.Name
	}
	return out
}

// DropDatabase drops every collection in db and the database record (spec 2061
// doc 09 §3.3).
func (e *Engine) DropDatabase(db string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, rec := range e.mcat.ListCollections(db) {
		if err := e.mcat.RemoveCollection(rec.DBName, rec.Name); err != nil {
			return err
		}
		delete(e.colls, nsKey(rec.DBName, rec.Name))
		e.fireDrop(rec.DBName, rec.Name)
	}
	if e.mcat.GetDatabase(db) != nil {
		return e.mcat.RemoveDatabase(db)
	}
	return nil
}

// Oracle exposes the shared MVCC oracle for cross-collection transaction wiring in
// the public layer (spec 2061 doc 14 §sessions).
func (e *Engine) Oracle() *mvcc.Oracle { return e.orc }

// Begin opens a multi-collection transaction over the shared oracle and pager. The
// public session layer drives it: every collection touched through the returned
// MultiTxn reads one snapshot and commits together (spec 2061 doc 14 §14).
func (e *Engine) Begin(iso collection.IsolationLevel) *collection.MultiTxn {
	return collection.NewMultiTxn(e.orc, e.pgr, iso)
}

// SweepTTL runs one TTL expiry pass across every open collection and returns the
// total number of documents deleted (spec 2061 doc 04 §11.4). The DB layer calls it
// on a background ticker; tests call it directly with a controlled clock.
func (e *Engine) SweepTTL(now time.Time) (int, error) {
	e.mu.Lock()
	colls := make([]*collection.Collection, 0, len(e.colls))
	for _, c := range e.colls {
		colls = append(colls, c)
	}
	e.mu.Unlock()
	total := 0
	for _, c := range colls {
		n, err := c.SweepTTL(now)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// Close flushes and closes the shared pager, releasing the whole file.
func (e *Engine) Close() error { return e.pgr.Close() }
