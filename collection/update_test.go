package collection

import (
	"errors"
	"testing"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/sys"
	"github.com/tamnd/doc/vfs"
)

// setDoc builds the update document {$set:{field:v}}.
func setDoc(field string, v int32) bson.Raw {
	inner := bson.NewBuilder().AppendInt32(field, v).Build()
	return bson.NewBuilder().AppendDocument("$set", inner).Build()
}

// incDoc builds {$inc:{field:v}}.
func incDoc(field string, v int32) bson.Raw {
	inner := bson.NewBuilder().AppendInt32(field, v).Build()
	return bson.NewBuilder().AppendDocument("$inc", inner).Build()
}

func mustInsert(t *testing.T, c *Collection, d bson.Raw) {
	t.Helper()
	if _, err := c.InsertOne(d); err != nil {
		t.Fatalf("InsertOne: %v", err)
	}
}

func lookupInt(t *testing.T, c *Collection, id int32, field string) (int32, bool) {
	t.Helper()
	got, err := c.FindOne(filterID(id))
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	if got == nil {
		return 0, false
	}
	v, ok := got.Lookup(field)
	if !ok {
		return 0, false
	}
	return v.Int32(), true
}

func TestUpdateOneSet(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, docInt(1, "n", 10))
	res, err := c.UpdateOne(filterID(1), setDoc("n", 20))
	if err != nil {
		t.Fatalf("UpdateOne: %v", err)
	}
	if res.Matched != 1 || res.Modified != 1 {
		t.Fatalf("result = %+v, want matched=1 modified=1", res)
	}
	if v, ok := lookupInt(t, c, 1, "n"); !ok || v != 20 {
		t.Fatalf("n = %d ok=%v, want 20", v, ok)
	}
}

func TestUpdateOneNewField(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, docInt(1, "n", 10))
	if _, err := c.UpdateOne(filterID(1), setDoc("m", 5)); err != nil {
		t.Fatalf("UpdateOne: %v", err)
	}
	if v, ok := lookupInt(t, c, 1, "m"); !ok || v != 5 {
		t.Fatalf("m = %d ok=%v, want 5", v, ok)
	}
	if v, ok := lookupInt(t, c, 1, "n"); !ok || v != 10 {
		t.Fatalf("n = %d ok=%v, want 10 preserved", v, ok)
	}
}

func TestUpdateOneNoMatch(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, docInt(1, "n", 10))
	res, err := c.UpdateOne(filterID(2), setDoc("n", 20))
	if err != nil {
		t.Fatalf("UpdateOne: %v", err)
	}
	if res.Matched != 0 || res.Modified != 0 {
		t.Fatalf("result = %+v, want all zero", res)
	}
}

func TestUpdateOneMatchedNotModified(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, docInt(1, "n", 10))
	res, err := c.UpdateOne(filterID(1), setDoc("n", 10))
	if err != nil {
		t.Fatalf("UpdateOne: %v", err)
	}
	if res.Matched != 1 || res.Modified != 0 {
		t.Fatalf("result = %+v, want matched=1 modified=0", res)
	}
}

func TestUpdateManyInc(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, docInt(1, "g", 1))
	mustInsert(t, c, docInt(2, "g", 1))
	mustInsert(t, c, docInt(3, "g", 2))
	filter := bson.NewBuilder().AppendInt32("g", 1).Build()
	res, err := c.UpdateMany(filter, incDoc("g", 10))
	if err != nil {
		t.Fatalf("UpdateMany: %v", err)
	}
	if res.Matched != 2 || res.Modified != 2 {
		t.Fatalf("result = %+v, want matched=2 modified=2", res)
	}
	if v, _ := lookupInt(t, c, 1, "g"); v != 11 {
		t.Fatalf("doc1 g = %d, want 11", v)
	}
	if v, _ := lookupInt(t, c, 3, "g"); v != 2 {
		t.Fatalf("doc3 g = %d, want 2 untouched", v)
	}
}

