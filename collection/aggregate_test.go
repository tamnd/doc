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
