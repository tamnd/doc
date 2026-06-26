package agg

import (
	"encoding/hex"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/sys"
)

// typeName returns the MongoDB $type alias for a value, "missing" for the absent
// value (spec 2061 doc 12 §4.9).
func typeName(v bson.RawValue) string {
	switch v.Type {
	case 0:
		return "missing"
	case bson.TypeDouble:
		return "double"
	case bson.TypeString:
		return "string"
	case bson.TypeDocument:
		return "object"
	case bson.TypeArray:
		return "array"
	case bson.TypeBinary:
		return "binData"
	case bson.TypeUndefined:
		return "undefined"
	case bson.TypeObjectID:
		return "objectId"
	case bson.TypeBoolean:
		return "bool"
	case bson.TypeDateTime:
		return "date"
	case bson.TypeNull:
		return "null"
	case bson.TypeRegex:
		return "regex"
	case bson.TypeInt32:
		return "int"
	case bson.TypeTimestamp:
		return "timestamp"
	case bson.TypeInt64:
		return "long"
	case bson.TypeDecimal128:
		return "decimal"
	case bson.TypeMinKey:
		return "minKey"
	case bson.TypeMaxKey:
		return "maxKey"
	default:
		return "unknown"
	}
}

// opType returns the BSON type name of its single operand.
func opType(vals []bson.RawValue) bson.RawValue {
	return mkString(typeName(vals[0]))
}

// opIsNumber reports whether the operand is a numeric BSON type.
func opIsNumber(vals []bson.RawValue) bson.RawValue {
	return mkBool(isNumber(vals[0]))
}

// compileConvert compiles $convert {input, to, onError?, onNull?}.
func compileConvert(arg bson.RawValue) (Expr, error) {
	if arg.Type != bson.TypeDocument {
		return nil, ErrBadExpr
	}
	d := arg.Document()
	iv, ok1 := d.Lookup("input")
	tv, ok2 := d.Lookup("to")
	if !ok1 || !ok2 {
		return nil, ErrBadExpr
	}
	ine, err := compileExpr(iv)
	if err != nil {
		return nil, err
	}
	te, err := compileExpr(tv)
	if err != nil {
		return nil, err
	}
	e := convertExpr{input: ine, to: te}
	if ov, ok := d.Lookup("onError"); ok {
		e.onError, err = compileExpr(ov)
		if err != nil {
			return nil, err
		}
	}
	if ov, ok := d.Lookup("onNull"); ok {
		e.onNull, err = compileExpr(ov)
		if err != nil {
			return nil, err
		}
	}
	return e, nil
}

type convertExpr struct {
	input, to       Expr
	onError, onNull Expr
}

func (e convertExpr) eval(c *evalCtx) bson.RawValue {
	iv := e.input.eval(c)
	if isNullish(iv) {
		if e.onNull != nil {
			return e.onNull.eval(c)
		}
		return mkNull()
	}
	target, ok := targetType(e.to.eval(c))
	if !ok {
		return e.fail(c)
	}
	out, cok := convertTo(iv, target)
	if !cok {
		return e.fail(c)
	}
	return out
}

func (e convertExpr) fail(c *evalCtx) bson.RawValue {
	if e.onError != nil {
		return e.onError.eval(c)
	}
	return mkNull()
}

// targetType reads the "to" operand, accepting a type-name string or a numeric
// type code.
func targetType(v bson.RawValue) (bson.Type, bool) {
	if s, ok := strOf(v); ok {
		switch s {
		case "double":
			return bson.TypeDouble, true
		case "string":
			return bson.TypeString, true
		case "objectId":
			return bson.TypeObjectID, true
		case "bool":
			return bson.TypeBoolean, true
		case "date":
			return bson.TypeDateTime, true
		case "int":
			return bson.TypeInt32, true
		case "long":
			return bson.TypeInt64, true
		case "decimal":
			return bson.TypeDecimal128, true
		}
		return 0, false
	}
	if n, ok := intArg(v); ok {
		switch n {
		case 1:
			return bson.TypeDouble, true
		case 2:
			return bson.TypeString, true
		case 7:
			return bson.TypeObjectID, true
		case 8:
			return bson.TypeBoolean, true
		case 9:
			return bson.TypeDateTime, true
		case 16:
			return bson.TypeInt32, true
		case 18:
			return bson.TypeInt64, true
		case 19:
			return bson.TypeDecimal128, true
		}
	}
	return 0, false
}

// convertTo coerces a value to the target type following MongoDB conversion
// rules, returning false when the conversion is not allowed. Decimal128 is
// represented as a double until the engine carries a native decimal type.
func convertTo(v bson.RawValue, target bson.Type) (bson.RawValue, bool) {
	switch target {
	case bson.TypeString:
		return toStringValue(v)
	case bson.TypeBoolean:
		return toBoolValue(v)
	case bson.TypeInt32:
		return toIntValue(v, false)
	case bson.TypeInt64:
		return toIntValue(v, true)
	case bson.TypeDouble, bson.TypeDecimal128:
		return toDoubleValue(v)
	case bson.TypeDateTime:
		return toDateValue(v)
	case bson.TypeObjectID:
		return toObjectIDValue(v)
	default:
		return missing, false
	}
}

