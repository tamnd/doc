package update

import (
	"testing"
	"time"

	"github.com/tamnd/doc/bson"
)

// ---- builders ------------------------------------------------------------

func doc(build ...func(*bson.Builder)) bson.Raw {
	b := bson.NewBuilder()
	for _, f := range build {
		f(b)
	}
	return b.Build()
}

func fInt(k string, v int32) func(*bson.Builder) {
	return func(b *bson.Builder) { b.AppendInt32(k, v) }
}
func fLong(k string, v int64) func(*bson.Builder) {
	return func(b *bson.Builder) { b.AppendInt64(k, v) }
}
func fDbl(k string, v float64) func(*bson.Builder) {
	return func(b *bson.Builder) { b.AppendDouble(k, v) }
}
func fStr(k, v string) func(*bson.Builder) { return func(b *bson.Builder) { b.AppendString(k, v) } }
func fBool(k string, v bool) func(*bson.Builder) {
	return func(b *bson.Builder) { b.AppendBoolean(k, v) }
}

func fDoc(k string, sub bson.Raw) func(*bson.Builder) {
	return func(b *bson.Builder) { b.AppendDocument(k, sub) }
}
func fArr(k string, vals ...bson.RawValue) func(*bson.Builder) {
	return func(b *bson.Builder) { b.AppendArray(k, bson.BuildArray(vals...)) }
}

func vInt(v int32) bson.RawValue { rv, _ := doc(fInt("x", v)).Lookup("x"); return rv }

var epoch = time.Unix(1_700_000_000, 0).UTC()

// applyOK compiles update u, applies it to before, and returns the result.
func applyOK(t *testing.T, before, upd bson.Raw) (bson.Raw, bool) {
	t.Helper()
	u, err := Compile(upd)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	out, mod, err := u.Apply(before, epoch)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := out.Validate(); err != nil {
		t.Fatalf("result not well-formed: %v", err)
	}
	return out, mod
}

// wantDoc asserts the result equals want byte for byte.
func wantDoc(t *testing.T, got, want bson.Raw) {
	t.Helper()
	if len(got) != len(want) || string(got) != string(want) {
		t.Fatalf("doc mismatch\n got: %x\nwant: %x", got, want)
	}
}

// lookup returns a top-level field value for assertions.
func lookup(t *testing.T, d bson.Raw, key string) bson.RawValue {
	t.Helper()
	v, ok := d.Lookup(key)
	if !ok {
		t.Fatalf("field %q missing", key)
	}
	return v
}

// ---- $set / $unset -------------------------------------------------------

func TestSetExistingAndNew(t *testing.T) {
	before := doc(fInt("_id", 1), fStr("name", "alice"))
	upd := doc(func(b *bson.Builder) {
		b.AppendDocument("$set", doc(fStr("name", "bob"), fInt("score", 42)))
	})
	out, mod := applyOK(t, before, upd)
	if !mod {
		t.Fatal("expected modified")
	}
	want := doc(fInt("_id", 1), fStr("name", "bob"), fInt("score", 42))
	wantDoc(t, out, want)
}

func TestSetSameValueNotModified(t *testing.T) {
	before := doc(fInt("_id", 1), fStr("name", "alice"))
	upd := doc(fDoc("$set", doc(fStr("name", "alice"))))
	out, mod := applyOK(t, before, upd)
	if mod {
		t.Fatal("expected not modified")
	}
	wantDoc(t, out, before)
}

func TestSetCreatesNestedDocument(t *testing.T) {
	before := doc(fInt("_id", 1), fDoc("addr", doc(fStr("city", "nyc"))))
	upd := doc(fDoc("$set", doc(fStr("addr.zip", "10001"))))
	out, _ := applyOK(t, before, upd)
	addr := lookup(t, out, "addr").Document()
	if v, _ := addr.Lookup("zip"); v.StringValue() != "10001" {
		t.Fatalf("addr.zip = %q", v.StringValue())
	}
	if v, _ := addr.Lookup("city"); v.StringValue() != "nyc" {
		t.Fatal("city lost")
	}
}

