package update

import (
	"testing"

	"github.com/tamnd/doc/bson"
)

func vStr(v string) bson.RawValue { rv, _ := doc(fStr("x", v)).Lookup("x"); return rv }

// arrVal frames element values into an array RawValue for assertions.
func arrVal(vals ...bson.RawValue) bson.RawValue {
	d := bson.NewBuilder().AppendArray("x", bson.BuildArray(vals...)).Build()
	rv, _ := d.Lookup("x")
	return rv
}

// wantArr asserts the field holds exactly the listed array element values.
func wantArr(t *testing.T, got bson.Raw, field string, want ...bson.RawValue) {
	t.Helper()
	v := lookup(t, got, field)
	if v.Type != bson.TypeArray {
		t.Fatalf("%s: type %v, want array", field, v.Type)
	}
	gotVals, err := arrayValues(v)
	if err != nil {
		t.Fatalf("arrayValues: %v", err)
	}
	if len(gotVals) != len(want) {
		t.Fatalf("%s: len %d, want %d", field, len(gotVals), len(want))
	}
	for i := range want {
		if bson.Compare(gotVals[i], want[i]) != 0 {
			t.Fatalf("%s[%d]: got %v, want %v", field, i, gotVals[i], want[i])
		}
	}
}

func TestPushSimpleAppendsAndCreates(t *testing.T) {
	before := doc(fInt("_id", 1), fArr("tags", vStr("db")))
	upd := doc(fDoc("$push", doc(func(b *bson.Builder) { b.AppendValue("tags", vStr("go")) })))
	out, mod := applyOK(t, before, upd)
	if !mod {
		t.Fatal("expected modified")
	}
	wantArr(t, out, "tags", vStr("db"), vStr("go"))

	// Missing field is created as a one-element array.
	out2, _ := applyOK(t, doc(fInt("_id", 1)), upd)
	wantArr(t, out2, "tags", vStr("go"))
}

func TestPushNestsArrayValueAsOneElement(t *testing.T) {
	before := doc(fInt("_id", 1), fArr("a", vInt(1)))
	upd := doc(fDoc("$push", doc(func(b *bson.Builder) {
		b.AppendValue("a", arrVal(vInt(2), vInt(3)))
	})))
	out, _ := applyOK(t, before, upd)
	// Without $each the array value is appended as a single nested element.
	v := lookup(t, out, "a")
	vals, _ := arrayValues(v)
	if len(vals) != 2 || vals[1].Type != bson.TypeArray {
		t.Fatalf("expected [1, [2,3]], got %v", vals)
	}
}

func TestPushEachSortSlice(t *testing.T) {
	before := doc(fInt("_id", 1), fArr("scores", vInt(5), vInt(3), vInt(8)))
	mod := doc(func(b *bson.Builder) {
		b.AppendArray("$each", bson.BuildArray(vInt(6), vInt(1)))
		b.AppendInt32("$sort", -1)
		b.AppendInt32("$slice", 3)
	})
	upd := doc(fDoc("$push", doc(fDoc("scores", mod))))
	out, _ := applyOK(t, before, upd)
	wantArr(t, out, "scores", vInt(8), vInt(6), vInt(5))
}

func TestPushPosition(t *testing.T) {
	before := doc(fInt("_id", 1), fArr("items", vStr("a"), vStr("b"), vStr("c")))
	mod := doc(func(b *bson.Builder) {
		b.AppendArray("$each", bson.BuildArray(vStr("x")))
		b.AppendInt32("$position", 1)
	})
	upd := doc(fDoc("$push", doc(fDoc("items", mod))))
	out, _ := applyOK(t, before, upd)
	wantArr(t, out, "items", vStr("a"), vStr("x"), vStr("b"), vStr("c"))
}

