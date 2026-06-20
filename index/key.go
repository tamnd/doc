// Package index is doc's B-tree index layer: the M1 _id index and the
// order-preserving key encoding that bridges BSON values to bytewise-comparable
// B-tree keys (spec 2061 doc 07). It implements storage.IndexStore over the
// pager, reusing format's slotted pages for variable-length B-tree cells.
//
// What M1 ships here is the always-present _id index: a unique, single-field,
// ascending B+tree mapping an encoded _id value to the document's RID, with the
// key encoding for the ObjectId and integer _id types (spec 2061 doc 07 §4, and
// roadmap doc 19 §22 M1). The encoders for the remaining BSON types, secondary
// indexes, the planner's bounds construction, and MVCC-versioned entries arrive
// in later milestones; the key format is fixed now so those extensions are
// additive.
package index

import (
	"encoding/binary"
	"math"

	"github.com/tamnd/doc/storage"
	"github.com/tamnd/doc/sys"
)

// Type tag bytes prefix every encoded key, ordering values by BSON type bracket
// (spec 2061 doc 07 §3.3). Gaps are left for types that land in later milestones.
const (
	tagMinKey  = 0x01
	tagNull    = 0x05
	tagNumber  = 0x10 // Double, Int32, Int64, Decimal128 share one numeric order
	tagString  = 0x20
	tagObjectID = 0x48
	tagBool    = 0x50
	tagMaxKey  = 0xFF
)

// ridSuffixLen is the fixed trailing RID appended to every index key (spec 2061
// doc 07 §3.16): a 4-byte big-endian page number and a 2-byte big-endian slot.
// It is a tiebreaker that makes the tree key globally unique even for a
// non-unique index, so the B-tree never holds duplicate keys.
const ridSuffixLen = 6

// EncodeObjectID encodes an ObjectId _id (spec 2061 doc 07 §3.6). The 12 raw
// big-endian bytes already sort in the required order (timestamp dominates), so
// no transformation is needed beyond the type tag.
func EncodeObjectID(oid sys.ObjectID) storage.IndexKey {
	k := make(storage.IndexKey, 1+12)
	k[0] = tagObjectID
	copy(k[1:], oid[:])
	return k
}

// EncodeInt64 encodes an integer _id (spec 2061 doc 07 §3.4). Integers and
// doubles share the numeric tag and a common order-preserving representation, so
// an Int32, Int64, and Double of equal value encode identically.
func EncodeInt64(v int64) storage.IndexKey { return encodeNumber(float64(v)) }

// EncodeFloat64 encodes a double-valued key (spec 2061 doc 07 §3.4).
func EncodeFloat64(v float64) storage.IndexKey { return encodeNumber(v) }

// encodeNumber applies the big-endian IEEE 754 sign-bit-flip transform: positive
// values flip only the sign bit, negative values flip all bits, yielding a byte
// string whose bytewise order matches arithmetic order with negatives below
// positives and NaN above all finite values.
func encodeNumber(v float64) storage.IndexKey {
	bits := math.Float64bits(v)
	if bits&(1<<63) == 0 {
		bits |= 1 << 63
	} else {
		bits = ^bits
	}
	k := make(storage.IndexKey, 1+8)
	k[0] = tagNumber
	binary.BigEndian.PutUint64(k[1:], bits)
	return k
}

// EncodeBool encodes a boolean key (spec 2061 doc 07 §3.7).
func EncodeBool(b bool) storage.IndexKey {
	v := byte(0)
	if b {
		v = 1
	}
	return storage.IndexKey{tagBool, v}
}

// EncodeNull encodes a null/missing key (spec 2061 doc 07 §3.11).
func EncodeNull() storage.IndexKey { return storage.IndexKey{tagNull} }

// EncodeString encodes a string key under the default binary collation (spec 2061
// doc 07 §3.5): the raw UTF-8 bytes with internal NULs escaped as 0x00 0x01 and a
// 0x00 terminator separating the key from the trailing RID. Named collations are
// a later milestone.
func EncodeString(s string) storage.IndexKey {
	k := make(storage.IndexKey, 0, len(s)+2)
	k = append(k, tagString)
	for i := 0; i < len(s); i++ {
		if s[i] == 0x00 {
			k = append(k, 0x00, 0x01)
		} else {
			k = append(k, s[i])
		}
	}
	k = append(k, 0x00)
	return k
}

// EncodeMinKey and EncodeMaxKey encode the internal sentinels that sort below and
// above every real value (spec 2061 doc 07 §3.12).
func EncodeMinKey() storage.IndexKey { return storage.IndexKey{tagMinKey} }
func EncodeMaxKey() storage.IndexKey { return storage.IndexKey{tagMaxKey} }

// treeKey forms the globally-unique B-tree key for a field key and RID: the field
// encoding followed by the 6-byte trailing RID (spec 2061 doc 07 §2.4, §3.16).
func treeKey(field storage.IndexKey, rid storage.RID) []byte {
	tk := make([]byte, len(field)+ridSuffixLen)
	copy(tk, field)
	putRIDSuffix(tk[len(field):], rid)
	return tk
}

// putRIDSuffix writes the 6-byte big-endian RID suffix into dst.
func putRIDSuffix(dst []byte, rid storage.RID) {
	_ = dst[ridSuffixLen-1]
	binary.BigEndian.PutUint32(dst[0:4], rid.PageNo)
	binary.BigEndian.PutUint16(dst[4:6], rid.Slot)
}

// ridFromSuffix reads the RID from the trailing 6 bytes of a tree key.
func ridFromSuffix(tk []byte) storage.RID {
	s := tk[len(tk)-ridSuffixLen:]
	return storage.RID{
		PageNo: binary.BigEndian.Uint32(s[0:4]),
		Slot:   binary.BigEndian.Uint16(s[4:6]),
	}
}

// fieldOf returns the field-key portion of a tree key (everything but the
// trailing RID).
func fieldOf(tk []byte) []byte { return tk[:len(tk)-ridSuffixLen] }