func TestSetCreatesIntermediatePath(t *testing.T) {
	before := doc(fInt("_id", 1))
	upd := doc(fDoc("$set", doc(fInt("a.b.c", 7))))
	out, _ := applyOK(t, before, upd)
	a := lookup(t, out, "a").Document()
	b := mustLookup(t, a, "b").Document()
	if v := mustLookup(t, b, "c"); v.Int32() != 7 {
		t.Fatalf("a.b.c = %d", v.Int32())
	}
}

func TestSetThroughScalarIsConflict(t *testing.T) {
	before := doc(fInt("_id", 1), fInt("a", 5))
	upd := doc(fDoc("$set", doc(fInt("a.b", 7))))
	u, err := Compile(upd)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if _, _, err := u.Apply(before, epoch); err != ErrPathConflict {
		t.Fatalf("err = %v, want ErrPathConflict", err)
	}
}

func TestUnsetRemovesField(t *testing.T) {
	before := doc(fInt("_id", 1), fStr("name", "alice"), fBool("temp", true))
	upd := doc(fDoc("$unset", doc(fStr("temp", ""))))
	out, mod := applyOK(t, before, upd)
	if !mod {
		t.Fatal("expected modified")
	}
	if _, ok := out.Lookup("temp"); ok {
		t.Fatal("temp not removed")
	}
	wantDoc(t, out, doc(fInt("_id", 1), fStr("name", "alice")))
}

func TestUnsetMissingIsNoop(t *testing.T) {
	before := doc(fInt("_id", 1))
	upd := doc(fDoc("$unset", doc(fStr("ghost", ""))))
	out, mod := applyOK(t, before, upd)
	if mod {
		t.Fatal("expected not modified")
	}
	wantDoc(t, out, before)
}

func TestUnsetArrayElementSetsNull(t *testing.T) {
	before := doc(fInt("_id", 1), fArr("a", vInt(1), vInt(2), vInt(3)))
	upd := doc(fDoc("$unset", doc(fStr("a.1", ""))))
	out, _ := applyOK(t, before, upd)
	arr := lookup(t, out, "a")
	elems, _ := arr.Document().Elements()
	if len(elems) != 3 {
		t.Fatalf("array length changed: %d", len(elems))
	}
	if elems[1].Value.Type != bson.TypeNull {
		t.Fatalf("a.1 type = %v, want null", elems[1].Value.Type)
	}
}

// ---- $inc / $mul ---------------------------------------------------------

func TestIncExistingAndMissing(t *testing.T) {
	before := doc(fInt("_id", 1), fInt("score", 10))
	upd := doc(fDoc("$inc", doc(fInt("score", 5), fInt("views", 1))))
	out, _ := applyOK(t, before, upd)
	if v := mustLookup(t, out, "score"); v.Int32() != 15 {
		t.Fatalf("score = %d", v.Int32())
	}
	if v := mustLookup(t, out, "views"); v.Int32() != 1 {
		t.Fatalf("views = %d", v.Int32())
	}
}

func TestIncTypePromotionToDouble(t *testing.T) {
	before := doc(fInt("_id", 1), fInt("n", 3))
	upd := doc(fDoc("$inc", doc(fDbl("n", 0.5))))
	out, _ := applyOK(t, before, upd)
	v := mustLookup(t, out, "n")
	if v.Type != bson.TypeDouble || v.Double() != 3.5 {
		t.Fatalf("n = %v %v", v.Type, v.Double())
	}
}

func TestIncInt32Overflow(t *testing.T) {
	before := doc(fInt("_id", 1), fInt("n", 2_000_000_000))
	upd := doc(fDoc("$inc", doc(fInt("n", 2_000_000_000))))
	u, _ := Compile(upd)
	if _, _, err := u.Apply(before, epoch); err != ErrOverflow {
		t.Fatalf("err = %v, want ErrOverflow", err)
	}
}

