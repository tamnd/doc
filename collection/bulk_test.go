package collection

import (
	"errors"
	"testing"

	"github.com/tamnd/doc/bson"
)

// filterName builds {name: v}.
func filterName(v string) bson.Raw {
	return bson.NewBuilder().AppendString("name", v).Build()
}

// setN builds {$set:{n:v}}.
func setN(v int32) bson.Raw {
	return bson.NewBuilder().AppendDocument("$set",
		bson.NewBuilder().AppendInt32("n", v).Build()).Build()
}

func TestUpsertInsertsFromFilterEquality(t *testing.T) {
	c := newTestColl(t)
	res, err := c.UpdateOneWith(filterName("widget"), setN(7), UpdateOptions{Upsert: true})
	if err != nil {
		t.Fatalf("UpdateOneWith: %v", err)
	}
	if res.Matched != 0 || res.Modified != 0 || res.Upserted != 1 {
		t.Fatalf("result = %+v, want matched=0 modified=0 upserted=1", res)
	}
	if res.UpsertedID.Type == 0 {
		t.Fatal("UpsertedID is empty")
	}
	got, err := c.FindOne(filterName("widget"))
	if err != nil || got == nil {
		t.Fatalf("FindOne after upsert: doc=%v err=%v", got, err)
	}
	// The filter equality and the $set both land in the inserted document.
	if v, _ := got.Lookup("name"); v.StringValue() != "widget" {
		t.Fatalf("name = %q, want widget", v.StringValue())
	}
	if v, _ := got.Lookup("n"); v.Int32() != 7 {
		t.Fatalf("n = %d, want 7", v.Int32())
	}
}

func TestUpsertHonorsSetOnInsert(t *testing.T) {
	c := newTestColl(t)
	upd := bson.NewBuilder().
		AppendDocument("$set", bson.NewBuilder().AppendInt32("n", 1).Build()).
		AppendDocument("$setOnInsert", bson.NewBuilder().AppendString("origin", "seed").Build()).
		Build()
	if _, err := c.UpdateOneWith(filterName("a"), upd, UpdateOptions{Upsert: true}); err != nil {
		t.Fatalf("UpdateOneWith: %v", err)
	}
	got, _ := c.FindOne(filterName("a"))
	if got == nil {
		t.Fatal("upsert did not insert")
	}
	if v, ok := got.Lookup("origin"); !ok || v.StringValue() != "seed" {
		t.Fatalf("origin = %v ok=%v, want seed", v, ok)
	}
}

func TestUpsertOnMatchUpdatesWithoutInserting(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, bson.NewBuilder().AppendInt32("_id", 1).AppendString("name", "a").AppendInt32("n", 0).Build())
	res, err := c.UpdateOneWith(filterName("a"), setN(9), UpdateOptions{Upsert: true})
	if err != nil {
		t.Fatalf("UpdateOneWith: %v", err)
	}
	if res.Matched != 1 || res.Modified != 1 || res.Upserted != 0 {
		t.Fatalf("result = %+v, want matched=1 modified=1 upserted=0", res)
	}
	if n := c.count(t); n != 1 {
		t.Fatalf("collection has %d docs, want 1 (no insert)", n)
	}
}

func TestUpsertReplaceUsesFilterID(t *testing.T) {
	c := newTestColl(t)
	repl := bson.NewBuilder().AppendString("name", "z").Build()
	res, err := c.ReplaceOneWith(filterID(42), repl, UpdateOptions{Upsert: true})
	if err != nil {
		t.Fatalf("ReplaceOneWith: %v", err)
	}
	if res.Upserted != 1 || res.UpsertedID.Int32() != 42 {
		t.Fatalf("result = %+v, want upserted=1 id=42", res)
	}
	got, _ := c.FindOne(filterID(42))
	if got == nil {
		t.Fatal("upsert replace did not insert under the filter _id")
	}
	if v, _ := got.Lookup("name"); v.StringValue() != "z" {
		t.Fatalf("name = %q, want z", v.StringValue())
	}
}

func TestUpdateManyUpsertInsertsOne(t *testing.T) {
	c := newTestColl(t)
	res, err := c.UpdateManyWith(filterName("none"), setN(1), UpdateOptions{Upsert: true})
	if err != nil {
		t.Fatalf("UpdateManyWith: %v", err)
	}
	if res.Upserted != 1 {
		t.Fatalf("upserted = %d, want 1", res.Upserted)
	}
	if n := c.count(t); n != 1 {
		t.Fatalf("collection has %d docs, want exactly 1", n)
	}
}

func TestFindOneAndUpdateUpsertReturnsAfter(t *testing.T) {
	c := newTestColl(t)
	got, err := c.FindOneAndUpdate(filterName("p"), setN(3), FindModifyOptions{Upsert: true, Return: ReturnAfter})
	if err != nil {
		t.Fatalf("FindOneAndUpdate: %v", err)
	}
	if got == nil {
		t.Fatal("ReturnAfter upsert returned nil")
	}
	if v, _ := got.Lookup("n"); v.Int32() != 3 {
		t.Fatalf("n = %d, want 3", v.Int32())
	}
}

