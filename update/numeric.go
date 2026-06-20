package update

import (
	"math"
	"time"

	"github.com/tamnd/doc/bson"
)

// num is a numeric value decoded for arithmetic: kind is one of TypeInt32,
// TypeInt64, or TypeDouble; i carries the integer value for the int kinds and f
// the float value for the double kind.
type num struct {
	kind bson.Type
	i    int64
	f    float64
}

// toNum decodes a numeric RawValue. Decimal128 is numeric but not yet supported
// for arithmetic, so it reports ok=false (the caller raises ErrNotNumeric).
func toNum(v bson.RawValue) (num, bool) {
	switch v.Type {
	case bson.TypeInt32:
		return num{kind: bson.TypeInt32, i: int64(v.Int32())}, true
	case bson.TypeInt64:
		return num{kind: bson.TypeInt64, i: v.Int64()}, true
	case bson.TypeDouble:
		return num{kind: bson.TypeDouble, f: v.Double()}, true
	default:
		return num{}, false
	}
}

// asFloat returns the value of n as a float64 regardless of kind.
func (n num) asFloat() float64 {
	if n.kind == bson.TypeDouble {
		return n.f
	}
	return float64(n.i)
}

// resultDouble reports whether a result combining a and b is a double: any double
// operand makes the result a double (spec 2061 doc 13 §5.4).
func resultDouble(a, b num) bool {
	return a.kind == bson.TypeDouble || b.kind == bson.TypeDouble
}

// resultInt64 reports whether an integer result is int64 rather than int32: any
// int64 operand widens the result.
func resultInt64(a, b num) bool {
	return a.kind == bson.TypeInt64 || b.kind == bson.TypeInt64
}

// addNumeric returns cur + arg with MongoDB's $inc type-promotion rules.
func addNumeric(cur, arg bson.RawValue) (bson.RawValue, error) {
	return arith(cur, arg, func(x, y int64) int64 { return x + y }, func(x, y float64) float64 { return x + y })
}

// mulNumeric returns cur * arg with MongoDB's $mul type-promotion rules.
func mulNumeric(cur, arg bson.RawValue) (bson.RawValue, error) {
	return arith(cur, arg, func(x, y int64) int64 { return x * y }, func(x, y float64) float64 { return x * y })
}

// arith combines two numeric values, choosing the result type from the operand
// types and checking int32 overflow.
func arith(cur, arg bson.RawValue, iop func(x, y int64) int64, fop func(x, y float64) float64) (bson.RawValue, error) {
	a, ok1 := toNum(cur)
	b, ok2 := toNum(arg)
	if !ok1 || !ok2 {
		return bson.RawValue{}, ErrNotNumeric
	}
	if resultDouble(a, b) {
		return doubleValue(fop(a.asFloat(), b.asFloat())), nil
	}
	r := iop(a.i, b.i)
	if resultInt64(a, b) {
		return int64Value(r), nil
	}
	if r < math.MinInt32 || r > math.MaxInt32 {
		return bson.RawValue{}, ErrOverflow
	}
	return int32Value(int32(r)), nil
}

// zeroLike returns the zero of arg's numeric type, the base for an arithmetic
// operator applied to a missing field.
func zeroLike(arg bson.RawValue) bson.RawValue {
	switch arg.Type {
	case bson.TypeInt64:
		return int64Value(0)
	case bson.TypeDouble:
		return doubleValue(0)
	default:
		return int32Value(0)
	}
}

// The value builders frame a single typed value as a RawValue. Each builds a
// one-field document and reads the value back; the returned RawValue aliases that
// fresh buffer, which the surrounding node keeps alive.

func int32Value(v int32) bson.RawValue {
	d := bson.NewBuilder().AppendInt32("v", v).Build()
	rv, _ := d.Lookup("v")
	return rv
}

func int64Value(v int64) bson.RawValue {
	d := bson.NewBuilder().AppendInt64("v", v).Build()
	rv, _ := d.Lookup("v")
	return rv
}

func doubleValue(v float64) bson.RawValue {
	d := bson.NewBuilder().AppendDouble("v", v).Build()
	rv, _ := d.Lookup("v")
	return rv
}

func nullValue() bson.RawValue {
	d := bson.NewBuilder().AppendNull("v").Build()
	rv, _ := d.Lookup("v")
	return rv
}

func dateValue(now time.Time) bson.RawValue {
	d := bson.NewBuilder().AppendDateTime("v", now.UnixMilli()).Build()
	rv, _ := d.Lookup("v")
	return rv
}

// timestampValue builds a BSON Timestamp from now: the high 32 bits are the Unix
// seconds, the low 32 bits an ordinal (0 here, since one update reads now once).
func timestampValue(now time.Time) bson.RawValue {
	ts := uint64(now.Unix()) << 32
	d := bson.NewBuilder().AppendTimestamp("v", ts).Build()
	rv, _ := d.Lookup("v")
	return rv
}