func TestPushSortSubdocField(t *testing.T) {
	mk := func(score int32) bson.RawValue {
		d := bson.NewBuilder().AppendInt32("v", score).Build()
		dd := bson.NewBuilder().AppendDocument("x", d).Build()
		rv, _ := dd.Lookup("x")
		return rv
	}
	before := doc(fInt("_id", 1), fArr("rows", mk(3), mk(1)))
	mod := doc(func(b *bson.Builder) {
		b.AppendArray("$each", bson.BuildArray(mk(2)))
		b.AppendDocument("$sort", bson.NewBuilder().AppendInt32("v", 1).Build())
	})
	upd := doc(fDoc("$push", doc(fDoc("rows", mod))))
	out, _ := applyOK(t, before, upd)
	v := lookup(t, out, "rows")
	vals, _ := arrayValues(v)
	got := make([]int32, len(vals))
	for i, e := range vals {
		sv, _ := e.Document().Lookup("v")
		got[i] = sv.Int32()
	}
	if got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("sort by field: got %v", got)
	}
}

func TestAddToSetDedupes(t *testing.T) {
	before := doc(fInt("_id", 1), fArr("tags", vStr("go"), vStr("db")))
	mod := doc(func(b *bson.Builder) {
		b.AppendArray("$each", bson.BuildArray(vStr("go"), vStr("fast"), vStr("db")))
	})
	upd := doc(fDoc("$addToSet", doc(fDoc("tags", mod))))
	out, _ := applyOK(t, before, upd)
	wantArr(t, out, "tags", vStr("go"), vStr("db"), vStr("fast"))
}

func TestAddToSetNoChangeIsNoop(t *testing.T) {
	before := doc(fInt("_id", 1), fArr("tags", vStr("go")))
	upd := doc(fDoc("$addToSet", doc(func(b *bson.Builder) { b.AppendValue("tags", vStr("go")) })))
	_, mod := applyOK(t, before, upd)
	if mod {
		t.Fatal("adding an existing value should be a no-op")
	}
}

func TestPop(t *testing.T) {
	before := doc(fInt("_id", 1), fArr("q", vStr("a"), vStr("b"), vStr("c")))
	last := doc(fDoc("$pop", doc(func(b *bson.Builder) { b.AppendInt32("q", 1) })))
	out, _ := applyOK(t, before, last)
	wantArr(t, out, "q", vStr("a"), vStr("b"))

	first := doc(fDoc("$pop", doc(func(b *bson.Builder) { b.AppendInt32("q", -1) })))
	out2, _ := applyOK(t, before, first)
	wantArr(t, out2, "q", vStr("b"), vStr("c"))
}

func TestPopMissingIsNoop(t *testing.T) {
	before := doc(fInt("_id", 1))
	upd := doc(fDoc("$pop", doc(func(b *bson.Builder) { b.AppendInt32("q", 1) })))
	_, mod := applyOK(t, before, upd)
	if mod {
		t.Fatal("$pop on a missing field should be a no-op")
	}
}

func TestPullScalar(t *testing.T) {
	before := doc(fInt("_id", 1), fArr("s", vInt(1), vInt(2), vInt(3), vInt(2), vInt(4)))
	upd := doc(fDoc("$pull", doc(func(b *bson.Builder) { b.AppendInt32("s", 2) })))
	out, _ := applyOK(t, before, upd)
	wantArr(t, out, "s", vInt(1), vInt(3), vInt(4))
}

func TestPullQueryOperatorForm(t *testing.T) {
	before := doc(fInt("_id", 1), fArr("s", vInt(1), vInt(6), vInt(3), vInt(8)))
	cond := doc(func(b *bson.Builder) { b.AppendInt32("$gte", 5) })
	upd := doc(fDoc("$pull", doc(fDoc("s", cond))))
	out, _ := applyOK(t, before, upd)
	wantArr(t, out, "s", vInt(1), vInt(3))
}