func TestInsertManyOrderedAll(t *testing.T) {
	c := newTestColl(t)
	docs := []bson.Raw{docID(1), docID(2), docID(3)}
	res, err := c.InsertMany(docs, true)
	if err != nil {
		t.Fatalf("InsertMany: %v", err)
	}
	if len(res.InsertedIDs) != 3 {
		t.Fatalf("inserted %d ids, want 3", len(res.InsertedIDs))
	}
	if n := c.count(t); n != 3 {
		t.Fatalf("collection has %d docs, want 3", n)
	}
}

func TestInsertManyOrderedHaltsOnDuplicate(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, docID(1))
	// Batch: 2 ok, 1 duplicates the existing doc and halts, 3 never attempted.
	res, err := c.InsertMany([]bson.Raw{docID(2), docID(1), docID(3)}, true)
	var bwe *BulkWriteException
	if !errors.As(err, &bwe) {
		t.Fatalf("err = %v, want *BulkWriteException", err)
	}
	if len(bwe.WriteErrors) != 1 || bwe.WriteErrors[0].Index != 1 {
		t.Fatalf("write errors = %+v, want one at index 1", bwe.WriteErrors)
	}
	if len(res.InsertedIDs) != 1 {
		t.Fatalf("inserted %d ids, want 1 (only doc 2)", len(res.InsertedIDs))
	}
	// Doc 2 committed, doc 3 not (ordered halt).
	if got, _ := c.FindOne(filterID(2)); got == nil {
		t.Fatal("doc 2 should be committed")
	}
	if got, _ := c.FindOne(filterID(3)); got != nil {
		t.Fatal("doc 3 should not be inserted after an ordered halt")
	}
}

func TestInsertManyUnorderedContinues(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, docID(1))
	res, err := c.InsertMany([]bson.Raw{docID(2), docID(1), docID(3)}, false)
	var bwe *BulkWriteException
	if !errors.As(err, &bwe) {
		t.Fatalf("err = %v, want *BulkWriteException", err)
	}
	if len(res.InsertedIDs) != 2 {
		t.Fatalf("inserted %d ids, want 2 (docs 2 and 3)", len(res.InsertedIDs))
	}
	if got, _ := c.FindOne(filterID(3)); got == nil {
		t.Fatal("doc 3 should be inserted in unordered mode")
	}
}

func TestBulkWriteMixed(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, bson.NewBuilder().AppendInt32("_id", 1).AppendString("name", "a").AppendInt32("n", 0).Build())
	mustInsert(t, c, bson.NewBuilder().AppendInt32("_id", 2).AppendString("name", "b").AppendInt32("n", 0).Build())
	ops := []BulkOp{
		InsertOneOp{Document: docID(3)},
		UpdateOneOp{Filter: filterID(1), Update: setN(5)},
		DeleteOneOp{Filter: filterID(2)},
		UpdateOneOp{Filter: filterName("ghost"), Update: setN(1), Upsert: true},
	}
	res, err := c.BulkWrite(ops, true)
	if err != nil {
		t.Fatalf("BulkWrite: %v", err)
	}
	if res.InsertedCount != 1 || res.MatchedCount != 1 || res.ModifiedCount != 1 || res.DeletedCount != 1 || res.UpsertedCount != 1 {
		t.Fatalf("result = %+v", res)
	}
	if _, ok := res.UpsertedIDs[3]; !ok {
		t.Fatalf("UpsertedIDs missing op index 3: %+v", res.UpsertedIDs)
	}
	if _, ok := res.InsertedIDs[0]; !ok {
		t.Fatalf("InsertedIDs missing op index 0: %+v", res.InsertedIDs)
	}
	if got, _ := c.FindOne(filterID(2)); got != nil {
		t.Fatal("doc 2 should be deleted")
	}
}

func TestBulkWriteOrderedHalts(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, docID(1))
	ops := []BulkOp{
		InsertOneOp{Document: docID(2)},
		InsertOneOp{Document: docID(1)}, // duplicate -> halts
		InsertOneOp{Document: docID(3)}, // not attempted
	}
	res, err := c.BulkWrite(ops, true)
	var bwe *BulkWriteException
	if !errors.As(err, &bwe) {
		t.Fatalf("err = %v, want *BulkWriteException", err)
	}
	if res.InsertedCount != 1 {
		t.Fatalf("inserted %d, want 1", res.InsertedCount)
	}
	if got, _ := c.FindOne(filterID(3)); got != nil {
		t.Fatal("doc 3 should not be inserted after an ordered halt")
	}
}

// count returns the total number of documents in the collection.
func (c *Collection) count(t *testing.T) int64 {
	t.Helper()
	n, err := c.CountDocuments(bson.NewBuilder().Build())
	if err != nil {
		t.Fatalf("CountDocuments: %v", err)
	}
	return n
}
