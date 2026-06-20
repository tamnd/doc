package bson

import (
	"bytes"
	"math"
	"math/big"
)

// Compare is the total order over BSON values that MongoDB uses for sorting, for
// the range query operators, and for index key ordering (spec 2061 doc 02 §7).
// It returns -1 if a sorts before b, +1 if after, and 0 if they compare equal.
//
// Values of different canonical types are ordered by their type rank; the four
// numeric types share one rank and compare by numeric value, so an int32, an
// int64, and a double of the same magnitude compare equal. Within a type the
// rules are MongoDB's: strings by raw UTF-8 bytes, documents field-by-field,
// arrays element-by-element then length, and so on (§7.3 through §7.8).
func Compare(a, b RawValue) int {
	ra, rb := CanonicalType(a.Type), CanonicalType(b.Type)
	if ra != rb {
		if ra < rb {
			return -1
		}
		return 1
	}
	switch ra {
	case rankNull, rankMinKey, rankMaxKey:
		return 0 // type rank carries the whole order; no payload to compare
	case rankNumber:
		return compareNumbers(a, b)
	case rankString:
		return cmpString(a.StringValue(), b.StringValue())
	case rankObject:
		return compareDocuments(a.Document(), b.Document())
	case rankArray:
		return compareArrays(a.Document(), b.Document())
	case rankBinary:
		return compareBinary(a, b)
	case rankObjectID:
		oa, ob := a.ObjectID(), b.ObjectID()
		return bytes.Compare(oa[:], ob[:])
	case rankBool:
		return cmpBool(a.Boolean(), b.Boolean())
	case rankDate:
		return cmpInt64(a.DateTime(), b.DateTime())
	case rankTimestamp:
		return cmpUint64(a.Timestamp(), b.Timestamp())
	case rankRegex:
		return compareRegex(a, b)
	default:
		return 0
	}
}

// Equal reports whether two values are equal under MongoDB's query equality,
// which is Compare returning 0: numeric types compare by value across int and
// double, and (unlike IEEE 754) a NaN equals a NaN, so a {x: NaN} filter matches
// a stored NaN (spec 2061 doc 02 §7.3, doc 08 §3.2).
func Equal(a, b RawValue) bool { return Compare(a, b) == 0 }

// Canonical type ranks (spec 2061 doc 02 §7.2). The four numeric types and the
// two null-like types each collapse to one rank.
const (
	rankMinKey = iota
	rankNull
	rankNumber
	rankString
	rankObject
	rankArray
	rankBinary
	rankObjectID
	rankBool
	rankDate
	rankTimestamp
	rankRegex
	rankMaxKey
)

// CanonicalType maps a BSON type to its comparison rank, collapsing the numeric
// types into one rank and the null-like types (null, undefined, missing) into
// another (spec 2061 doc 02 §7.2).
func CanonicalType(t Type) int {
	switch t {
	case TypeMinKey:
		return rankMinKey
	case TypeNull, TypeUndefined:
		return rankNull
	case TypeDouble, TypeInt32, TypeInt64, TypeDecimal128:
		return rankNumber
	case TypeString, TypeSymbol, TypeJavaScript:
		return rankString
	case TypeDocument:
		return rankObject
	case TypeArray:
		return rankArray
	case TypeBinary:
		return rankBinary
	case TypeObjectID:
		return rankObjectID
	case TypeBoolean:
		return rankBool
	case TypeDateTime:
		return rankDate
	case TypeTimestamp:
		return rankTimestamp
	case TypeRegex:
		return rankRegex
	case TypeMaxKey:
		return rankMaxKey
	default:
		return rankMaxKey
	}
}