func TestPullQueryFieldForm(t *testing.T) {
	mk := func(item string, score int32) bson.RawValue {
		d := bson.NewBuilder().AppendString("item", item).AppendInt32("score", score).Build()
		dd := bson.NewBuilder().AppendDocument("x", d).Build()
		rv, _ := dd.Lookup("x")
		return rv
	}
	before := doc(fInt("_id", 1), fArr("votes", mk("a", 4), mk("b", 1), mk("c", 5)))
	cond := doc(fDoc("score", doc(func(b *bson.Builder) { b.AppendInt32("$lt", 3) })))
	upd := doc(fDoc("$pull", doc(fDoc("votes", cond))))
	out, _ := applyOK(t, before, upd)
	v := lookup(t, out, "votes")
	vals, _ := arrayValues(v)
	if len(vals) != 2 {
		t.Fatalf("expected 2 votes left, got %d", len(vals))
	}
}

func TestPullAll(t *testing.T) {
	before := doc(fInt("_id", 1), fArr("c", vStr("red"), vStr("blue"), vStr("green"), vStr("red")))
	upd := doc(fDoc("$pullAll", doc(func(b *bson.Builder) {
		b.AppendArray("c", bson.BuildArray(vStr("red"), vStr("blue")))
	})))
	out, _ := applyOK(t, before, upd)
	wantArr(t, out, "c", vStr("green"))
}

func TestBit(t *testing.T) {
	before := doc(fInt("_id", 1), fInt("flags", 0b1100))
	or := doc(fDoc("$bit", doc(fDoc("flags", doc(func(b *bson.Builder) { b.AppendInt32("or", 0b0011) })))))
	out, _ := applyOK(t, before, or)
	if lookup(t, out, "flags").Int32() != 0b1111 {
		t.Fatalf("or: got %d", lookup(t, out, "flags").Int32())
	}

	before2 := doc(fInt("_id", 2), fInt("flags", 0b1010))
	and := doc(fDoc("$bit", doc(fDoc("flags", doc(func(b *bson.Builder) { b.AppendInt32("and", 0b1100) })))))
	out2, _ := applyOK(t, before2, and)
	if lookup(t, out2, "flags").Int32() != 0b1000 {
		t.Fatalf("and: got %d", lookup(t, out2, "flags").Int32())
	}
}

func TestBitOnMissingFieldErrors(t *testing.T) {
	before := doc(fInt("_id", 1))
	upd := doc(fDoc("$bit", doc(fDoc("flags", doc(func(b *bson.Builder) { b.AppendInt32("or", 1) })))))
	u, err := Compile(upd)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if _, _, err := u.Apply(before, epoch); err != ErrBitType {
		t.Fatalf("expected ErrBitType, got %v", err)
	}
}

func TestArrayOperatorOnNonArrayErrors(t *testing.T) {
	before := doc(fInt("_id", 1), fStr("tags", "scalar"))
	upd := doc(fDoc("$push", doc(func(b *bson.Builder) { b.AppendValue("tags", vStr("go")) })))
	u, _ := Compile(upd)
	if _, _, err := u.Apply(before, epoch); err != ErrBadArrayOperand {
		t.Fatalf("expected ErrBadArrayOperand, got %v", err)
	}
}

func TestSetOnInsertOnlyAppliesOnInsert(t *testing.T) {
	upd := doc(
		fDoc("$set", doc(fInt("n", 5))),
		fDoc("$setOnInsert", doc(fStr("created", "now"))),
	)
	u, err := Compile(upd)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	// Normal update path: $setOnInsert is skipped.
	out, _, err := u.Apply(doc(fInt("_id", 1)), epoch)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, ok := out.Lookup("created"); ok {
		t.Fatal("$setOnInsert applied on a non-insert update")
	}
	// Insert branch: $setOnInsert applies.
	out2, _, err := u.ApplyForInsert(doc(fInt("_id", 1)), epoch)
	if err != nil {
		t.Fatalf("ApplyForInsert: %v", err)
	}
	if _, ok := out2.Lookup("created"); !ok {
		t.Fatal("$setOnInsert not applied on the insert branch")
	}
	if lookup(t, out2, "n").Int32() != 5 {
		t.Fatal("$set not applied on the insert branch")
	}
}