func toStringValue(v bson.RawValue) (bson.RawValue, bool) {
	switch v.Type {
	case bson.TypeString:
		return v, true
	case bson.TypeInt32:
		return mkString(strconv.FormatInt(int64(v.Int32()), 10)), true
	case bson.TypeInt64:
		return mkString(strconv.FormatInt(v.Int64(), 10)), true
	case bson.TypeDouble:
		return mkString(strconv.FormatFloat(v.Double(), 'g', -1, 64)), true
	case bson.TypeBoolean:
		return mkString(strconv.FormatBool(v.Boolean())), true
	case bson.TypeDateTime:
		t := time.UnixMilli(v.DateTime()).UTC()
		return mkString(t.Format("2006-01-02T15:04:05.000Z")), true
	case bson.TypeObjectID:
		return mkString(v.ObjectID().Hex()), true
	default:
		return missing, false
	}
}

func toBoolValue(v bson.RawValue) (bson.RawValue, bool) {
	switch v.Type {
	case bson.TypeBoolean:
		return v, true
	case bson.TypeInt32:
		return mkBool(v.Int32() != 0), true
	case bson.TypeInt64:
		return mkBool(v.Int64() != 0), true
	case bson.TypeDouble:
		return mkBool(v.Double() != 0), true
	case bson.TypeString, bson.TypeObjectID, bson.TypeDateTime:
		return mkBool(true), true
	default:
		return missing, false
	}
}

func toIntValue(v bson.RawValue, long bool) (bson.RawValue, bool) {
	var n int64
	switch v.Type {
	case bson.TypeInt32:
		n = int64(v.Int32())
	case bson.TypeInt64:
		n = v.Int64()
	case bson.TypeDouble:
		f := v.Double()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return missing, false
		}
		n = int64(f)
	case bson.TypeBoolean:
		if v.Boolean() {
			n = 1
		}
	case bson.TypeString:
		p, err := strconv.ParseInt(strings.TrimSpace(v.StringValue()), 10, 64)
		if err != nil {
			return missing, false
		}
		n = p
	case bson.TypeDateTime:
		n = v.DateTime()
	default:
		return missing, false
	}
	if long {
		return mkInt64(n), true
	}
	if n < math.MinInt32 || n > math.MaxInt32 {
		return missing, false
	}
	return mkInt32(int32(n)), true
}

func toDoubleValue(v bson.RawValue) (bson.RawValue, bool) {
	switch v.Type {
	case bson.TypeDouble:
		return v, true
	case bson.TypeInt32:
		return mkDouble(float64(v.Int32())), true
	case bson.TypeInt64:
		return mkDouble(float64(v.Int64())), true
	case bson.TypeBoolean:
		if v.Boolean() {
			return mkDouble(1), true
		}
		return mkDouble(0), true
	case bson.TypeString:
		f, err := strconv.ParseFloat(strings.TrimSpace(v.StringValue()), 64)
		if err != nil {
			return missing, false
		}
		return mkDouble(f), true
	case bson.TypeDateTime:
		return mkDouble(float64(v.DateTime())), true
	default:
		return missing, false
	}
}

func toDateValue(v bson.RawValue) (bson.RawValue, bool) {
	switch v.Type {
	case bson.TypeDateTime:
		return v, true
	case bson.TypeInt64:
		return mkDate(v.Int64()), true
	case bson.TypeInt32:
		return mkDate(int64(v.Int32())), true
	case bson.TypeDouble:
		return mkDate(int64(v.Double())), true
	case bson.TypeObjectID:
		return mkDate(int64(v.ObjectID().Timestamp()) * 1000), true
	case bson.TypeString:
		for _, layout := range isoLayouts {
			if t, err := time.Parse(layout, v.StringValue()); err == nil {
				return mkDate(t.UnixMilli()), true
			}
		}
		return missing, false
	default:
		return missing, false
	}
}

func toObjectIDValue(v bson.RawValue) (bson.RawValue, bool) {
	switch v.Type {
	case bson.TypeObjectID:
		return v, true
	case bson.TypeString:
		b, err := hex.DecodeString(v.StringValue())
		if err != nil || len(b) != 12 {
			return missing, false
		}
		var oid sys.ObjectID
		copy(oid[:], b)
		rv, _ := bson.NewBuilder().AppendObjectID("v", oid).Build().Lookup("v")
		return rv, true
	default:
		return missing, false
	}
}

// shorthandConvert builds $toInt, $toLong, $toDouble, $toDecimal, $toString,
// $toBool, $toDate, $toObjectId. A nullish input passes through as null; a failed
// conversion is an error surfaced as null (no onError branch in the shorthand).
func shorthandConvert(target bson.Type) opCompiler {
	return eager(1, 1, func(vals []bson.RawValue) bson.RawValue {
		if isNullish(vals[0]) {
			return mkNull()
		}
		out, ok := convertTo(vals[0], target)
		if !ok {
			return mkNull()
		}
		return out
	})
}
