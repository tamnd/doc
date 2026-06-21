package collection

import (
	"testing"

	"github.com/tamnd/doc/bson"
)

// aggDoc builds {_id, cat, qty}.
func aggDoc(id int32, cat string, qty int32) bson.Raw {
	return bson.NewBuilder().
		AppendInt32("_id", id).
		AppendString("cat", cat).
		AppendInt32("qty", qty).
		Build()
}

func TestAggregateMatchProject(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, aggDoc(1, "a", 5))
	mustInsert(t, c, aggDoc(2, "b", 15))
	mustInsert(t, c, aggDoc(3, "a", 25))

	match := bson.NewBuilder().AppendDocument("$match",
		bson.NewBuilder().AppendDocument("qty",
			bson.NewBuilder().AppendInt32("$gte", 10).Build()).Build()).Build()
	project := bson.NewBuilder().AppendDocument("$project",
		bson.NewBuilder().AppendInt32("cat", 1).AppendInt32("_id", 0).Build()).Build()

	out, err := c.Aggregate([]bson.Raw{match, project})
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d docs, want 2", len(out))
	}
	for _, d := range out {
		if _, ok := d.Lookup("_id"); ok {
			t.Fatal("_id should be projected out")
		}
		if _, ok := d.Lookup("cat"); !ok {
			t.Fatal("cat should be present")
		}
	}
}

func TestAggregateComputedAndUnwind(t *testing.T) {
	c := newTestColl(t)
	tags := bson.BuildArray(
		bsonString("x"), bsonString("y"), bsonString("z"))
	doc := bson.NewBuilder().AppendInt32("_id", 1).AppendArray("tags", tags).Build()
	mustInsert(t, c, doc)

	unwind := bson.NewBuilder().AppendString("$unwind", "$tags").Build()
	out, err := c.Aggregate([]bson.Raw{unwind})
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("unwind produced %d docs, want 3", len(out))
	}
}

// bsonString builds a string RawValue for array construction.
func bsonString(s string) bson.RawValue {
	v, _ := bson.NewBuilder().AppendString("v", s).Build().Lookup("v")
	return v
}

func TestAggregateGroup(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, aggDoc(1, "a", 5))
	mustInsert(t, c, aggDoc(2, "a", 15))
	mustInsert(t, c, aggDoc(3, "b", 25))

	group := bson.NewBuilder().AppendDocument("$group", bson.NewBuilder().
		AppendString("_id", "$cat").
		AppendDocument("total", bson.NewBuilder().AppendString("$sum", "$qty").Build()).
		Build()).Build()
	out, err := c.Aggregate([]bson.Raw{group})
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	got := map[string]int32{}
	for _, d := range out {
		id, _ := d.Lookup("_id")
		total, _ := d.Lookup("total")
		got[id.StringValue()] = total.Int32()
	}
	if got["a"] != 20 || got["b"] != 25 {
		t.Fatalf("group totals = %v", got)
	}
}

func TestAggregateLookupSelfJoin(t *testing.T) {
	// With one collection per file (M4), $lookup resolves the foreign name to this
	// same collection, so a self-join on cat groups documents that share a category.
	c := newTestColl(t)
	mustInsert(t, c, aggDoc(1, "a", 5))
	mustInsert(t, c, aggDoc(2, "a", 15))
	mustInsert(t, c, aggDoc(3, "b", 25))

	lookup := bson.NewBuilder().AppendDocument("$lookup", bson.NewBuilder().
		AppendString("from", "self").
		AppendString("localField", "cat").
		AppendString("foreignField", "cat").
		AppendString("as", "same").
		Build()).Build()
	match := bson.NewBuilder().AppendDocument("$match",
		bson.NewBuilder().AppendDocument("_id",
			bson.NewBuilder().AppendInt32("$eq", 1).Build()).Build()).Build()
	out, err := c.Aggregate([]bson.Raw{match, lookup})
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	same, _ := out[0].Lookup("same")
	els, _ := same.Document().Elements()
	if len(els) != 2 {
		t.Fatalf("self-join matched %d docs in cat a, want 2", len(els))
	}
}

func TestAggregateOut(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, aggDoc(1, "a", 5))
	mustInsert(t, c, aggDoc(2, "b", 15))

	// $out replaces the collection with the projected output. Run inside a writable
	// transaction so the write path is exercised.
	addField := bson.NewBuilder().AppendDocument("$addFields",
		bson.NewBuilder().AppendInt32("tagged", 1).Build()).Build()
	out := bson.NewBuilder().AppendString("$out", "self").Build()
	tx := c.Begin()
	if _, err := tx.Aggregate([]bson.Raw{addField, out}); err != nil {
		t.Fatalf("Aggregate $out: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	all, err := c.Aggregate([]bson.Raw{})
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("after $out have %d docs, want 2", len(all))
	}
	for _, d := range all {
		if _, ok := d.Lookup("tagged"); !ok {
			t.Fatal("rewritten doc missing tagged field")
		}
	}
}

func TestAggregateMerge(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, aggDoc(1, "a", 5))

	// $merge with whenMatched replace overwrites the existing _id:1 document.
	addField := bson.NewBuilder().AppendDocument("$addFields",
		bson.NewBuilder().AppendInt32("qty", 99).Build()).Build()
	merge := bson.NewBuilder().AppendDocument("$merge", bson.NewBuilder().
		AppendString("into", "self").
		AppendString("whenMatched", "replace").
		AppendString("whenNotMatched", "insert").
		Build()).Build()
	tx := c.Begin()
	if _, err := tx.Aggregate([]bson.Raw{addField, merge}); err != nil {
		t.Fatalf("Aggregate $merge: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	got, err := c.FindOne(bson.NewBuilder().AppendInt32("_id", 1).Build())
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	qty, _ := got.Lookup("qty")
	if qty.Int32() != 99 {
		t.Fatalf("merge did not replace qty: %d", qty.Int32())
	}
}
