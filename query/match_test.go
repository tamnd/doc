package query

import (
	"math"
	"testing"

	"github.com/tamnd/doc/bson"
)

// doc builds a document from a sequence of field appenders.
func doc(fields ...func(*bson.Builder)) bson.Raw {
	b := bson.NewBuilder()
	for _, f := range fields {
		f(b)
	}
	return b.Build()
}

func fInt(k string, n int32) func(*bson.Builder) {
	return func(b *bson.Builder) { b.AppendInt32(k, n) }
}
func fStr(k, s string) func(*bson.Builder) { return func(b *bson.Builder) { b.AppendString(k, s) } }
func fDbl(k string, f float64) func(*bson.Builder) {
	return func(b *bson.Builder) { b.AppendDouble(k, f) }
}
func fBool(k string, t bool) func(*bson.Builder) {
	return func(b *bson.Builder) { b.AppendBoolean(k, t) }
}
func fNull(k string) func(*bson.Builder) { return func(b *bson.Builder) { b.AppendNull(k) } }
func fArr(k string, vs ...bson.RawValue) func(*bson.Builder) {
	return func(b *bson.Builder) { b.AppendArray(k, bson.BuildArray(vs...)) }
}
func fDoc(k string, sub bson.Raw) func(*bson.Builder) {
	return func(b *bson.Builder) { b.AppendDocument(k, sub) }
}

// rv mints a RawValue of a given type via a one-field document.
func rv(f func(*bson.Builder)) bson.RawValue {
	d := doc(f)
	elems, _ := d.Elements()
	return elems[0].Value
}

func vInt(n int32) bson.RawValue { return rv(fInt("x", n)) }

// must compiles a filter or fails the test.
func must(t *testing.T, filter bson.Raw) *Matcher {
	t.Helper()
	m, err := Compile(filter)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return m
}

func TestImplicitEquality(t *testing.T) {
	m := must(t, doc(fInt("a", 1)))
	if !m.Match(doc(fInt("a", 1), fStr("b", "x"))) {
		t.Error("{a:1} should match {a:1,b:x}")
	}
	if m.Match(doc(fInt("a", 2))) {
		t.Error("{a:1} should not match {a:2}")
	}
	// Cross-type numeric equality.
	if !m.Match(doc(fDbl("a", 1.0))) {
		t.Error("{a:1} should match {a:1.0}")
	}
}

func TestImplicitAnd(t *testing.T) {
	m := must(t, doc(fInt("a", 1), fStr("b", "x")))
	if !m.Match(doc(fInt("a", 1), fStr("b", "x"))) {
		t.Error("both fields present and equal should match")
	}
	if m.Match(doc(fInt("a", 1), fStr("b", "y"))) {
		t.Error("second field mismatch should fail the AND")
	}
}

func TestComparisonTypeBracket(t *testing.T) {
	m := must(t, doc(func(b *bson.Builder) {
		b.AppendDocument("a", doc(fInt("$gt", 5)))
	}))
	if !m.Match(doc(fInt("a", 6))) {
		t.Error("{a:{$gt:5}} should match {a:6}")
	}
	if m.Match(doc(fInt("a", 5))) {
		t.Error("$gt is strict")
	}
	// A string never satisfies a numeric $gt (different type bracket).
	if m.Match(doc(fStr("a", "zzz"))) {
		t.Error("{a:{$gt:5}} should not match a string")
	}
	// Missing never satisfies a range operator.
	if m.Match(doc(fInt("b", 1))) {
		t.Error("{a:{$gt:5}} should not match a missing field")
	}
}

func TestNullMatchesMissing(t *testing.T) {
	m := must(t, doc(fNull("a")))
	if !m.Match(doc(fNull("a"))) {
		t.Error("{a:null} should match stored null")
	}
	if !m.Match(doc(fInt("b", 1))) {
		t.Error("{a:null} should match a missing field")
	}
	if m.Match(doc(fInt("a", 1))) {
		t.Error("{a:null} should not match {a:1}")
	}
}

func TestNeExcludesNullAndMissing(t *testing.T) {
	m := must(t, doc(func(b *bson.Builder) { b.AppendDocument("a", doc(fNull("$ne"))) }))
	if m.Match(doc(fNull("a"))) {
		t.Error("{a:{$ne:null}} should exclude stored null")
	}
	if m.Match(doc(fInt("b", 1))) {
		t.Error("{a:{$ne:null}} should exclude a missing field")
	}
	if !m.Match(doc(fInt("a", 1))) {
		t.Error("{a:{$ne:null}} should match a present non-null value")
	}
}

