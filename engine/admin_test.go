package engine

import (
	"testing"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/catalog"
	"github.com/tamnd/doc/vfs"
)

func TestCollectionStatsCounts(t *testing.T) {
	e := newEngine(t, vfs.NewMemFS())
	c, err := e.CreateCollection("shop", "orders")
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	for i := int32(0); i < 25; i++ {
		if _, err := c.InsertOne(doc(i, "row")); err != nil {
			t.Fatalf("InsertOne: %v", err)
		}
	}
	if _, err := c.CreateIndex(idxModel("v")); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	cs, err := e.CollectionStats("shop", "orders")
	if err != nil {
		t.Fatalf("CollectionStats: %v", err)
	}
	if cs.DocumentCount != 25 {
		t.Fatalf("DocumentCount = %d, want 25", cs.DocumentCount)
	}
	if cs.StorageSize <= 0 || cs.IndexSize <= 0 {
		t.Fatalf("sizes not positive: storage=%d index=%d", cs.StorageSize, cs.IndexSize)
	}
	if len(cs.IndexSizes) != 2 {
		t.Fatalf("IndexSizes = %v, want 2 entries", cs.IndexSizes)
	}
}

func TestCollectionStatsMissing(t *testing.T) {
	e := newEngine(t, vfs.NewMemFS())
	if _, err := e.CollectionStats("shop", "ghost"); err == nil {
		t.Fatal("CollectionStats on a missing collection should error")
	}
}

func TestDatabaseStatsAggregate(t *testing.T) {
	e := newEngine(t, vfs.NewMemFS())
	for _, name := range []string{"a", "b"} {
		c, err := e.CreateCollection("shop", name)
		if err != nil {
			t.Fatalf("CreateCollection: %v", err)
		}
		for i := int32(0); i < 5; i++ {
			if _, err := c.InsertOne(doc(i, "x")); err != nil {
				t.Fatalf("InsertOne: %v", err)
			}
		}
	}
	ds := e.DatabaseStats("shop")
	if ds.Collections != 2 {
		t.Fatalf("Collections = %d, want 2", ds.Collections)
	}
	if ds.DocumentCount != 10 {
		t.Fatalf("DocumentCount = %d, want 10", ds.DocumentCount)
	}
	if ds.TotalSize != ds.StorageSize+ds.IndexSize {
		t.Fatalf("TotalSize = %d, want %d", ds.TotalSize, ds.StorageSize+ds.IndexSize)
	}
}

func TestCollModValidatorPersists(t *testing.T) {
	fs := vfs.NewMemFS()
	e := newEngine(t, fs)
	if _, err := e.CreateCollection("shop", "users"); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	lvl := catalog.ValidationStrict
	validator := bson.NewBuilder().
		AppendDocument("email", bson.NewBuilder().AppendBoolean("$exists", true).Build()).
		Build()
	if err := e.CollMod("shop", "users", CollModSpec{
		SetValidator:    true,
		Validator:       validator,
		ValidationLevel: &lvl,
	}); err != nil {
		t.Fatalf("CollMod: %v", err)
	}
	c := e.GetCollection("shop", "users")
	// A document without email now fails the installed validator.
	if _, err := c.InsertOne(doc(1, "x")); err == nil {
		t.Fatal("insert without email should fail the new validator")
	}

	// Reopen the file and confirm the validator survived the commit.
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	e2, err := Open(fs, "test.doc", Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = e2.Close() }()
	c2 := e2.GetCollection("shop", "users")
	if _, err := c2.InsertOne(doc(2, "x")); err == nil {
		t.Fatal("validator should persist across reopen")
	}
}

func TestCollModIndexTTL(t *testing.T) {
	e := newEngine(t, vfs.NewMemFS())
	c, err := e.CreateCollection("shop", "sessions")
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	if _, err := c.InsertOne(doc(1, "x")); err != nil {
		t.Fatalf("InsertOne: %v", err)
	}
	if _, err := c.CreateIndex(idxModel("created")); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	secs := int64(300)
	if err := e.CollMod("shop", "sessions", CollModSpec{
		IndexName:          "created_1",
		ExpireAfterSeconds: &secs,
	}); err != nil {
		t.Fatalf("CollMod: %v", err)
	}
	for _, info := range c.ListIndexes() {
		if info.Name == "created_1" && info.ExpireAfterSeconds != 300 {
			t.Fatalf("ExpireAfterSeconds = %d, want 300", info.ExpireAfterSeconds)
		}
	}
}

func TestCollModMissing(t *testing.T) {
	e := newEngine(t, vfs.NewMemFS())
	if err := e.CollMod("shop", "ghost", CollModSpec{}); err == nil {
		t.Fatal("CollMod on a missing collection should error")
	}
}
