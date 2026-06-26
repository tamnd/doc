package agg

import (
	"github.com/tamnd/doc/bson"
)

// missing is the absent value: a field path that resolves to no value, or an
// operator result that MongoDB drops. It is a zero RawValue (invalid type) and is
// distinct from BSON null in $group key handling and in $project field omission,
// but behaves like null in most expression operators (spec 2061 doc 12 §3.2).
var missing = bson.RawValue{}

// isMissing reports the absent value.
func isMissing(v bson.RawValue) bool { return v.Type == 0 }

// isNull reports an explicit BSON null.
func isNull(v bson.RawValue) bool { return v.Type == bson.TypeNull }

// isNullish reports null or missing, the falsy nullable values most operators
// propagate as null.
func isNullish(v bson.RawValue) bool { return isMissing(v) || isNull(v) }

// truthy applies aggregation truthiness: false, null, missing, and the deprecated
// undefined are falsy; every other value (including 0 and "") is truthy (spec 2061
// doc 12 §4.3).
func truthy(v bson.RawValue) bool {
	switch v.Type {
	case 0, bson.TypeNull, bson.TypeUndefined:
		return false
	case bson.TypeBoolean:
		return v.Boolean()
	default:
		return true
	}
}

// mkInt32, mkInt64, mkDouble, mkString, mkBool, mkNull, and mkDate build a
// standalone RawValue by appending under a scratch key and reading it back, the
// same construction the update package uses (update/numeric.go).
func mkInt32(v int32) bson.RawValue {
	rv, _ := bson.NewBuilder().AppendInt32("v", v).Build().Lookup("v")
	return rv
}

func mkInt64(v int64) bson.RawValue {
	rv, _ := bson.NewBuilder().AppendInt64("v", v).Build().Lookup("v")
	return rv
}

func mkDouble(v float64) bson.RawValue {
	rv, _ := bson.NewBuilder().AppendDouble("v", v).Build().Lookup("v")
	return rv
}

func mkString(v string) bson.RawValue {
	rv, _ := bson.NewBuilder().AppendString("v", v).Build().Lookup("v")
	return rv
}

func mkBool(v bool) bson.RawValue {
	rv, _ := bson.NewBuilder().AppendBoolean("v", v).Build().Lookup("v")
	return rv
}

func mkNull() bson.RawValue {
	rv, _ := bson.NewBuilder().AppendNull("v").Build().Lookup("v")
	return rv
}

func mkDate(msec int64) bson.RawValue {
	rv, _ := bson.NewBuilder().AppendDateTime("v", msec).Build().Lookup("v")
	return rv
}

// mkArray wraps element values into an array RawValue.
func mkArray(vals []bson.RawValue) bson.RawValue {
	rv, _ := bson.NewBuilder().AppendArray("v", bson.BuildArray(vals...)).Build().Lookup("v")
	return rv
}

// mkDoc wraps a document body into a document RawValue.
func mkDoc(d bson.Raw) bson.RawValue {
	rv, _ := bson.NewBuilder().AppendDocument("v", d).Build().Lookup("v")
	return rv
}

// numKind ranks numeric width for arithmetic result typing: int32 < int64 <
// double, matching MongoDB's widest-operand result rule (spec 2061 doc 12 §3.7).
type numKind int

const (
	kindInt32 numKind = iota
	kindInt64
	kindDouble
	kindNotNum
)

// numOf classifies a numeric value, returning its int64 and float64 views and its
// width. A non-numeric value reports kindNotNum.
func numOf(v bson.RawValue) (i int64, f float64, k numKind) {
	switch v.Type {
	case bson.TypeInt32:
		x := v.Int32()
		return int64(x), float64(x), kindInt32
	case bson.TypeInt64:
		x := v.Int64()
		return x, float64(x), kindInt64
	case bson.TypeDouble:
		return 0, v.Double(), kindDouble
	default:
		return 0, 0, kindNotNum
	}
}

// isNumber reports a numeric BSON type (int32, int64, or double; Decimal128 is not
// yet evaluated by the expression engine).
func isNumber(v bson.RawValue) bool {
	_, _, k := numOf(v)
	return k != kindNotNum
}

// widen returns the wider of two numeric kinds.
func widen(a, b numKind) numKind {
	if a > b {
		return a
	}
	return b
}

// mkNum builds a numeric result of the given width from an int64 and float64 view,
// promoting int64 to double when it does not fit the requested integer width
// (matching MongoDB's overflow-to-double rule, spec 2061 doc 12 §3.7).
func mkNum(i int64, f float64, k numKind) bson.RawValue {
	switch k {
	case kindInt32:
		if i >= -2147483648 && i <= 2147483647 {
			return mkInt32(int32(i))
		}
		return mkInt64(i)
	case kindInt64:
		return mkInt64(i)
	default:
		return mkDouble(f)
	}
}