func TestNeMatchesMissingForNonNull(t *testing.T) {
	m := must(t, doc(func(b *bson.Builder) { b.AppendDocument("a", doc(fInt("$ne", 5))) }))
	if !m.Match(doc(fInt("b", 1))) {
		t.Error("{a:{$ne:5}} should match a missing field")
	}
	if m.Match(doc(fInt("a", 5))) {
		t.Error("{a:{$ne:5}} should exclude {a:5}")
	}
}

func TestExistsDistinguishesNullFromMissing(t *testing.T) {
	exists := must(t, doc(func(b *bson.Builder) { b.AppendDocument("a", doc(fBool("$exists", true))) }))
	if !exists.Match(doc(fNull("a"))) {
		t.Error("$exists:true should match a present null")
	}
	if exists.Match(doc(fInt("b", 1))) {
		t.Error("$exists:true should not match a missing field")
	}
	notExists := must(t, doc(func(b *bson.Builder) { b.AppendDocument("a", doc(fBool("$exists", false))) }))
	if !notExists.Match(doc(fInt("b", 1))) {
		t.Error("$exists:false should match a missing field")
	}
	if notExists.Match(doc(fNull("a"))) {
		t.Error("$exists:false should not match a present null")
	}
}

func TestInAndNin(t *testing.T) {
	in := must(t, doc(func(b *bson.Builder) {
		b.AppendDocument("a", doc(func(bd *bson.Builder) { bd.AppendArray("$in", bson.BuildArray(vInt(1), vInt(2), vInt(3))) }))
	}))
	if !in.Match(doc(fInt("a", 2))) {
		t.Error("$in should match a listed value")
	}
	if in.Match(doc(fInt("a", 9))) {
		t.Error("$in should not match an unlisted value")
	}
	// $in fans out over array fields.
	if !in.Match(doc(fArr("a", vInt(9), vInt(3)))) {
		t.Error("$in should match when an array element is listed")
	}
}

func TestInWithNullMatchesMissing(t *testing.T) {
	m := must(t, doc(func(b *bson.Builder) {
		b.AppendDocument("a", doc(func(bd *bson.Builder) {
			bd.AppendArray("$in", bson.BuildArray(vInt(1), rv(fNull("0"))))
		}))
	}))
	if !m.Match(doc(fInt("b", 1))) {
		t.Error("$in containing null should match a missing field")
	}
}

func TestLogicalOrAndNor(t *testing.T) {
	or := must(t, doc(func(b *bson.Builder) {
		b.AppendArray("$or", bson.BuildArray(
			rv(fDoc("0", doc(fInt("a", 1)))),
			rv(fDoc("0", doc(fInt("b", 2)))),
		))
	}))
	if !or.Match(doc(fInt("b", 2))) {
		t.Error("$or should match when one branch matches")
	}
	if or.Match(doc(fInt("c", 3))) {
		t.Error("$or should fail when no branch matches")
	}
	nor := must(t, doc(func(b *bson.Builder) {
		b.AppendArray("$nor", bson.BuildArray(
			rv(fDoc("0", doc(fInt("a", 1)))),
		))
	}))
	if !nor.Match(doc(fInt("a", 2))) {
		t.Error("$nor should match when the branch does not")
	}
	if nor.Match(doc(fInt("a", 1))) {
		t.Error("$nor should fail when the branch matches")
	}
}

func TestNot(t *testing.T) {
	m := must(t, doc(func(b *bson.Builder) {
		b.AppendDocument("a", doc(func(bd *bson.Builder) { bd.AppendDocument("$not", doc(fInt("$gt", 5))) }))
	}))
	if !m.Match(doc(fInt("a", 3))) {
		t.Error("{a:{$not:{$gt:5}}} should match {a:3}")
	}
	if m.Match(doc(fInt("a", 9))) {
		t.Error("{a:{$not:{$gt:5}}} should not match {a:9}")
	}
	// $not also matches a missing field (the negated predicate is false there).
	if !m.Match(doc(fInt("b", 1))) {
		t.Error("{a:{$not:{$gt:5}}} should match a missing field")
	}
}

func TestDottedPathAndArrayFanOut(t *testing.T) {
	d := doc(fDoc("a", doc(fInt("b", 7))))
	m := must(t, doc(fInt("a.b", 7)))
	if !m.Match(d) {
		t.Error("dotted path should resolve nested field")
	}
	// Array of subdocuments: a.b fans out across elements.
	arr := doc(fArr("a",
		rv(fDoc("0", doc(fInt("b", 1)))),
		rv(fDoc("0", doc(fInt("b", 2)))),
	))
	if !must(t, doc(fInt("a.b", 2))).Match(arr) {
		t.Error("a.b should match an element's b in an array of docs")
	}
}

