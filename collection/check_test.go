package collection

import (
	"strings"
	"testing"

	"github.com/tamnd/doc/catalog"
	"github.com/tamnd/doc/storage"
)

// TestCollectionCheckClean inserts documents and a secondary index, then confirms
// the collection check finds the heap, the _id index, and the secondary index all
// consistent, with the document and entry counts it expects.
func TestCollectionCheckClean(t *testing.T) {
	c := newTestColl(t)
	for i := int32(1); i <= 50; i++ {
		if _, err := c.InsertOne(docInt(i, "age", i%7)); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if _, err := c.CreateIndex(IndexModel{Key: []catalog.KeyPart{{Field: "age"}}}); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	// Delete a few to leave dead slots and shrink both indexes in step.
	for _, id := range []int32{3, 9, 27} {
		if _, err := c.DeleteOne(filterID(id)); err != nil {
			t.Fatalf("delete: %v", err)
		}
	}

	rep := c.Check()
	if !rep.Valid {
		t.Fatalf("clean collection reported problems: heap=%v top=%v indexes=%+v",
			rep.HeapProblems, rep.Problems, rep.Indexes)
	}
	if rep.Documents != 47 {
		t.Fatalf("document count = %d, want 47", rep.Documents)
	}
	for _, ix := range rep.Indexes {
		if ix.Name == "age_1" && ix.Entries != 47 {
			t.Fatalf("age_1 entries = %d, want 47", ix.Entries)
		}
	}
}

// TestCollectionCheckDetectsStrayIndexEntry injects an index entry that points at a
// RID with no live document behind it, the shape of an index that drifted out of
// step with the heap. The check must flag the secondary index as invalid.
func TestCollectionCheckDetectsStrayIndexEntry(t *testing.T) {
	c := newTestColl(t)
	for i := int32(1); i <= 10; i++ {
		if _, err := c.InsertOne(docInt(i, "age", i)); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	name, err := c.CreateIndex(IndexModel{Key: []catalog.KeyPart{{Field: "age"}}})
	if err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	if rep := c.Check(); !rep.Valid {
		t.Fatalf("collection should start clean, got %+v", rep)
	}

	bt := c.sidx[name]
	tx := bt.Begin()
	bogus := storage.RID{PageNo: 999999, Slot: 0}
	if err := bt.Put(tx, storage.IndexKey("zzzzz"), bogus); err != nil {
		t.Fatalf("inject: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit inject: %v", err)
	}

	rep := c.Check()
	if rep.Valid {
		t.Fatal("a stray index entry should make the check fail")
	}
	var found bool
	for _, ix := range rep.Indexes {
		if ix.Name == name && !ix.Valid {
			found = true
			if !strings.Contains(strings.Join(ix.Problems, ";"), "no live document") {
				t.Fatalf("expected an unresolved-entry problem, got %v", ix.Problems)
			}
		}
	}
	if !found {
		t.Fatalf("secondary index was not flagged: %+v", rep.Indexes)
	}
}
