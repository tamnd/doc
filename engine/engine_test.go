package engine

import (
	"errors"
	"testing"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/catalog"
	"github.com/tamnd/doc/collection"
	"github.com/tamnd/doc/sys"
	"github.com/tamnd/doc/vfs"
)

func idxModel(field string) collection.IndexModel {
	return collection.IndexModel{Key: []catalog.KeyPart{{Field: field}}}
}

func indexNames(t *testing.T, c *collection.Collection) []string {
	t.Helper()
	if c == nil {
		t.Fatal("collection is nil")
	}
	infos := c.ListIndexes()
	out := make([]string, len(infos))
	for i, info := range infos {
		out[i] = info.Name
	}
	return out
}

// fixedGen gives deterministic _id values so tests can assert exact bytes; the
// timestamp seeds the ObjectId prefix and the counter increments per call.
func newEngine(t *testing.T, fs vfs.FS) *Engine {
	t.Helper()
	e, err := Open(fs, "test.doc", Options{IDGen: &sys.FixedIDGenerator{Timestamp: 1}})
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	return e
}

func doc(id int32, v string) bson.Raw {
	return bson.NewBuilder().AppendInt32("_id", id).AppendString("v", v).Build()
}

func filterID(id int32) bson.Raw {
	return bson.NewBuilder().AppendInt32("_id", id).Build()
}

func mustValue(t *testing.T, d bson.Raw) string {
	t.Helper()
	if d == nil {
		t.Fatal("document is nil")
	}
	v, ok := d.Lookup("v")
	if !ok {
		t.Fatalf("document %v has no v field", d)
	}
	return v.StringValue()
}