func TestArrayElementEquality(t *testing.T) {
	d := doc(fArr("a", vInt(1), vInt(2), vInt(3)))
	if !must(t, doc(fInt("a", 2))).Match(d) {
		t.Error("{a:2} should match an array containing 2")
	}
	// Whole-array equality.
	if !must(t, doc(fArr("a", vInt(1), vInt(2), vInt(3)))).Match(d) {
		t.Error("whole-array equality should match")
	}
	if must(t, doc(fArr("a", vInt(1), vInt(2)))).Match(d) {
		t.Error("whole-array equality should be exact")
	}
}

func TestSize(t *testing.T) {
	d := doc(fArr("a", vInt(1), vInt(2)))
	if !must(t, doc(func(b *bson.Builder) { b.AppendDocument("a", doc(fInt("$size", 2))) })).Match(d) {
		t.Error("$size:2 should match a 2-element array")
	}
	if must(t, doc(func(b *bson.Builder) { b.AppendDocument("a", doc(fInt("$size", 3))) })).Match(d) {
		t.Error("$size:3 should not match a 2-element array")
	}
}

func TestAll(t *testing.T) {
	d := doc(fArr("a", vInt(1), vInt(2), vInt(3)))
	m := must(t, doc(func(b *bson.Builder) {
		b.AppendDocument("a", doc(func(bd *bson.Builder) { bd.AppendArray("$all", bson.BuildArray(vInt(2), vInt(3))) }))
	}))
	if !m.Match(d) {
		t.Error("$all should match when every operand is present")
	}
	missing := must(t, doc(func(b *bson.Builder) {
		b.AppendDocument("a", doc(func(bd *bson.Builder) { bd.AppendArray("$all", bson.BuildArray(vInt(2), vInt(9))) }))
	}))
	if missing.Match(d) {
		t.Error("$all should fail when an operand is absent")
	}
}

func TestElemMatchOperatorForm(t *testing.T) {
	d := doc(fArr("a", vInt(3), vInt(7), vInt(20)))
	m := must(t, doc(func(b *bson.Builder) {
		b.AppendDocument("a", doc(func(bd *bson.Builder) {
			bd.AppendDocument("$elemMatch", doc(fInt("$gt", 5), fInt("$lt", 10)))
		}))
	}))
	if !m.Match(d) {
		t.Error("$elemMatch should match when one element satisfies all operators (7)")
	}
	none := doc(fArr("a", vInt(3), vInt(20)))
	if m.Match(none) {
		t.Error("$elemMatch should fail when no single element satisfies all operators")
	}
}

func TestElemMatchCriteriaForm(t *testing.T) {
	d := doc(fArr("a",
		rv(fDoc("0", doc(fInt("x", 1), fInt("y", 1)))),
		rv(fDoc("0", doc(fInt("x", 2), fInt("y", 9)))),
	))
	m := must(t, doc(func(b *bson.Builder) {
		b.AppendDocument("a", doc(func(bd *bson.Builder) {
			bd.AppendDocument("$elemMatch", doc(fInt("x", 2), fInt("y", 9)))
		}))
	}))
	if !m.Match(d) {
		t.Error("$elemMatch criteria form should match the second element")
	}
	cross := must(t, doc(func(b *bson.Builder) {
		b.AppendDocument("a", doc(func(bd *bson.Builder) {
			bd.AppendDocument("$elemMatch", doc(fInt("x", 1), fInt("y", 9)))
		}))
	}))
	if cross.Match(d) {
		t.Error("$elemMatch should require one element to satisfy all criteria, not a mix")
	}
}

func TestType(t *testing.T) {
	tInt := must(t, doc(func(b *bson.Builder) { b.AppendDocument("a", doc(fStr("$type", "int"))) }))
	if !tInt.Match(doc(fInt("a", 1))) {
		t.Error("$type:int should match an int32")
	}
	if tInt.Match(doc(fStr("a", "x"))) {
		t.Error("$type:int should not match a string")
	}
	tNum := must(t, doc(func(b *bson.Builder) { b.AppendDocument("a", doc(fStr("$type", "number"))) }))
	if !tNum.Match(doc(fDbl("a", 1.5))) {
		t.Error("$type:number should match a double")
	}
}

func TestNaNEqualsNaN(t *testing.T) {
	m := must(t, doc(fDbl("a", math.NaN())))
	if !m.Match(doc(fDbl("a", math.NaN()))) {
		t.Error("{a:NaN} should match a stored NaN")
	}
}

func TestEmptyFilterMatchesAll(t *testing.T) {
	m := must(t, bson.NewBuilder().Build())
	if !m.Match(doc(fInt("a", 1))) {
		t.Error("empty filter should match any document")
	}
}

func TestUnknownOperatorErrors(t *testing.T) {
	_, err := Compile(doc(func(b *bson.Builder) { b.AppendDocument("a", doc(fInt("$bogus", 1))) }))
	if err == nil {
		t.Error("an unknown operator should be a compile error")
	}
}