func TestIncWidensToInt64(t *testing.T) {
	before := doc(fInt("_id", 1), fLong("n", 1))
	upd := doc(fDoc("$inc", doc(fInt("n", 2))))
	out, _ := applyOK(t, before, upd)
	v := mustLookup(t, out, "n")
	if v.Type != bson.TypeInt64 || v.Int64() != 3 {
		t.Fatalf("n = %v %d", v.Type, v.Int64())
	}
}

func TestIncNonNumericField(t *testing.T) {
	before := doc(fInt("_id", 1), fStr("n", "x"))
	upd := doc(fDoc("$inc", doc(fInt("n", 1))))
	u, _ := Compile(upd)
	if _, _, err := u.Apply(before, epoch); err != ErrNotNumeric {
		t.Fatalf("err = %v, want ErrNotNumeric", err)
	}
}

func TestMul(t *testing.T) {
	before := doc(fInt("_id", 1), fDbl("price", 10))
	upd := doc(fDoc("$mul", doc(fDbl("price", 1.1))))
	out, _ := applyOK(t, before, upd)
	if v := mustLookup(t, out, "price"); v.Double() != 11 {
		t.Fatalf("price = %v", v.Double())
	}
}

func TestMulMissingFieldIsZero(t *testing.T) {
	before := doc(fInt("_id", 1))
	upd := doc(fDoc("$mul", doc(fInt("n", 5))))
	out, _ := applyOK(t, before, upd)
	if v := mustLookup(t, out, "n"); v.Int32() != 0 {
		t.Fatalf("n = %d, want 0", v.Int32())
	}
}

// ---- $min / $max ---------------------------------------------------------

func TestMinApplies(t *testing.T) {
	before := doc(fInt("_id", 1), fInt("low", 80))
	out, mod := applyOK(t, before, doc(fDoc("$min", doc(fInt("low", 75)))))
	if !mod || mustLookup(t, out, "low").Int32() != 75 {
		t.Fatalf("low = %d", mustLookup(t, out, "low").Int32())
	}
}

func TestMinSkips(t *testing.T) {
	before := doc(fInt("_id", 1), fInt("low", 80))
	out, mod := applyOK(t, before, doc(fDoc("$min", doc(fInt("low", 85)))))
	if mod {
		t.Fatal("expected not modified")
	}
	wantDoc(t, out, before)
}

func TestMaxAcrossNumericTypes(t *testing.T) {
	before := doc(fInt("_id", 1), fInt("hi", 5))
	out, _ := applyOK(t, before, doc(fDoc("$max", doc(fDbl("hi", 5.5)))))
	if v := mustLookup(t, out, "hi"); v.Type != bson.TypeDouble || v.Double() != 5.5 {
		t.Fatalf("hi = %v %v", v.Type, v.Double())
	}
}

func TestMinMissingFieldSets(t *testing.T) {
	before := doc(fInt("_id", 1))
	out, _ := applyOK(t, before, doc(fDoc("$min", doc(fInt("low", 7)))))
	if v := mustLookup(t, out, "low"); v.Int32() != 7 {
		t.Fatalf("low = %d", v.Int32())
	}
}

// ---- $rename -------------------------------------------------------------

func TestRename(t *testing.T) {
	before := doc(fInt("_id", 1), fStr("fname", "alice"), fStr("lname", "smith"))
	upd := doc(fDoc("$rename", doc(fStr("fname", "firstName"), fStr("lname", "lastName"))))
	out, mod := applyOK(t, before, upd)
	if !mod {
		t.Fatal("expected modified")
	}
	if _, ok := out.Lookup("fname"); ok {
		t.Fatal("fname still present")
	}
	if v := mustLookup(t, out, "firstName"); v.StringValue() != "alice" {
		t.Fatalf("firstName = %q", v.StringValue())
	}
}

