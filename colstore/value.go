// Package colstore implements doc's optional columnar projection store (spec 2061
// doc 04 §10). It is a derived, auxiliary structure: the heap file is always the
// source of truth, and the column store shreds chosen fields into compressed,
// encoded segments so analytical queries that touch many documents but few fields
// read a fraction of the bytes a heap scan would.
//
// The package is split into layers. value.go is the typed value model a projected
// field collapses to. encoding.go turns a column of values into one of six
// lightweight encodings (plain, dictionary, RLE, bit-packing, frame-of-reference,
// delta plus bit-packing) chosen per segment from the observed distribution, with a
// zone map and null bitmap. segment.go frames an immutable segment with its MVCC
// version stamps. store.go is the per-collection set of segments with the
// in-progress buffer and the snapshot-aware scan. aggregate.go is the
// column-at-a-time vectorized path the engine runs $group, $sum, and $avg through.
//
// The package depends only on bson and sys from the doc module, so it stays inside
// the zero-dependency core.
package colstore

import (
	"encoding/binary"
	"math"

	"github.com/tamnd/doc/bson"
)

// Kind is the column-store value type. A projected BSON field collapses to one of
// these: the scalars the encodings specialize on, plus Other for any value the
// column store keeps opaque (documents, arrays, binary, regex, and the rest), which
// rides through the dictionary and plain paths as raw BSON value bytes.
type Kind uint8

const (
	KindNull Kind = iota
	KindBool
	KindInt
	KindFloat
	KindString
	KindOther
)

// Value is one projected field value. Numeric BSON types collapse so the encodings
// can specialize: int32 and int64 both land in KindInt, double in KindFloat. A
// missing field and an explicit null both collapse to KindNull, which the null
// bitmap records. Strings keep their bytes in S; Other keeps the raw BSON value
// bytes (the type byte is carried in raw[0] via OtherTag) in S so it round-trips.
type Value struct {
	Kind Kind
	I    int64   // KindBool (0/1), KindInt
	F    float64 // KindFloat
	S    string  // KindString, KindOther (opaque raw bytes)
	tag  byte    // KindOther: the BSON type byte the opaque bytes belong to
}

// NullValue is the canonical null/missing value.
var NullValue = Value{Kind: KindNull}

// FromField projects a field out of a document and collapses it to a Value. A
// missing field returns NullValue, the same as an explicit BSON null, which matches
// how the heap path treats an absent projected field (spec 2061 doc 04 §10.4).
func FromField(doc bson.Raw, path string) Value {
	v, ok := doc.Lookup(path)
	if !ok {
		return NullValue
	}
	return FromRawValue(v)
}

// FromRawValue collapses one BSON value to a column Value.
func FromRawValue(v bson.RawValue) Value {
	switch v.Type {
	case 0, bson.TypeNull, bson.TypeUndefined:
		return NullValue
	case bson.TypeBoolean:
		if v.Boolean() {
			return Value{Kind: KindBool, I: 1}
		}
		return Value{Kind: KindBool, I: 0}
	case bson.TypeInt32:
		return Value{Kind: KindInt, I: int64(v.Int32())}
	case bson.TypeInt64:
		return Value{Kind: KindInt, I: v.Int64()}
	case bson.TypeDateTime:
		return Value{Kind: KindInt, I: v.DateTime()}
	case bson.TypeTimestamp:
		return Value{Kind: KindInt, I: int64(v.Timestamp())}
	case bson.TypeDouble:
		return Value{Kind: KindFloat, F: v.Double()}
	case bson.TypeString:
		return Value{Kind: KindString, S: v.StringValue()}
	default:
		// Keep anything else opaque: store the raw value payload and its type byte so
		// the dictionary and plain encoders treat it as bytes and it round-trips.
		return Value{Kind: KindOther, S: string(v.Data), tag: byte(v.Type)}
	}
}