func TestUpdateImmutableID(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, docInt(1, "n", 10))
	upd := bson.NewBuilder().AppendDocument("$set",
		bson.NewBuilder().AppendInt32("_id", 2).Build()).Build()
	_, err := c.UpdateOne(filterID(1), upd)
	if !errors.Is(err, ErrImmutableField) {
		t.Fatalf("err = %v, want ErrImmutableField", err)
	}
}

func TestUpdateRejectsReplacementDoc(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, docInt(1, "n", 10))
	_, err := c.UpdateOne(filterID(1), docInt(1, "n", 99))
	if err == nil {
		t.Fatal("UpdateOne with a replacement document should error")
	}
}

func TestReplaceOne(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, docInt(1, "n", 10))
	repl := bson.NewBuilder().AppendInt32("x", 7).Build()
	res, err := c.ReplaceOne(filterID(1), repl)
	if err != nil {
		t.Fatalf("ReplaceOne: %v", err)
	}
	if res.Matched != 1 || res.Modified != 1 {
		t.Fatalf("result = %+v, want matched=1 modified=1", res)
	}
	got, _ := c.FindOne(filterID(1))
	if _, ok := got.Lookup("n"); ok {
		t.Fatal("old field n should be gone after replace")
	}
	if v, ok := got.Lookup("x"); !ok || v.Int32() != 7 {
		t.Fatalf("x = %+v ok=%v, want 7", v, ok)
	}
	if id, ok := got.Lookup("_id"); !ok || id.Int32() != 1 {
		t.Fatalf("_id = %+v, want 1 preserved", id)
	}
}

func TestReplaceRejectsOperatorDoc(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, docInt(1, "n", 10))
	_, err := c.ReplaceOne(filterID(1), setDoc("n", 20))
	if err == nil {
		t.Fatal("ReplaceOne with an operator document should error")
	}
}

func TestReplaceDifferentIDRejected(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, docInt(1, "n", 10))
	repl := bson.NewBuilder().AppendInt32("_id", 2).AppendInt32("x", 7).Build()
	_, err := c.ReplaceOne(filterID(1), repl)
	if !errors.Is(err, ErrImmutableField) {
		t.Fatalf("err = %v, want ErrImmutableField", err)
	}
}

func TestFindOneAndUpdateReturnBefore(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, docInt(1, "n", 10))
	got, err := c.FindOneAndUpdate(filterID(1), setDoc("n", 20), FindModifyOptions{})
	if err != nil {
		t.Fatalf("FindOneAndUpdate: %v", err)
	}
	if v, ok := got.Lookup("n"); !ok || v.Int32() != 10 {
		t.Fatalf("returned n = %+v, want 10 (before)", v)
	}
	if v, _ := lookupInt(t, c, 1, "n"); v != 20 {
		t.Fatalf("stored n = %d, want 20", v)
	}
}

func TestFindOneAndUpdateReturnAfter(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, docInt(1, "n", 10))
	got, err := c.FindOneAndUpdate(filterID(1), setDoc("n", 20), FindModifyOptions{Return: ReturnAfter})
	if err != nil {
		t.Fatalf("FindOneAndUpdate: %v", err)
	}
	if v, ok := got.Lookup("n"); !ok || v.Int32() != 20 {
		t.Fatalf("returned n = %+v, want 20 (after)", v)
	}
}

func TestFindOneAndUpdateSortPicksFirst(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, docInt(1, "n", 30))
	mustInsert(t, c, docInt(2, "n", 10))
	mustInsert(t, c, docInt(3, "n", 20))
	sort := bson.NewBuilder().AppendInt32("n", 1).Build()
	got, err := c.FindOneAndUpdate(emptyFilter(), setDoc("n", 99), FindModifyOptions{Sort: sort})
	if err != nil {
		t.Fatalf("FindOneAndUpdate: %v", err)
	}
	if id, _ := got.Lookup("_id"); id.Int32() != 2 {
		t.Fatalf("acted on _id %d, want 2 (smallest n)", id.Int32())
	}
}

