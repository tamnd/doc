package agg

import (
	"testing"

	"github.com/tamnd/doc/bson"
)

// op builds an operator expression {name: [args...]}.
func op(name string, args ...bson.RawValue) bson.RawValue {
	return mkDoc(bson.NewBuilder().AppendArray(name, bson.BuildArray(args...)).Build())
}

// opv builds an operator expression {name: v} with a single non-array argument.
func opv(name string, v bson.RawValue) bson.RawValue {
	return mkDoc(bson.NewBuilder().AppendValue(name, v).Build())
}

// opd builds an operator expression {name: <document>}.
func opd(name string, body bson.Raw) bson.RawValue {
	return mkDoc(bson.NewBuilder().AppendDocument(name, body).Build())
}

// evalC compiles and evaluates an expression against root.
func evalC(t *testing.T, expr bson.RawValue, root bson.Raw) bson.RawValue {
	t.Helper()
	e, err := compileExpr(expr)
	if err != nil {
		t.Fatalf("compileExpr: %v", err)
	}
	return e.eval(docCtx(root, &execCtx{}))
}

// evalK evaluates against an empty document.
func evalK(t *testing.T, expr bson.RawValue) bson.RawValue {
	t.Helper()
	return evalC(t, expr, bson.NewBuilder().Build())
}

func wantEqual(t *testing.T, got, want bson.RawValue) {
	t.Helper()
	if !bson.Equal(got, want) {
		t.Fatalf("got %v (type %#x), want %v (type %#x)", got, got.Type, want, want.Type)
	}
}

func TestArithmetic(t *testing.T) {
	wantEqual(t, evalK(t, op("$add", mkInt32(2), mkInt32(3))), mkInt32(5))
	wantEqual(t, evalK(t, op("$add", mkInt32(2), mkDouble(0.5))), mkDouble(2.5))
	wantEqual(t, evalK(t, op("$subtract", mkInt64(10), mkInt32(3))), mkInt64(7))
	wantEqual(t, evalK(t, op("$multiply", mkInt32(4), mkInt32(5))), mkInt32(20))
	wantEqual(t, evalK(t, op("$divide", mkInt32(9), mkInt32(2))), mkDouble(4.5))
	wantEqual(t, evalK(t, op("$divide", mkInt32(1), mkInt32(0))), mkNull())
	wantEqual(t, evalK(t, op("$mod", mkInt32(10), mkInt32(3))), mkInt32(1))
	wantEqual(t, evalK(t, opv("$abs", mkInt32(-7))), mkInt32(7))
	wantEqual(t, evalK(t, opv("$ceil", mkDouble(1.2))), mkDouble(2))
	wantEqual(t, evalK(t, opv("$floor", mkDouble(1.8))), mkDouble(1))
	wantEqual(t, evalK(t, op("$pow", mkInt32(2), mkInt32(10))), mkDouble(1024))
	wantEqual(t, evalK(t, opv("$sqrt", mkDouble(16))), mkDouble(4))
}

func TestAddOverflowWidens(t *testing.T) {
	// int32 max + 1 must widen to int64, not wrap.
	got := evalK(t, op("$add", mkInt32(2147483647), mkInt32(1)))
	wantEqual(t, got, mkInt64(2147483648))
}

func TestComparison(t *testing.T) {
	wantEqual(t, evalK(t, op("$eq", mkInt32(1), mkInt32(1))), mkBool(true))
	wantEqual(t, evalK(t, op("$eq", mkInt32(1), mkDouble(1))), mkBool(true)) // cross-type numeric
	wantEqual(t, evalK(t, op("$gt", mkInt32(2), mkInt32(1))), mkBool(true))
	wantEqual(t, evalK(t, op("$lte", mkInt32(2), mkInt32(2))), mkBool(true))
	wantEqual(t, evalK(t, op("$cmp", mkInt32(3), mkInt32(5))), mkInt32(-1))
	// Missing compares as null, below numbers.
	wantEqual(t, evalC(t, op("$lt", mkString("$missing"), mkInt32(0)), bson.NewBuilder().Build()), mkBool(true))
}

func TestBooleanAndConditional(t *testing.T) {
	wantEqual(t, evalK(t, op("$and", mkBool(true), mkInt32(1))), mkBool(true))
	wantEqual(t, evalK(t, op("$and", mkBool(true), mkNull())), mkBool(false))
	wantEqual(t, evalK(t, op("$or", mkBool(false), mkInt32(0))), mkBool(true)) // 0 is truthy
	wantEqual(t, evalK(t, opv("$not", mkBool(false))), mkBool(true))

	cond := opd("$cond", bson.NewBuilder().
		AppendBoolean("if", true).
		AppendString("then", "yes").
		AppendString("else", "no").Build())
	wantEqual(t, evalK(t, cond), mkString("yes"))

	wantEqual(t, evalK(t, op("$ifNull", mkNull(), mkString("fallback"))), mkString("fallback"))
}