// AsFloat returns the value as a float64 for the numeric accumulators, and whether
// it is numeric at all. Bool counts as numeric (0 or 1), matching how $sum and $avg
// coerce in the heap path.
func (v Value) AsFloat() (float64, bool) {
	switch v.Kind {
	case KindInt, KindBool:
		return float64(v.I), true
	case KindFloat:
		return v.F, true
	default:
		return 0, false
	}
}

// hashKey returns a map key that agrees with equalKey: numeric values hash by their
// float64 value so Int and Float of equal magnitude collapse, the rest by kind and
// payload.
func (v Value) hashKey() string {
	var b [9]byte
	if f, ok := v.AsFloat(); ok {
		b[0] = 'n'
		binary.LittleEndian.PutUint64(b[1:], math.Float64bits(f))
		return string(b[:])
	}
	switch v.Kind {
	case KindString:
		return "s" + v.S
	case KindOther:
		return "o" + string(v.tag) + v.S
	default:
		return "z" // null
	}
}

// compareNumericOrString orders two values within a comparable family and reports
// ok=false when they are not both numeric or both string. It is enough for zone
// maps, which only prune numeric and string range predicates.
func compareNumericOrString(a, b Value) (int, bool) {
	if f1, ok1 := a.AsFloat(); ok1 {
		if f2, ok2 := b.AsFloat(); ok2 {
			switch {
			case f1 < f2:
				return -1, true
			case f1 > f2:
				return 1, true
			default:
				return 0, true
			}
		}
		return 0, false
	}
	if a.Kind == KindString && b.Kind == KindString {
		switch {
		case a.S < b.S:
			return -1, true
		case a.S > b.S:
			return 1, true
		default:
			return 0, true
		}
	}
	return 0, false
}

// comparableForZone reports whether a value participates in a zone map: numeric or
// string. A segment with any other non-null kind carries no zone map.
func (v Value) comparableForZone() bool {
	if _, ok := v.AsFloat(); ok {
		return true
	}
	return v.Kind == KindString
}

// strictKey returns a map key that distinguishes values by exact kind and payload,
// unlike hashKey which collapses numeric kinds. The dictionary and run-length
// encoders use it so a decoded value reproduces its original kind exactly.
func (v Value) strictKey() string {
	var b [9]byte
	switch v.Kind {
	case KindBool:
		if v.I != 0 {
			return "b1"
		}
		return "b0"
	case KindInt:
		b[0] = 'i'
		binary.LittleEndian.PutUint64(b[1:], uint64(v.I))
		return string(b[:])
	case KindFloat:
		b[0] = 'f'
		binary.LittleEndian.PutUint64(b[1:], math.Float64bits(v.F))
		return string(b[:])
	case KindString:
		return "s" + v.S
	case KindOther:
		return "o" + string(v.tag) + v.S
	default:
		return "z"
	}
}

// ToRawValue turns a column Value back into a BSON value for materialization. Int
// values reconstruct as int64, which is lossless for the integer types the column
// collapsed (int32 widens, which $group output already does). The scalar cases
// build a standalone value through a scratch key, the same construction agg and
// update use; Other reattaches its stored type byte to its raw payload directly.
func (v Value) ToRawValue() bson.RawValue {
	switch v.Kind {
	case KindBool:
		return scratch(bson.NewBuilder().AppendBoolean("v", v.I != 0))
	case KindInt:
		return scratch(bson.NewBuilder().AppendInt64("v", v.I))
	case KindFloat:
		return scratch(bson.NewBuilder().AppendDouble("v", v.F))
	case KindString:
		return scratch(bson.NewBuilder().AppendString("v", v.S))
	case KindOther:
		return bson.RawValue{Type: bson.Type(v.tag), Data: []byte(v.S)}
	default:
		return scratch(bson.NewBuilder().AppendNull("v"))
	}
}

// scratch reads the single value appended under key "v" back out of a builder.
func scratch(b *bson.Builder) bson.RawValue {
	rv, _ := b.Build().Lookup("v")
	return rv
}
