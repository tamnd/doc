package index

import (
	"encoding/binary"
	"errors"
	"math"

	"github.com/tamnd/doc/bson"
)

// ErrNotCovered reports an index-key field whose type cannot be reconstructed
// into a document value, so a covered scan is not possible over it. The nested
// document, array, regex, and decimal128 encodings are injective for ordering and
// uniqueness but are not reversed here (spec 2061 doc 07 §3.13, doc 11 §5.6).
var ErrNotCovered = errors.New("index: field type is not coverable")

// AppendDecodedField decodes one field encoding from the front of b (a field key
// with no trailing RID), inverting the bytes first when the field is descending,
// appends the reconstructed value under key to bldr, and returns the number of
// source bytes the field consumed. It powers the covered-scan fast path: the
// values come straight from the index key, with no heap fetch.
//
// Numeric keys decode to Double because the key encoding collapses the four
// numeric BSON types into one order (spec 2061 doc 07 §3.3); a covered scan over a
// numeric field therefore normalizes its type, which the planner accounts for when
// it decides a plan is coverable.
func AppendDecodedField(bldr *bson.Builder, key string, b []byte, descending bool) (int, error) {
	var mask byte
	if descending {
		mask = 0xFF
	}
	if len(b) == 0 {
		return 0, ErrCorruptKey
	}
	tag := b[0] ^ mask
	switch tag {
	case tagMinKey:
		bldr.AppendValue(key, bson.RawValue{Type: bson.TypeMinKey})
		return 1, nil
	case tagMaxKey:
		bldr.AppendValue(key, bson.RawValue{Type: bson.TypeMaxKey})
		return 1, nil
	case tagNull:
		bldr.AppendNull(key)
		return 1, nil
	case tagBool:
		if len(b) < 2 {
			return 0, ErrCorruptKey
		}
		bldr.AppendBoolean(key, (b[1]^mask) != 0)
		return 2, nil
	case tagNumber:
		if len(b) < 9 {
			return 0, ErrCorruptKey
		}
		bits := readMasked64(b[1:9], mask)
		bldr.AppendDouble(key, math.Float64frombits(unflipSign(bits)))
		return 9, nil
	case tagDate:
		if len(b) < 9 {
			return 0, ErrCorruptKey
		}
		bits := readMasked64(b[1:9], mask)
		bldr.AppendDateTime(key, int64(unflipSign(bits)))
		return 9, nil
	case tagTimestamp:
		if len(b) < 9 {
			return 0, ErrCorruptKey
		}
		hi := uint64(readMasked32(b[1:5], mask))
		lo := uint64(readMasked32(b[5:9], mask))
		bldr.AppendTimestamp(key, hi<<32|lo)
		return 9, nil
	case tagObjectID:
		if len(b) < 13 {
			return 0, ErrCorruptKey
		}
		var oid [12]byte
		for i := range oid {
			oid[i] = b[1+i] ^ mask
		}
		bldr.AppendObjectID(key, oid)
		return 13, nil
	case tagString:
		s, n, err := decodeString(b[1:], mask)
		if err != nil {
			return 0, err
		}
		bldr.AppendString(key, s)
		return n + 1, nil
	case tagBinary:
		if len(b) < 5 {
			return 0, ErrCorruptKey
		}
		ln := int(readMasked32(b[1:5], mask))
		if len(b) < 5+ln+1 {
			return 0, ErrCorruptKey
		}
		data := make([]byte, ln)
		for i := range data {
			data[i] = b[5+i] ^ mask
		}
		sub := b[5+ln] ^ mask
		bldr.AppendBinary(key, sub, data)
		return 5 + ln + 1, nil
	default:
		return 0, ErrNotCovered
	}
}

// FieldLen returns the number of bytes the field encoding at the front of b
// occupies, without reconstructing a value. It lets the covered path skip over a
// leading compound-key field it does not need to project.
func FieldLen(b []byte, descending bool) (int, error) {
	var sink bson.Builder
	return AppendDecodedField(&sink, "_", b, descending)
}

// decodeString reads a NUL-escaped string body (after its tag) under mask,
// returning the string and the bytes consumed including the terminator.
func decodeString(b []byte, mask byte) (string, int, error) {
	var out []byte
	i := 0
	for i < len(b) {
		c := b[i] ^ mask
		if c == 0x00 {
			// A real NUL is escaped as 0x00 0x01; a lone 0x00 terminates.
			if i+1 < len(b) && (b[i+1]^mask) == 0x01 {
				out = append(out, 0x00)
				i += 2
				continue
			}
			return string(out), i + 1, nil
		}
		out = append(out, c)
		i++
	}
	return "", 0, ErrCorruptKey
}

func readMasked64(b []byte, mask byte) uint64 {
	var tmp [8]byte
	for i := range tmp {
		tmp[i] = b[i] ^ mask
	}
	return binary.BigEndian.Uint64(tmp[:])
}

func readMasked32(b []byte, mask byte) uint32 {
	var tmp [4]byte
	for i := range tmp {
		tmp[i] = b[i] ^ mask
	}
	return binary.BigEndian.Uint32(tmp[:])
}

// unflipSign reverses appendSignFlipped64: if the high bit is set the original
// was non-negative (clear it), otherwise the whole word was inverted.
func unflipSign(bits uint64) uint64 {
	if bits&(1<<63) != 0 {
		return bits &^ (1 << 63)
	}
	return ^bits
}
