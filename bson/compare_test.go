package bson

import (
	"math"
	"sort"
	"testing"

	"github.com/tamnd/doc/sys"
)

// val builds a single-element document and returns the value of its one field, a
// convenient way to mint a RawValue of any type for comparison tests.
func val(build func(b *Builder) *Builder) RawValue {
	d := build(NewBuilder()).Build()
	v, ok := d.Lookup("x")
	if !ok {
		panic("val: builder did not append field x")
	}
	return v
}

func i32(n int32) RawValue   { return val(func(b *Builder) *Builder { return b.AppendInt32("x", n) }) }
func i64(n int64) RawValue   { return val(func(b *Builder) *Builder { return b.AppendInt64("x", n) }) }
func dbl(f float64) RawValue { return val(func(b *Builder) *Builder { return b.AppendDouble("x", f) }) }
func str(s string) RawValue  { return val(func(b *Builder) *Builder { return b.AppendString("x", s) }) }
func boolean(t bool) RawValue {
	return val(func(b *Builder) *Builder { return b.AppendBoolean("x", t) })
}
func null() RawValue { return val(func(b *Builder) *Builder { return b.AppendNull("x") }) }

func TestCompareNumericAcrossTypes(t *testing.T) {
	cases := []struct {
		name string
		a, b RawValue
		want int
	}{
		{"int32 eq int64", i32(3), i64(3), 0},
		{"int32 eq double", i32(3), dbl(3.0), 0},
		{"int64 eq double", i64(3), dbl(3.0), 0},
		{"int32 lt double", i32(3), dbl(3.5), -1},
		{"double gt int32", dbl(3.5), i32(3), 1},
		{"neg int lt pos", i32(-1), i32(1), -1},
		{"double lt double", dbl(1.25), dbl(1.5), -1},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("%s: Compare = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestCompareInt64DoubleNoPrecisionLoss(t *testing.T) {
	// 2^53+1 is not representable as a float64; the int64 is strictly greater
	// than the double 2^53, and the comparison must not round them together.
	big := i64((1 << 53) + 1)
	d := dbl(float64(int64(1) << 53))
	if got := Compare(big, d); got != 1 {
		t.Fatalf("Compare(2^53+1 int64, 2^53 double) = %d, want 1", got)
	}
}

func TestCompareNaNSortsHighestAmongNumbers(t *testing.T) {
	nan := dbl(math.NaN())
	if got := Compare(nan, dbl(math.Inf(1))); got != 1 {
		t.Errorf("NaN vs +Inf = %d, want 1", got)
	}
	if got := Compare(dbl(1e300), nan); got != -1 {
		t.Errorf("1e300 vs NaN = %d, want -1", got)
	}
	if got := Compare(nan, nan); got != 0 {
		t.Errorf("NaN vs NaN = %d, want 0", got)
	}
}

func TestCompareTypeBracketOrder(t *testing.T) {
	// One value per rank, in ascending canonical-type order.
	oid := val(func(b *Builder) *Builder {
		var o sys.ObjectID
		return b.AppendObjectID("x", o)
	})
	date := val(func(b *Builder) *Builder { return b.AppendDateTime("x", 0) })
	ts := val(func(b *Builder) *Builder { return b.AppendTimestamp("x", 1) })
	ascending := []RawValue{
		null(),
		i32(5),
		str("a"),
		val(func(b *Builder) *Builder { return b.AppendDocument("x", NewBuilder().AppendInt32("a", 1).Build()) }),
		val(func(b *Builder) *Builder { return b.AppendArray("x", BuildArray(i32(1))) }),
		oid,
		boolean(false),
		date,
		ts,
	}
	for i := 0; i+1 < len(ascending); i++ {
		if got := Compare(ascending[i], ascending[i+1]); got != -1 {
			t.Errorf("rank %d vs %d: Compare = %d, want -1", i, i+1, got)
		}
	}
}

func TestCompareStrings(t *testing.T) {
	if Compare(str("apple"), str("banana")) != -1 {
		t.Error("apple should sort before banana")
	}
	// Binary collation: uppercase sorts before lowercase.
	if Compare(str("Z"), str("a")) != -1 {
		t.Error("uppercase Z should sort before lowercase a")
	}
}

func TestCompareBooleans(t *testing.T) {
	if Compare(boolean(false), boolean(true)) != -1 {
		t.Error("false should sort before true")
	}
}

func TestCompareArraysElementThenLength(t *testing.T) {
	a := val(func(b *Builder) *Builder { return b.AppendArray("x", BuildArray(i32(1), i32(2))) })
	b := val(func(bd *Builder) *Builder { return bd.AppendArray("x", BuildArray(i32(1), i32(3))) })
	if Compare(a, b) != -1 {
		t.Error("[1,2] should sort before [1,3]")
	}
	short := val(func(bd *Builder) *Builder { return bd.AppendArray("x", BuildArray(i32(1))) })
	if Compare(short, a) != -1 {
		t.Error("[1] should sort before [1,2]")
	}
}

func TestCompareSortsAMixedSlice(t *testing.T) {
	got := []RawValue{str("b"), i32(2), null(), boolean(true), i32(1), str("a")}
	sort.SliceStable(got, func(i, j int) bool { return Compare(got[i], got[j]) < 0 })
	want := []RawValue{null(), i32(1), i32(2), str("a"), str("b"), boolean(true)}
	for i := range want {
		if Compare(got[i], want[i]) != 0 {
			t.Fatalf("position %d: got type %s, want type %s", i, got[i].Type, want[i].Type)
		}
	}
}

func TestEqualSemantics(t *testing.T) {
	if !Equal(i32(7), i64(7)) {
		t.Error("int32 7 should equal int64 7")
	}
	if !Equal(dbl(math.NaN()), dbl(math.NaN())) {
		t.Error("NaN should equal NaN for query equality")
	}
	if Equal(i32(7), str("7")) {
		t.Error("int 7 should not equal string 7")
	}
}