func TestFindOneAndDelete(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, docInt(1, "n", 10))
	got, err := c.FindOneAndDelete(filterID(1), FindModifyOptions{})
	if err != nil {
		t.Fatalf("FindOneAndDelete: %v", err)
	}
	if v, ok := got.Lookup("n"); !ok || v.Int32() != 10 {
		t.Fatalf("returned n = %+v, want 10", v)
	}
	if doc, _ := c.FindOne(filterID(1)); doc != nil {
		t.Fatal("document should be gone after FindOneAndDelete")
	}
}

func TestFindOneAndReplaceReturnAfter(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, docInt(1, "n", 10))
	repl := bson.NewBuilder().AppendInt32("x", 7).Build()
	got, err := c.FindOneAndReplace(filterID(1), repl, FindModifyOptions{Return: ReturnAfter})
	if err != nil {
		t.Fatalf("FindOneAndReplace: %v", err)
	}
	if v, ok := got.Lookup("x"); !ok || v.Int32() != 7 {
		t.Fatalf("returned x = %+v, want 7", v)
	}
	if _, ok := got.Lookup("n"); ok {
		t.Fatal("old field n should be gone")
	}
}

func TestFindOneAndUpdateNoMatch(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, docInt(1, "n", 10))
	got, err := c.FindOneAndUpdate(filterID(2), setDoc("n", 20), FindModifyOptions{})
	if err != nil {
		t.Fatalf("FindOneAndUpdate: %v", err)
	}
	if got != nil {
		t.Fatalf("got %v, want nil for no match", got)
	}
}

func TestDistinct(t *testing.T) {
	c := newTestColl(t)
	mustInsert(t, c, docInt(1, "g", 1))
	mustInsert(t, c, docInt(2, "g", 2))
	mustInsert(t, c, docInt(3, "g", 1))
	mustInsert(t, c, docInt(4, "g", 3))
	vals, err := c.Distinct("g", emptyFilter())
	if err != nil {
		t.Fatalf("Distinct: %v", err)
	}
	if len(vals) != 3 {
		t.Fatalf("got %d distinct values, want 3", len(vals))
	}
	want := []int32{1, 2, 3}
	for i, v := range vals {
		if v.Int32() != want[i] {
			t.Fatalf("vals[%d] = %d, want %d", i, v.Int32(), want[i])
		}
	}
}

func TestDistinctArrayUnwind(t *testing.T) {
	c := newTestColl(t)
	arr := bson.BuildArray(int32RV(1), int32RV(2), int32RV(2))
	d := bson.NewBuilder().AppendInt32("_id", 1).AppendArray("tags", arr).Build()
	mustInsert(t, c, d)
	d2 := bson.NewBuilder().AppendInt32("_id", 2).AppendArray("tags", bson.BuildArray(int32RV(3))).Build()
	mustInsert(t, c, d2)
	vals, err := c.Distinct("tags", emptyFilter())
	if err != nil {
		t.Fatalf("Distinct: %v", err)
	}
	if len(vals) != 3 {
		t.Fatalf("got %d distinct tags, want 3 (1,2,3)", len(vals))
	}
}

func TestUpdatePersistsAcrossReopen(t *testing.T) {
	fs := vfs.NewMemFS()
	opts := Options{IDGen: &sys.FixedIDGenerator{Timestamp: 1}}
	c, err := Open(fs, "u.doc", opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mustInsert(t, c, docInt(1, "n", 10))
	if _, err := c.UpdateOne(filterID(1), setDoc("n", 42)); err != nil {
		t.Fatalf("UpdateOne: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	c2, err := Open(fs, "u.doc", opts)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = c2.Close() }()
	got, err := c2.FindOne(filterID(1))
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	if v, ok := got.Lookup("n"); !ok || v.Int32() != 42 {
		t.Fatalf("after reopen n = %+v, want 42", v)
	}
}

func emptyFilter() bson.Raw { return bson.NewBuilder().Build() }

func int32RV(v int32) bson.RawValue {
	d := bson.NewBuilder().AppendInt32("v", v).Build()
	rv, _ := d.Lookup("v")
	return rv
}
