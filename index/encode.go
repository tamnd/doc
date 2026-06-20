package index

import (
	"encoding/binary"
	"errors"
	"math"
	"sort"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/storage"
)

// ErrUnindexableType reports a BSON value whose type has no defined order-
// preserving key encoding (the deprecated DBPointer and code-with-scope types).
// Such values can be stored in documents but cannot be index keys.
var ErrUnindexableType = errors.New("index: value type cannot be encoded as a key")

// EncodeValue produces the order-preserving field-key encoding for any BSON value
// (spec 2061 doc 07 §3): a type-tag byte that brackets by type followed by an
// order-preserving body. It is the general form behind the typed Encode* helpers
// and the encoder the write path uses to turn an `_id` of any type into a key.
//
// The returned key carries no trailing RID; treeKey appends that. For the
// numeric, string, objectId, bool, and null types this matches the typed helpers
// byte for byte. Document and array values encode injectively (equality- and
// uniqueness-correct) but their cross-document ordering against MongoDB's nested
// comparison is finalized with the comparison engine in M3.
func EncodeValue(v bson.RawValue) (storage.IndexKey, error) {
	return appendValueKey(nil, v)
}

func appendValueKey(dst []byte, v bson.RawValue) ([]byte, error) {
	switch v.Type {
	case bson.TypeMinKey:
		return append(dst, tagMinKey), nil
	case bson.TypeMaxKey:
		return append(dst, tagMaxKey), nil
	case bson.TypeNull, bson.TypeUndefined:
		return append(dst, tagNull), nil

	case bson.TypeDouble:
		return appendNumber(dst, v.Double()), nil
	case bson.TypeInt32:
		return appendNumber(dst, float64(v.Int32())), nil
	case bson.TypeInt64:
		return appendNumber(dst, float64(v.Int64())), nil
	case bson.TypeDecimal128:
		return appendDecimal128(dst, v.Decimal128()), nil

	case bson.TypeString, bson.TypeSymbol, bson.TypeJavaScript:
		s, ok := v.StringValueOK()
		if !ok {
			return nil, ErrCorruptKey
		}
		return appendString(dst, s), nil

	case bson.TypeObjectID:
		oid := v.ObjectID()
		dst = append(dst, tagObjectID)
		return append(dst, oid[:]...), nil

	case bson.TypeBoolean:
		b := byte(0)
		if v.Boolean() {
			b = 1
		}
		return append(dst, tagBool, b), nil

	case bson.TypeDateTime:
		dst = append(dst, tagDate)
		return appendSignFlipped64(dst, uint64(v.DateTime())), nil

	case bson.TypeTimestamp:
		ts := v.Timestamp()
		dst = append(dst, tagTimestamp)
		dst = binary.BigEndian.AppendUint32(dst, uint32(ts>>32))   // ordinal (seconds)
		return binary.BigEndian.AppendUint32(dst, uint32(ts)), nil // increment

	case bson.TypeBinary:
		sub, data, ok := v.Binary()
		if !ok {
			return nil, ErrCorruptKey
		}
		dst = append(dst, tagBinary)
		dst = binary.BigEndian.AppendUint32(dst, uint32(len(data)))
		dst = append(dst, data...)
		return append(dst, sub), nil

	case bson.TypeRegex:
		pat, opt, ok := v.Regex()
		if !ok {
			return nil, ErrCorruptKey
		}
		dst = append(dst, tagRegex)
		dst = append(dst, pat...)
		dst = append(dst, 0x00)
		dst = append(dst, opt...)
		return append(dst, 0x00), nil

	case bson.TypeDocument:
		return appendDocKey(dst, tagDocument, v.Document(), true)
	case bson.TypeArray:
		return appendDocKey(dst, tagArray, v.Document(), false)

	default:
		return nil, ErrUnindexableType
	}
}

// ErrCorruptKey reports a value whose payload was too short to encode.
var ErrCorruptKey = errors.New("index: corrupt value payload")

// appendNumber appends tag + big-endian IEEE 754 with the sign-bit-flip.
func appendNumber(dst []byte, f float64) []byte {
	dst = append(dst, tagNumber)
	return appendSignFlippedFloat(dst, f)
}

func appendSignFlippedFloat(dst []byte, f float64) []byte {
	return appendSignFlipped64(dst, math.Float64bits(f))
}

// appendSignFlipped64 writes the order-preserving big-endian form of a 64-bit
// value whose sign lives in bit 63: flip the sign bit if clear, else flip all.
func appendSignFlipped64(dst []byte, bits uint64) []byte {
	if bits&(1<<63) == 0 {
		bits |= 1 << 63
	} else {
		bits = ^bits
	}
	return binary.BigEndian.AppendUint64(dst, bits)
}

// appendDecimal128 applies the sign-flip to the 16-byte big-endian decimal
// (spec 2061 doc 07 §3.4). v is the little-endian on-wire bytes; reverse first.
func appendDecimal128(dst []byte, v [16]byte) []byte {
	var be [16]byte
	for i := range be {
		be[i] = v[15-i]
	}
	if be[0]&0x80 == 0 {
		be[0] |= 0x80
	} else {
		for i := range be {
			be[i] = ^be[i]
		}
	}
	dst = append(dst, tagNumber)
	return append(dst, be[:]...)
}

// appendString appends tag + NUL-escaped UTF-8 bytes + terminator.
func appendString(dst []byte, s string) []byte {
	dst = append(dst, tagString)
	for i := 0; i < len(s); i++ {
		if s[i] == 0x00 {
			dst = append(dst, 0x00, 0x01)
		} else {
			dst = append(dst, s[i])
		}
	}
	return append(dst, 0x00)
}

// appendDocKey encodes a nested document or array injectively: the tag, then for
// each field its NUL-escaped name, a 0x00 separator, and the recursive value
// encoding, terminated by 0x00 0x00. Document fields are sorted by name; array
// elements keep their order (spec 2061 doc 07 §3.13). Each sub-encoding is self-
// delimiting, so the structure round-trips for equality and uniqueness.
func appendDocKey(dst []byte, tag byte, doc bson.Raw, sortFields bool) ([]byte, error) {
	dst = append(dst, tag)
	elems, err := doc.Elements()
	if err != nil {
		return nil, err
	}
	if sortFields {
		sort.SliceStable(elems, func(i, j int) bool { return elems[i].Key < elems[j].Key })
	}
	for _, e := range elems {
		for i := 0; i < len(e.Key); i++ {
			if e.Key[i] == 0x00 {
				dst = append(dst, 0x00, 0x01)
			} else {
				dst = append(dst, e.Key[i])
			}
		}
		dst = append(dst, 0x00)
		dst, err = appendValueKey(dst, e.Value)
		if err != nil {
			return nil, err
		}
	}
	return append(dst, 0x00, 0x00), nil
}