// compareNumbers orders two numeric values by magnitude across the int32, int64,
// and double types (spec 2061 doc 02 §7.3). NaN sorts above every other number
// and equal to itself; integers compare exactly, and an int64 against a double
// is compared without precision loss.
func compareNumbers(a, b RawValue) int {
	aNaN, bNaN := isNaN(a), isNaN(b)
	if aNaN || bNaN {
		switch {
		case aNaN && bNaN:
			return 0
		case aNaN:
			return 1
		default:
			return -1
		}
	}
	// Both finite (or infinite). Integers compare exactly; a mixed int64/double
	// pair is compared through big.Float so a large int64 is not rounded.
	ai, aIsInt := asInt64(a)
	bi, bIsInt := asInt64(b)
	if aIsInt && bIsInt {
		return cmpInt64(ai, bi)
	}
	af, _ := a.AsFloat64()
	bf, _ := b.AsFloat64()
	if aIsInt {
		return -compareFloatInt64(bf, ai)
	}
	if bIsInt {
		return compareFloatInt64(af, bi)
	}
	return cmpFloat64(af, bf)
}

// compareFloatInt64 compares a float64 against an int64 exactly. Within the range
// where every int64 is representable as a float64 it compares directly; outside
// it, big.Float carries the full precision of both operands.
func compareFloatInt64(f float64, i int64) int {
	if math.IsInf(f, 1) {
		return 1
	}
	if math.IsInf(f, -1) {
		return -1
	}
	const exact = 1 << 53
	if i > -exact && i < exact {
		return cmpFloat64(f, float64(i))
	}
	bf := big.NewFloat(f)
	bi := new(big.Float).SetInt64(i)
	return bf.Cmp(bi)
}

func isNaN(v RawValue) bool {
	f, ok := v.DoubleOK()
	return ok && math.IsNaN(f)
}

// asInt64 reports an integer value as an int64, false for doubles and anything
// non-integral.
func asInt64(v RawValue) (int64, bool) {
	switch v.Type {
	case TypeInt32:
		return int64(v.Int32()), true
	case TypeInt64:
		return v.Int64(), true
	default:
		return 0, false
	}
}

// compareDocuments orders two documents field by field: names first (by raw
// bytes), then values, then length, so a shorter prefix sorts before a longer
// document (spec 2061 doc 02 §7.6). Field order is significant and is taken as
// stored, not sorted.
func compareDocuments(a, b Raw) int {
	ea, _ := a.Elements()
	eb, _ := b.Elements()
	for i := 0; i < len(ea) && i < len(eb); i++ {
		if c := cmpString(ea[i].Key, eb[i].Key); c != 0 {
			return c
		}
		if c := Compare(ea[i].Value, eb[i].Value); c != 0 {
			return c
		}
	}
	return cmpInt(len(ea), len(eb))
}

// compareArrays orders two arrays element by element, then by length (spec 2061
// doc 02 §7.5). Arrays are documents with ascending numeric keys, so the stored
// element order is the comparison order.
func compareArrays(a, b Raw) int {
	ea, _ := a.Elements()
	eb, _ := b.Elements()
	for i := 0; i < len(ea) && i < len(eb); i++ {
		if c := Compare(ea[i].Value, eb[i].Value); c != 0 {
			return c
		}
	}
	return cmpInt(len(ea), len(eb))
}

// compareBinary orders binary values by length, then subtype, then bytes, which
// is MongoDB's BinData order (spec 2061 doc 02 §7.8).
func compareBinary(a, b RawValue) int {
	sa, da, _ := a.Binary()
	sb, db, _ := b.Binary()
	if c := cmpInt(len(da), len(db)); c != 0 {
		return c
	}
	if c := cmpByte(sa, sb); c != 0 {
		return c
	}
	return bytes.Compare(da, db)
}

// compareRegex orders regex values by pattern, then options (spec 2061 doc 02
// §7.8).
func compareRegex(a, b RawValue) int {
	pa, oa, _ := a.Regex()
	pb, ob, _ := b.Regex()
	if c := cmpString(pa, pb); c != 0 {
		return c
	}
	return cmpString(oa, ob)
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func cmpByte(a, b byte) int { return cmpInt(int(a), int(b)) }

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func cmpUint64(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func cmpFloat64(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func cmpBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case !a:
		return -1
	default:
		return 1
	}
}
