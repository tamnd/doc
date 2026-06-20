package collection

import (
	"testing"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/catalog"
)

// strVal frames a string as a RawValue for building array documents.
func strVal(s string) bson.RawValue {
	d := bson.NewBuilder().AppendString("x", s).Build()
	rv, _ := d.Lookup("x")
	return rv
}

// docTags builds {_id: id, tags: [...]}.
func docTags(id int32, tags ...string) bson.Raw {
	vals := make([]bson.RawValue, len(tags))
	for i, t := range tags {
		vals[i] = strVal(t)
	}
	return bson.NewBuilder().
		AppendInt32("_id", id).
		AppendArray("tags", bson.BuildArray(vals...)).
		Build()
}

// pushDoc builds {$push:{tags:v}}.
func pushDoc(v string) bson.Raw {
	return bson.NewBuilder().AppendDocument("$push",
		bson.NewBuilder().AppendValue("tags", strVal(v)).Build()).Build()
}

// pullDoc builds {$pull:{tags:v}}.
func pullDoc(v string) bson.Raw {
	return bson.NewBuilder().AppendDocument("$pull",
		bson.NewBuilder().AppendValue("tags", strVal(v)).Build()).Build()
}

func TestArrayUpdateMaintainsMultikeyIndex(t *testing.T) {
	c := newTestColl(t)
	name, err := c.CreateIndex(IndexModel{Key: []catalog.KeyPart{{Field: "tags"}}})
	if err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	mustInsert(t, c, docTags(1, "go", "db"))
	mustInsert(t, c, docTags(2, "rust"))
	// 3 entries: go, db, rust.
	if got := c.entryCount(name); got != 3 {
		t.Fatalf("after inserts entry count = %d, want 3", got)
	}

	// $push adds one more multikey entry.
	if _, err := c.UpdateOne(filterID(1), pushDoc("fast")); err != nil {
		t.Fatalf("UpdateOne $push: %v", err)
	}
	if got := c.entryCount(name); got != 4 {
		t.Fatalf("after $push entry count = %d, want 4", got)
	}

	// $pull removes a multikey entry.
	if _, err := c.UpdateOne(filterID(1), pullDoc("db")); err != nil {
		t.Fatalf("UpdateOne $pull: %v", err)
	}
	if got := c.entryCount(name); got != 3 {
		t.Fatalf("after $pull entry count = %d, want 3", got)
	}

	// The document reflects the array edits.
	got, err := c.FindOne(filterID(1))
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	tags, ok := got.Lookup("tags")
	if !ok || tags.Type != bson.TypeArray {
		t.Fatalf("tags missing or not an array")
	}
	elems, _ := tags.Document().Elements()
	if len(elems) != 2 || elems[0].Value.StringValue() != "go" || elems[1].Value.StringValue() != "fast" {
		t.Fatalf("tags = %v, want [go, fast]", elems)
	}
}

func TestAddToSetThroughCollection(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, docTags(1, "go"))
	addToSet := bson.NewBuilder().AppendDocument("$addToSet",
		bson.NewBuilder().AppendValue("tags", strVal("go")).Build()).Build()
	res, err := c.UpdateOne(filterID(1), addToSet)
	if err != nil {
		t.Fatalf("UpdateOne: %v", err)
	}
	if res.Matched != 1 || res.Modified != 0 {
		t.Fatalf("adding an existing value: matched=%d modified=%d, want 1/0", res.Matched, res.Modified)
	}
}