func TestRenameMissingSourceNoop(t *testing.T) {
	before := doc(fInt("_id", 1), fStr("a", "x"))
	out, mod := applyOK(t, before, doc(fDoc("$rename", doc(fStr("ghost", "b")))))
	if mod {
		t.Fatal("expected not modified")
	}
	wantDoc(t, out, before)
}

func TestRenameMovesSubdocument(t *testing.T) {
	before := doc(fInt("_id", 1), fDoc("addr", doc(fStr("city", "nyc"))))
	out, _ := applyOK(t, before, doc(fDoc("$rename", doc(fStr("addr", "location")))))
	if _, ok := out.Lookup("addr"); ok {
		t.Fatal("addr still present")
	}
	loc := lookup(t, out, "location").Document()
	if v := mustLookup(t, loc, "city"); v.StringValue() != "nyc" {
		t.Fatalf("location.city = %q", v.StringValue())
	}
}

// ---- $currentDate --------------------------------------------------------

func TestCurrentDate(t *testing.T) {
	before := doc(fInt("_id", 1))
	upd := doc(func(b *bson.Builder) {
		b.AppendDocument("$currentDate", doc(
			fBool("updatedAt", true),
			fDoc("ts", doc(fStr("$type", "timestamp"))),
		))
	})
	out, mod := applyOK(t, before, upd)
	if !mod {
		t.Fatal("expected modified")
	}
	if v := mustLookup(t, out, "updatedAt"); v.Type != bson.TypeDateTime || v.DateTime() != epoch.UnixMilli() {
		t.Fatalf("updatedAt = %v %d", v.Type, v.DateTime())
	}
	if v := mustLookup(t, out, "ts"); v.Type != bson.TypeTimestamp {
		t.Fatalf("ts type = %v", v.Type)
	}
}

// ---- compile errors ------------------------------------------------------

func TestCompileRejectsReplacement(t *testing.T) {
	if _, err := Compile(doc(fStr("name", "alice"))); err != ErrBadUpdate {
		t.Fatalf("err = %v, want ErrBadUpdate", err)
	}
}

func TestCompileRejectsMixedKeys(t *testing.T) {
	upd := doc(fDoc("$set", doc(fInt("a", 1))), fStr("b", "x"))
	if _, err := Compile(upd); err != ErrBadUpdate {
		t.Fatalf("err = %v, want ErrBadUpdate", err)
	}
}

func TestCompileRejectsUnknownOperator(t *testing.T) {
	upd := doc(fDoc("$frobnicate", doc(fInt("a", 1))))
	if _, err := Compile(upd); err != ErrBadUpdate {
		t.Fatalf("err = %v, want ErrBadUpdate", err)
	}
}

func TestCompileRejectsPathConflict(t *testing.T) {
	upd := doc(fDoc("$set", doc(fInt("a", 1))), fDoc("$inc", doc(fInt("a", 1))))
	if _, err := Compile(upd); err != ErrConflict {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestCompileRejectsPrefixConflict(t *testing.T) {
	upd := doc(fDoc("$set", doc(fInt("a", 1), fInt("a.b", 2))))
	if _, err := Compile(upd); err != ErrConflict {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestIsOperatorDoc(t *testing.T) {
	if !IsOperatorDoc(doc(fDoc("$set", doc(fInt("a", 1))))) {
		t.Fatal("operator doc not detected")
	}
	if IsOperatorDoc(doc(fStr("name", "x"))) {
		t.Fatal("replacement doc misdetected as operator")
	}
	if IsOperatorDoc(doc()) {
		t.Fatal("empty doc should not be operator form")
	}
}

func mustLookup(t *testing.T, d bson.Raw, key string) bson.RawValue {
	t.Helper()
	v, ok := d.Lookup(key)
	if !ok {
		t.Fatalf("field %q missing in %x", key, d)
	}
	return v
}
