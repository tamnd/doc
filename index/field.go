package index

import (
	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/storage"
)

// EncodeField produces the order-preserving key encoding for one indexed field
// value, inverting every byte when the field is descending (spec 2061 doc 07
// §3.15). For an ascending field this is exactly EncodeValue; for a descending
// field the bitwise inversion flips bytewise comparison so the B-tree's ascending
// traversal yields descending value order. The inversion covers the type tag as
// well, which keeps type-bracket order consistent within the descending field.
func EncodeField(v bson.RawValue, descending bool) (storage.IndexKey, error) {
	k, err := EncodeValue(v)
	if err != nil {
		return nil, err
	}
	if descending {
		for i := range k {
			k[i] = 0xFF ^ k[i]
		}
	}
	return k, nil
}

// AppendField appends the encoding of one field value to dst, inverting bytes for
// a descending field. It is the building block for a compound key: each field's
// self-delimiting encoding is concatenated in key-spec order (spec 2061 doc 07
// §3.14), with no separator because every per-type encoding is self-delimiting.
func AppendField(dst []byte, v bson.RawValue, descending bool) ([]byte, error) {
	start := len(dst)
	dst, err := appendValueKey(dst, v)
	if err != nil {
		return nil, err
	}
	if descending {
		for i := start; i < len(dst); i++ {
			dst[i] = 0xFF ^ dst[i]
		}
	}
	return dst, nil
}

// EncodeBound encodes a sentinel-aware bound value for a single field: a real
// value via EncodeField, or the descending-aware MinKey/MaxKey sentinel. The
// planner uses these to build open-ended key ranges (spec 2061 doc 07 §3.12,
// doc 10 §6.2). For a descending field the sentinels swap and invert so the
// bytewise range still brackets the field's inverted encoding.
func EncodeBoundMin(descending bool) storage.IndexKey {
	if descending {
		return storage.IndexKey{0xFF ^ tagMaxKey}
	}
	return storage.IndexKey{tagMinKey}
}

func EncodeBoundMax(descending bool) storage.IndexKey {
	if descending {
		return storage.IndexKey{0xFF ^ tagMinKey}
	}
	return storage.IndexKey{tagMaxKey}
}