func TestSwitch(t *testing.T) {
	branch := mkDoc(bson.NewBuilder().
		AppendBoolean("case", false).AppendString("then", "a").Build())
	branch2 := mkDoc(bson.NewBuilder().
		AppendBoolean("case", true).AppendString("then", "b").Build())
	sw := opd("$switch", bson.NewBuilder().
		AppendArray("branches", bson.BuildArray(branch, branch2)).
		AppendString("default", "d").Build())
	wantEqual(t, evalK(t, sw), mkString("b"))
}

func TestStringOps(t *testing.T) {
	wantEqual(t, evalK(t, op("$concat", mkString("a"), mkString("b"), mkString("c"))), mkString("abc"))
	wantEqual(t, evalK(t, op("$concat", mkString("a"), mkNull())), mkNull())
	wantEqual(t, evalK(t, opv("$toUpper", mkString("hi"))), mkString("HI"))
	wantEqual(t, evalK(t, opv("$toLower", mkString("HI"))), mkString("hi"))
	wantEqual(t, evalK(t, opv("$strLenBytes", mkString("hello"))), mkInt32(5))
	wantEqual(t, evalK(t, op("$substrBytes", mkString("hello"), mkInt32(1), mkInt32(3))), mkString("ell"))
	wantEqual(t, evalK(t, op("$indexOfBytes", mkString("hello"), mkString("l"))), mkInt32(2))

	split := evalK(t, op("$split", mkString("a,b,c"), mkString(",")))
	wantEqual(t, split, mkArray([]bson.RawValue{mkString("a"), mkString("b"), mkString("c")}))

	trim := opd("$trim", bson.NewBuilder().AppendString("input", "  x  ").Build())
	wantEqual(t, evalK(t, trim), mkString("x"))

	repl := opd("$replaceAll", bson.NewBuilder().
		AppendString("input", "a-b-c").AppendString("find", "-").AppendString("replacement", "_").Build())
	wantEqual(t, evalK(t, repl), mkString("a_b_c"))
}

func TestRegex(t *testing.T) {
	rm := opd("$regexMatch", bson.NewBuilder().
		AppendString("input", "Hello").AppendString("regex", "^h").AppendString("options", "i").Build())
	wantEqual(t, evalK(t, rm), mkBool(true))
}

func TestArrayOps(t *testing.T) {
	arr := mkArray([]bson.RawValue{mkInt32(10), mkInt32(20), mkInt32(30)})
	// A single-argument operator counts the elements of a bare array literal as
	// arguments, so the array is referenced through a field path, as in MongoDB.
	root := bson.NewBuilder().AppendValue("arr", arr).Build()
	ref := mkString("$arr")
	wantEqual(t, evalC(t, opv("$size", ref), root), mkInt32(3))
	wantEqual(t, evalC(t, op("$arrayElemAt", ref, mkInt32(1)), root), mkInt32(20))
	wantEqual(t, evalC(t, op("$arrayElemAt", ref, mkInt32(-1)), root), mkInt32(30))
	wantEqual(t, evalC(t, opv("$first", ref), root), mkInt32(10))
	wantEqual(t, evalC(t, opv("$last", ref), root), mkInt32(30))
	wantEqual(t, evalC(t, op("$in", mkInt32(20), ref), root), mkBool(true))
	wantEqual(t, evalC(t, op("$indexOfArray", ref, mkInt32(30)), root), mkInt32(2))
	wantEqual(t, evalC(t, opv("$isArray", ref), root), mkBool(true))

	rev := evalC(t, opv("$reverseArray", ref), root)
	wantEqual(t, rev, mkArray([]bson.RawValue{mkInt32(30), mkInt32(20), mkInt32(10)}))

	rng := evalK(t, op("$range", mkInt32(0), mkInt32(3)))
	wantEqual(t, rng, mkArray([]bson.RawValue{mkInt32(0), mkInt32(1), mkInt32(2)}))
}

func TestMapFilterReduce(t *testing.T) {
	arr := mkArray([]bson.RawValue{mkInt32(1), mkInt32(2), mkInt32(3), mkInt32(4)})

	flt := opd("$filter", bson.NewBuilder().
		AppendValue("input", arr).
		AppendString("as", "n").
		AppendValue("cond", op("$gt", mkString("$$n"), mkInt32(2))).Build())
	wantEqual(t, evalK(t, flt), mkArray([]bson.RawValue{mkInt32(3), mkInt32(4)}))

	mp := opd("$map", bson.NewBuilder().
		AppendValue("input", arr).
		AppendString("as", "n").
		AppendValue("in", op("$multiply", mkString("$$n"), mkInt32(10))).Build())
	wantEqual(t, evalK(t, mp), mkArray([]bson.RawValue{mkInt32(10), mkInt32(20), mkInt32(30), mkInt32(40)}))

	red := opd("$reduce", bson.NewBuilder().
		AppendValue("input", arr).
		AppendInt32("initialValue", 0).
		AppendValue("in", op("$add", mkString("$$value"), mkString("$$this"))).Build())
	wantEqual(t, evalK(t, red), mkInt32(10))
}

