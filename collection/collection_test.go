package collection

import (
	"errors"
	"testing"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/storage"
	"github.com/tamnd/doc/sys"
	"github.com/tamnd/doc/vfs"
)

// newTestColl opens an in-memory collection with a deterministic id generator so
// auto-minted _ids are predictable across the reference and the subject.
func newTestColl(t *testing.T) *Collection {
	t.Helper()
	fs := vfs.NewMemFS()
	c, err := Open(fs, "test.doc", Options{IDGen: &sys.FixedIDGenerator{Timestamp: 1}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// docInt builds {_id: id, n: v}.
func docInt(id int32, field string, v int32) bson.Raw {
	b := bson.NewBuilder()
	b.AppendInt32("_id", id)
	b.AppendInt32(field, v)
	return b.Build()
}

func docID(id int32) bson.Raw {
	b := bson.NewBuilder()
	b.AppendInt32("_id", id)
	return b.Build()
}

func filterID(id int32) bson.Raw {
	b := bson.NewBuilder()
	b.AppendInt32("_id", id)
	return b.Build()
}

func TestInsertAndFindByID(t *testing.T) {
	c := newTestColl(t)
	if _, err := c.InsertOne(docInt(1, "n", 10)); err != nil {
		t.Fatalf("InsertOne: %v", err)
	}
	got, err := c.FindOne(filterID(1))
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	if got == nil {
		t.Fatal("FindOne returned nil for an inserted document")
	}
	v, ok := got.Lookup("n")
	if !ok || v.Int32() != 10 {
		t.Fatalf("field n: got %+v ok=%v, want 10", v, ok)
	}
}

func TestFindMissingReturnsNil(t *testing.T) {
	c := newTestColl(t)
	got, err := c.FindOne(filterID(99))
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	if got != nil {
		t.Fatalf("FindOne on empty collection: got %v, want nil", got)
	}
}

func TestDuplicateIDRejected(t *testing.T) {
	c := newTestColl(t)
	if _, err := c.InsertOne(docID(1)); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_, err := c.InsertOne(docID(1))
	if !errors.Is(err, storage.ErrDuplicateKey) {
		t.Fatalf("duplicate _id: got %v, want ErrDuplicateKey", err)
	}
}

func TestAutoMintedID(t *testing.T) {
	c := newTestColl(t)
	d := bson.NewBuilder().AppendString("name", "x").Build()
	id, err := c.InsertOne(d)
	if err != nil {
		t.Fatalf("InsertOne: %v", err)
	}
	if id.Type != bson.TypeObjectID {
		t.Fatalf("minted _id type: got %v, want ObjectID", id.Type)
	}
	n, err := c.CountDocuments(nil)
	if err != nil {
		t.Fatalf("CountDocuments: %v", err)
	}
	if n != 1 {
		t.Fatalf("count after one auto-id insert: got %d, want 1", n)
	}
}

func TestDeleteOne(t *testing.T) {
	c := newTestColl(t)
	for i := int32(1); i <= 3; i++ {
		if _, err := c.InsertOne(docID(i)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	n, err := c.DeleteOne(filterID(2))
	if err != nil {
		t.Fatalf("DeleteOne: %v", err)
	}
	if n != 1 {
		t.Fatalf("DeleteOne returned %d, want 1", n)
	}
	got, err := c.FindOne(filterID(2))
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	if got != nil {
		t.Fatal("deleted document is still visible")
	}
	count, _ := c.CountDocuments(nil)
	if count != 2 {
		t.Fatalf("count after delete: got %d, want 2", count)
	}
}

func TestDeleteMissing(t *testing.T) {
	c := newTestColl(t)
	n, err := c.DeleteOne(filterID(7))
	if err != nil {
		t.Fatalf("DeleteOne: %v", err)
	}
	if n != 0 {
		t.Fatalf("DeleteOne on missing: got %d, want 0", n)
	}
}

func TestReinsertAfterDelete(t *testing.T) {
	c := newTestColl(t)
	if _, err := c.InsertOne(docInt(1, "n", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteOne(filterID(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.InsertOne(docInt(1, "n", 2)); err != nil {
		t.Fatalf("reinsert after delete: %v", err)
	}
	got, err := c.FindOne(filterID(1))
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("reinserted document not found")
	}
	if v, _ := got.Lookup("n"); v.Int32() != 2 {
		t.Fatalf("reinserted n: got %d, want 2", v.Int32())
	}
}

func TestCountWithFilter(t *testing.T) {
	c := newTestColl(t)
	if _, err := c.InsertOne(docInt(1, "grp", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.InsertOne(docInt(2, "grp", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.InsertOne(docInt(3, "grp", 2)); err != nil {
		t.Fatal(err)
	}
	f := bson.NewBuilder().AppendInt32("grp", 1).Build()
	n, err := c.CountDocuments(f)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("count grp=1: got %d, want 2", n)
	}
}

func TestFindOneEmptyFilterNaturalOrder(t *testing.T) {
	c := newTestColl(t)
	for _, id := range []int32{5, 3, 9} {
		if _, err := c.InsertOne(docID(id)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := c.FindOne(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("FindOne(nil) on non-empty collection returned nil")
	}
	if v, _ := got.Lookup("_id"); v.Int32() != 5 {
		t.Fatalf("natural-order first _id: got %d, want 5", v.Int32())
	}
}

// TestSnapshotIsolation verifies a read transaction keeps seeing the document it
// began with after a concurrent transaction deletes and the writer commits.
func TestSnapshotIsolation(t *testing.T) {
	c := newTestColl(t)
	if _, err := c.InsertOne(docInt(1, "n", 100)); err != nil {
		t.Fatal(err)
	}

	reader := c.BeginReadOnly()
	defer reader.Rollback()

	// A separate writer deletes the document and commits.
	if _, err := c.DeleteOne(filterID(1)); err != nil {
		t.Fatal(err)
	}

	// The reader's snapshot predates the delete, so it still sees the document.
	got, err := reader.FindOne(filterID(1))
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("snapshot isolation broken: reader lost a document a later txn deleted")
	}
	if v, _ := got.Lookup("n"); v.Int32() != 100 {
		t.Fatalf("reader sees n=%d, want 100", v.Int32())
	}

	// A fresh read sees the delete.
	fresh, _ := c.FindOne(filterID(1))
	if fresh != nil {
		t.Fatal("a fresh read should not see the deleted document")
	}
}

// TestWriteConflict verifies first-committer-wins: two transactions that both
// write the same _id cannot both commit.
func TestWriteConflict(t *testing.T) {
	c := newTestColl(t)

	t1 := c.Begin()
	t2 := c.Begin()
	if _, err := t1.InsertOne(docInt(1, "w", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := t2.InsertOne(docInt(1, "w", 2)); err != nil {
		t.Fatal(err)
	}
	if err := t1.Commit(); err != nil {
		t.Fatalf("t1 commit: %v", err)
	}
	err := t2.Commit()
	if !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("t2 commit: got %v, want ErrConflict", err)
	}

	// The winner's value survives.
	got, _ := c.FindOne(filterID(1))
	if v, _ := got.Lookup("w"); v.Int32() != 1 {
		t.Fatalf("winner value: got %d, want 1", v.Int32())
	}
}

// TestDurabilityAcrossReopen verifies committed documents survive a close and
// reopen of the same file.
func TestDurabilityAcrossReopen(t *testing.T) {
	fs := vfs.NewMemFS()
	c, err := Open(fs, "d.doc", Options{IDGen: &sys.FixedIDGenerator{Timestamp: 1}})
	if err != nil {
		t.Fatal(err)
	}
	for i := int32(1); i <= 5; i++ {
		if _, err := c.InsertOne(docInt(i, "n", i*10)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.DeleteOne(filterID(3)); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	c2, err := Open(fs, "d.doc", Options{IDGen: &sys.FixedIDGenerator{Timestamp: 1}})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer c2.Close()

	n, _ := c2.CountDocuments(nil)
	if n != 4 {
		t.Fatalf("count after reopen: got %d, want 4", n)
	}
	if got, _ := c2.FindOne(filterID(3)); got != nil {
		t.Fatal("deleted document reappeared after reopen")
	}
	got, _ := c2.FindOne(filterID(4))
	if got == nil {
		t.Fatal("surviving document missing after reopen")
	}
	if v, _ := got.Lookup("n"); v.Int32() != 40 {
		t.Fatalf("reopened n: got %d, want 40", v.Int32())
	}

	// New inserts after reopen get versions above the recovered maximum and do
	// not collide.
	if _, err := c2.InsertOne(docID(6)); err != nil {
		t.Fatalf("insert after reopen: %v", err)
	}
}

func TestRollbackDiscardsWrites(t *testing.T) {
	c := newTestColl(t)
	tx := c.Begin()
	if _, err := tx.InsertOne(docID(1)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	n, _ := c.CountDocuments(nil)
	if n != 0 {
		t.Fatalf("count after rollback: got %d, want 0", n)
	}
}

func TestReadYourOwnWrites(t *testing.T) {
	c := newTestColl(t)
	tx := c.Begin()
	if _, err := tx.InsertOne(docInt(1, "n", 7)); err != nil {
		t.Fatal(err)
	}
	got, err := tx.FindOne(filterID(1))
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("a transaction cannot see its own uncommitted insert")
	}
	// Another transaction cannot.
	other := c.BeginReadOnly()
	defer other.Rollback()
	if o, _ := other.FindOne(filterID(1)); o != nil {
		t.Fatal("an uncommitted insert leaked to another transaction")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestReadOnlyTxnRejectsWrites(t *testing.T) {
	c := newTestColl(t)
	tx := c.BeginReadOnly()
	defer tx.Rollback()
	if _, err := tx.InsertOne(docID(1)); !errors.Is(err, storage.ErrReadOnly) {
		t.Fatalf("read-only insert: got %v, want ErrReadOnly", err)
	}
}