func TestCreateInsertFind(t *testing.T) {
	e := newEngine(t, vfs.NewMemFS())
	defer e.Close()

	c, err := e.CreateCollection("shop", "orders")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := c.InsertOne(doc(1, "a")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := c.FindOne(filterID(1))
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if v := mustValue(t, got); v != "a" {
		t.Fatalf("got v=%q, want a", v)
	}
}

func TestCreateDuplicateNamespace(t *testing.T) {
	e := newEngine(t, vfs.NewMemFS())
	defer e.Close()

	if _, err := e.CreateCollection("db", "c"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := e.CreateCollection("db", "c"); !errors.Is(err, ErrNamespaceExists) {
		t.Fatalf("got %v, want ErrNamespaceExists", err)
	}
}

// TestReopenPersistsData is the critical M6-a check: a collection's _id index root
// is persisted in the master catalog, not the file header, so a file with several
// collections recovers every collection's data on reopen.
func TestReopenPersistsData(t *testing.T) {
	fs := vfs.NewMemFS()
	e := newEngine(t, fs)
	for _, ns := range []struct{ db, c string }{{"a", "x"}, {"a", "y"}, {"b", "z"}} {
		c, err := e.CreateCollection(ns.db, ns.c)
		if err != nil {
			t.Fatalf("create %s.%s: %v", ns.db, ns.c, err)
		}
		for i := int32(1); i <= 5; i++ {
			if _, err := c.InsertOne(doc(i, ns.db+ns.c)); err != nil {
				t.Fatalf("insert: %v", err)
			}
		}
	}
	if err := e.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	e2 := newEngine(t, fs)
	defer e2.Close()
	for _, ns := range []struct{ db, c string }{{"a", "x"}, {"a", "y"}, {"b", "z"}} {
		c := e2.GetCollection(ns.db, ns.c)
		if c == nil {
			t.Fatalf("collection %s.%s missing after reopen", ns.db, ns.c)
		}
		n, err := c.CountDocuments(nil)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 5 {
			t.Fatalf("%s.%s has %d docs, want 5", ns.db, ns.c, n)
		}
		got, err := c.FindOne(filterID(3))
		if err != nil {
			t.Fatalf("find: %v", err)
		}
		if v := mustValue(t, got); v != ns.db+ns.c {
			t.Fatalf("%s.%s _id=3 has v=%q, want %q", ns.db, ns.c, v, ns.db+ns.c)
		}
	}
}

// TestCollectionsAreIsolated checks that two collections in one file do not see each
// other's documents even when they share _id values.
func TestCollectionsAreIsolated(t *testing.T) {
	e := newEngine(t, vfs.NewMemFS())
	defer e.Close()

	a, _ := e.CreateCollection("db", "a")
	b, _ := e.CreateCollection("db", "b")
	if _, err := a.InsertOne(doc(1, "from-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.InsertOne(doc(1, "from-b")); err != nil {
		t.Fatal(err)
	}
	av, _ := a.FindOne(filterID(1))
	bv, _ := b.FindOne(filterID(1))
	if mustValue(t, av) != "from-a" || mustValue(t, bv) != "from-b" {
		t.Fatalf("cross-collection bleed: a=%q b=%q", mustValue(t, av), mustValue(t, bv))
	}
}

func TestListDatabasesAndCollections(t *testing.T) {
	e := newEngine(t, vfs.NewMemFS())
	defer e.Close()

	e.CreateCollection("alpha", "one")
	e.CreateCollection("alpha", "two")
	e.CreateCollection("beta", "three")

	dbs := e.ListDatabases()
	if len(dbs) != 2 || dbs[0] != "alpha" || dbs[1] != "beta" {
		t.Fatalf("databases = %v, want [alpha beta]", dbs)
	}
	colls := e.ListCollections("alpha")
	if len(colls) != 2 || colls[0] != "one" || colls[1] != "two" {
		t.Fatalf("alpha collections = %v, want [one two]", colls)
	}
}

func TestEnsureCollectionImplicitCreate(t *testing.T) {
	e := newEngine(t, vfs.NewMemFS())
	defer e.Close()

	c, err := e.EnsureCollection("db", "c")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if _, err := c.InsertOne(doc(1, "x")); err != nil {
		t.Fatal(err)
	}
	// A second ensure returns the same live handle, not a fresh empty one.
	c2, _ := e.EnsureCollection("db", "c")
	n, _ := c2.CountDocuments(nil)
	if n != 1 {
		t.Fatalf("ensure returned a fresh handle: count=%d, want 1", n)
	}
}

func TestDropCollectionAndDatabase(t *testing.T) {
	fs := vfs.NewMemFS()
	e := newEngine(t, fs)

	a, _ := e.CreateCollection("db", "a")
	a.InsertOne(doc(1, "x"))
	e.CreateCollection("db", "b")

	if err := e.DropCollection("db", "a"); err != nil {
		t.Fatalf("drop collection: %v", err)
	}
	if e.GetCollection("db", "a") != nil {
		t.Fatal("dropped collection still resolves")
	}
	if len(e.ListCollections("db")) != 1 {
		t.Fatalf("after drop, collections = %v", e.ListCollections("db"))
	}
	// Database still exists because b remains.
	if len(e.ListDatabases()) != 1 {
		t.Fatalf("database vanished while a collection remains")
	}
	// Dropping the last collection removes the database record.
	e.DropCollection("db", "b")
	if len(e.ListDatabases()) != 0 {
		t.Fatalf("database survived its last collection drop: %v", e.ListDatabases())
	}

	e.Close()
	e2 := newEngine(t, fs)
	defer e2.Close()
	if len(e2.ListCollections("db")) != 0 {
		t.Fatalf("dropped collections came back after reopen: %v", e2.ListCollections("db"))
	}
}

// TestSharedOracleAcrossCollections checks that every collection in the file commits
// through one oracle: a commit on one collection advances the version a snapshot on
// another collection then observes, which is what makes a cross-collection
// transaction coherent (spec 2061 doc 06 §7, doc 09 §2).
func TestSharedOracleAcrossCollections(t *testing.T) {
	e := newEngine(t, vfs.NewMemFS())
	defer e.Close()

	a, _ := e.CreateCollection("db", "a")
	b, _ := e.CreateCollection("db", "b")

	// A write that commits on a advances the global version.
	t1 := a.BeginTx(collection.TransactionOptions{})
	if _, err := t1.InsertOne(doc(1, "x")); err != nil {
		t.Fatal(err)
	}
	if err := t1.Commit(); err != nil {
		t.Fatalf("commit on a: %v", err)
	}
	av := t1.CommitVersion()
	if av == 0 {
		t.Fatal("commit on a produced version 0")
	}

	// A snapshot opened on b afterward reads at or above a's commit version, which
	// can only hold if a and b share the oracle.
	t2 := b.BeginReadOnly()
	defer t2.Rollback()
	if t2.SnapshotVersion() < av {
		t.Fatalf("snapshot on b is %d, below a's commit %d: oracles are not shared",
			t2.SnapshotVersion(), av)
	}
}

// TestSecondaryIndexesPerCollection checks each collection keeps its own index
// catalog: an index on one collection is invisible to another, and both survive a
// reopen.
func TestSecondaryIndexesPerCollection(t *testing.T) {
	fs := vfs.NewMemFS()
	e := newEngine(t, fs)

	a, _ := e.CreateCollection("db", "a")
	b, _ := e.CreateCollection("db", "b")
	if _, err := a.CreateIndex(idxModel("v")); err != nil {
		t.Fatalf("create index on a: %v", err)
	}
	a.InsertOne(doc(1, "hello"))
	b.InsertOne(doc(1, "hello"))

	if names := indexNames(t, a); len(names) != 2 {
		t.Fatalf("collection a has indexes %v, want _id_ and v_1", names)
	}
	if names := indexNames(t, b); len(names) != 1 {
		t.Fatalf("collection b has indexes %v, want only _id_", names)
	}

	e.Close()
	e2 := newEngine(t, fs)
	defer e2.Close()
	if names := indexNames(t, e2.GetCollection("db", "a")); len(names) != 2 {
		t.Fatalf("after reopen collection a has indexes %v, want 2", names)
	}
}