func TestSetOps(t *testing.T) {
	a := mkArray([]bson.RawValue{mkInt32(1), mkInt32(2), mkInt32(3)})
	b := mkArray([]bson.RawValue{mkInt32(2), mkInt32(3), mkInt32(4)})
	wantEqual(t, evalK(t, op("$setIsSubset", mkArray([]bson.RawValue{mkInt32(2)}), a)), mkBool(true))
	inter := evalK(t, op("$setIntersection", a, b))
	wantEqual(t, inter, mkArray([]bson.RawValue{mkInt32(2), mkInt32(3)}))
	diff := evalK(t, op("$setDifference", a, b))
	wantEqual(t, diff, mkArray([]bson.RawValue{mkInt32(1)}))
	wantEqual(t, evalK(t, op("$setEquals", a, a)), mkBool(true))
}

func TestTypeOps(t *testing.T) {
	wantEqual(t, evalK(t, opv("$type", mkInt32(1))), mkString("int"))
	wantEqual(t, evalK(t, opv("$type", mkString("x"))), mkString("string"))
	wantEqual(t, evalC(t, opv("$type", mkString("$nope")), bson.NewBuilder().Build()), mkString("missing"))
	wantEqual(t, evalK(t, opv("$isNumber", mkInt64(5))), mkBool(true))
	wantEqual(t, evalK(t, opv("$toInt", mkString("42"))), mkInt32(42))
	wantEqual(t, evalK(t, opv("$toString", mkInt32(7))), mkString("7"))
	wantEqual(t, evalK(t, opv("$toBool", mkInt32(0))), mkBool(false))
}

func TestDateOps(t *testing.T) {
	// 2021-01-15T00:00:00Z = 1610668800000 ms.
	const ms = 1610668800000
	d := mkDate(ms)
	wantEqual(t, evalK(t, opv("$year", d)), mkInt32(2021))
	wantEqual(t, evalK(t, opv("$month", d)), mkInt32(1))
	wantEqual(t, evalK(t, opv("$dayOfMonth", d)), mkInt32(15))

	dts := opd("$dateToString", bson.NewBuilder().
		AppendValue("date", d).AppendString("format", "%Y-%m-%d").Build())
	wantEqual(t, evalK(t, dts), mkString("2021-01-15"))

	add := opd("$dateAdd", bson.NewBuilder().
		AppendValue("startDate", d).AppendString("unit", "day").AppendInt32("amount", 1).Build())
	wantEqual(t, evalK(t, add), mkDate(ms+86400000))

	diff := opd("$dateDiff", bson.NewBuilder().
		AppendValue("startDate", d).AppendValue("endDate", mkDate(ms+86400000*3)).AppendString("unit", "day").Build())
	wantEqual(t, evalK(t, diff), mkInt64(3))
}

func TestMiscOps(t *testing.T) {
	lit := opv("$literal", mkInt32(1))
	wantEqual(t, evalK(t, lit), mkInt32(1))

	merge := op("$mergeObjects",
		mkDoc(bson.NewBuilder().AppendInt32("a", 1).Build()),
		mkDoc(bson.NewBuilder().AppendInt32("b", 2).Build()))
	got := evalK(t, merge)
	if got.Type != bson.TypeDocument {
		t.Fatalf("mergeObjects type = %#x", got.Type)
	}
	if v, _ := got.Document().Lookup("a"); v.Int32() != 1 {
		t.Fatalf("merged a = %d", v.Int32())
	}
	if v, _ := got.Document().Lookup("b"); v.Int32() != 2 {
		t.Fatalf("merged b = %d", v.Int32())
	}
}

func TestFieldPathAndVars(t *testing.T) {
	root := bson.NewBuilder().
		AppendInt32("x", 5).
		AppendDocument("sub", bson.NewBuilder().AppendString("y", "deep").Build()).
		Build()
	wantEqual(t, evalC(t, mkString("$x"), root), mkInt32(5))
	wantEqual(t, evalC(t, mkString("$sub.y"), root), mkString("deep"))
	wantEqual(t, evalC(t, mkString("$$ROOT.x"), root), mkInt32(5))

	let := opd("$let", bson.NewBuilder().
		AppendDocument("vars", bson.NewBuilder().AppendValue("d", op("$add", mkString("$x"), mkInt32(1))).Build()).
		AppendValue("in", op("$multiply", mkString("$$d"), mkInt32(2))).Build())
	wantEqual(t, evalC(t, let, root), mkInt32(12))
}
