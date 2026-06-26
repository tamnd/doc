package colstore

import (
	"encoding/binary"
	"errors"
	"math"
)

// ErrCorruptColumn is returned when an encoded column cannot be decoded: a
// truncated buffer, an unknown encoding tag, or a length that does not add up.
var ErrCorruptColumn = errors.New("colstore: corrupt column encoding")

// A column is a positional slice of values for one field over a segment's RID
// range. A KindNull entry is a null or missing field; the encoder records those in
// a null bitmap and encodes only the non-null stream (spec 2061 doc 04 §10.2).
//
// The six encodings the spec names (§10.3) live here. Three are general and correct
// for any column: plain (every value serialized in order), dictionary (distinct
// values plus bit-packed codes), and run-length (value plus run-length pairs).
// Three specialize on a homogeneous integer or boolean column and are pure size
// wins: bit-packing (zig-zag values packed to the minimum width), frame-of-reference
// (subtract the minimum, then bit-pack), and delta plus bit-packing (successive
// differences, zig-zagged and packed). EncodeColumn tries every applicable encoding
// and keeps the smallest, so the choice is driven by the observed distribution.
const (
	encPlain   byte = 1 // any column: each value serialized in order
	encDict    byte = 2 // any column: distinct dictionary + bit-packed codes
	encRLE     byte = 3 // any column: (value, run length) pairs
	encBitpack byte = 4 // int/bool: zig-zag values, bit-packed
	encFOR     byte = 5 // int/bool: base + bit-packed offsets
	encDelta   byte = 6 // int/bool: first + zig-zag deltas, bit-packed
)

// EncodeColumn frames a column as [uvarint n][null bitmap][encoded stream]. The
// stream begins with a one-byte encoding tag chosen as the smallest of the
// applicable encodings over the non-null values.
func EncodeColumn(vals []Value) []byte {
	n := len(vals)
	nonNull := make([]Value, 0, n)
	for _, v := range vals {
		if v.Kind != KindNull {
			nonNull = append(nonNull, v)
		}
	}
	out := make([]byte, 0, len(nonNull)*4+n/8+16)
	out = binary.AppendUvarint(out, uint64(n))
	out = appendNullBitmap(out, vals)
	return append(out, chooseEncoding(nonNull)...)
}

// DecodeColumn reverses EncodeColumn, scattering the decoded non-null stream back
// into its positions and filling the rest with KindNull.
func DecodeColumn(b []byte) ([]Value, error) {
	n64, k := binary.Uvarint(b)
	if k <= 0 {
		return nil, ErrCorruptColumn
	}
	b = b[k:]
	n := int(n64)
	nbBytes := (n + 7) / 8
	if len(b) < nbBytes {
		return nil, ErrCorruptColumn
	}
	nulls := readNullBitmap(b[:nbBytes], n)
	b = b[nbBytes:]
	m := 0
	for _, isNull := range nulls {
		if !isNull {
			m++
		}
	}
	stream, err := decodeStream(b, m)
	if err != nil {
		return nil, err
	}
	out := make([]Value, n)
	j := 0
	for i := 0; i < n; i++ {
		if nulls[i] {
			out[i] = NullValue
		} else {
			out[i] = stream[j]
			j++
		}
	}
	return out, nil
}

// appendNullBitmap appends ceil(n/8) bytes with bit i set when vals[i] is null.
func appendNullBitmap(dst []byte, vals []Value) []byte {
	nb := make([]byte, (len(vals)+7)/8)
	for i, v := range vals {
		if v.Kind == KindNull {
			nb[i>>3] |= 1 << uint(i&7)
		}
	}
	return append(dst, nb...)
}

// readNullBitmap expands a bitmap into a per-position bool slice.
func readNullBitmap(b []byte, n int) []bool {
	out := make([]bool, n)
	for i := 0; i < n; i++ {
		out[i] = b[i>>3]&(1<<uint(i&7)) != 0
	}
	return out
}

// chooseEncoding returns the smallest applicable encoding of the non-null values.
func chooseEncoding(vals []Value) []byte {
	best := encodePlain(vals)
	if c := encodeRLE(vals); len(c) < len(best) {
		best = c
	}
	if c := encodeDict(vals); len(c) < len(best) {
		best = c
	}
	if rk, ok := homogeneousIntKind(vals); ok {
		if c := encodeBitpack(vals, rk); len(c) < len(best) {
			best = c
		}
		if c := encodeFOR(vals, rk); len(c) < len(best) {
			best = c
		}
		if c := encodeDelta(vals, rk); len(c) < len(best) {
			best = c
		}
	}
	return best
}

// decodeStream dispatches on the encoding tag and returns m values.
func decodeStream(b []byte, m int) ([]Value, error) {
	if len(b) == 0 {
		if m == 0 {
			return nil, nil
		}
		return nil, ErrCorruptColumn
	}
	tag, body := b[0], b[1:]
	switch tag {
	case encPlain:
		return decodePlain(body, m)
	case encDict:
		return decodeDict(body, m)
	case encRLE:
		return decodeRLE(body, m)
	case encBitpack:
		return decodeBitpack(body, m)
	case encFOR:
		return decodeFOR(body, m)
	case encDelta:
		return decodeDelta(body, m)
	default:
		return nil, ErrCorruptColumn
	}
}

// putValue serializes one value: a kind byte then a kind-specific payload. It is
// the unit the general encodings (plain, RLE, dictionary) are built from, so they
// are correct for every value type including the opaque Other case.
func putValue(dst []byte, v Value) []byte {
	dst = append(dst, byte(v.Kind))
	switch v.Kind {
	case KindBool, KindInt:
		dst = binary.AppendVarint(dst, v.I)
	case KindFloat:
		dst = binary.LittleEndian.AppendUint64(dst, math.Float64bits(v.F))
	case KindString:
		dst = binary.AppendUvarint(dst, uint64(len(v.S)))
		dst = append(dst, v.S...)
	case KindOther:
		dst = append(dst, v.tag)
		dst = binary.AppendUvarint(dst, uint64(len(v.S)))
		dst = append(dst, v.S...)
	}
	return dst
}

// getValue reads one value, returning it and the bytes consumed, or ok=false on a
// short or malformed buffer.
func getValue(b []byte) (Value, int, bool) {
	if len(b) == 0 {
		return Value{}, 0, false
	}
	kind := Kind(b[0])
	off := 1
	switch kind {
	case KindNull:
		return NullValue, off, true
	case KindBool, KindInt:
		i, k := binary.Varint(b[off:])
		if k <= 0 {
			return Value{}, 0, false
		}
		return Value{Kind: kind, I: i}, off + k, true
	case KindFloat:
		if len(b) < off+8 {
			return Value{}, 0, false
		}
		f := math.Float64frombits(binary.LittleEndian.Uint64(b[off:]))
		return Value{Kind: KindFloat, F: f}, off + 8, true
	case KindString:
		ln, k := binary.Uvarint(b[off:])
		if k <= 0 || len(b) < off+k+int(ln) {
			return Value{}, 0, false
		}
		off += k
		return Value{Kind: KindString, S: string(b[off : off+int(ln)])}, off + int(ln), true
	case KindOther:
		if len(b) < off+1 {
			return Value{}, 0, false
		}
		tag := b[off]
		off++
		ln, k := binary.Uvarint(b[off:])
		if k <= 0 || len(b) < off+k+int(ln) {
			return Value{}, 0, false
		}
		off += k
		return Value{Kind: KindOther, S: string(b[off : off+int(ln)]), tag: tag}, off + int(ln), true
	default:
		return Value{}, 0, false
	}
}

// --- plain ---

func encodePlain(vals []Value) []byte {
	out := []byte{encPlain}
	for _, v := range vals {
		out = putValue(out, v)
	}
	return out
}

func decodePlain(b []byte, m int) ([]Value, error) {
	out := make([]Value, 0, m)
	for i := 0; i < m; i++ {
		v, n, ok := getValue(b)
		if !ok {
			return nil, ErrCorruptColumn
		}
		out = append(out, v)
		b = b[n:]
	}
	return out, nil
}

// --- run-length ---

func encodeRLE(vals []Value) []byte {
	out := []byte{encRLE}
	var count uint64
	// First pass to count runs so the run count leads the body.
	for i := 0; i < len(vals); {
		j := i + 1
		for j < len(vals) && vals[j].strictKey() == vals[i].strictKey() {
			j++
		}
		count++
		i = j
	}
	out = binary.AppendUvarint(out, count)
	for i := 0; i < len(vals); {
		j := i + 1
		for j < len(vals) && vals[j].strictKey() == vals[i].strictKey() {
			j++
		}
		out = putValue(out, vals[i])
		out = binary.AppendUvarint(out, uint64(j-i))
		i = j
	}
	return out
}

func decodeRLE(b []byte, m int) ([]Value, error) {
	runs, k := binary.Uvarint(b)
	if k <= 0 {
		return nil, ErrCorruptColumn
	}
	b = b[k:]
	out := make([]Value, 0, m)
	for r := uint64(0); r < runs; r++ {
		v, n, ok := getValue(b)
		if !ok {
			return nil, ErrCorruptColumn
		}
		b = b[n:]
		ln, kk := binary.Uvarint(b)
		if kk <= 0 {
			return nil, ErrCorruptColumn
		}
		b = b[kk:]
		for c := uint64(0); c < ln; c++ {
			out = append(out, v)
		}
	}
	if len(out) != m {
		return nil, ErrCorruptColumn
	}
	return out, nil
}

// --- dictionary ---

func encodeDict(vals []Value) []byte {
	codeOf := make(map[string]int, len(vals))
	var dict []Value
	codes := make([]uint64, len(vals))
	for i, v := range vals {
		key := v.strictKey()
		c, ok := codeOf[key]
		if !ok {
			c = len(dict)
			codeOf[key] = c
			dict = append(dict, v)
		}
		codes[i] = uint64(c)
	}
	out := []byte{encDict}
	out = binary.AppendUvarint(out, uint64(len(dict)))
	for _, d := range dict {
		out = putValue(out, d)
	}
	width := 0
	if len(dict) > 1 {
		width = bitWidth(uint64(len(dict)-1), 0)
	}
	out = append(out, byte(width))
	out = append(out, packBits(codes, width)...)
	return out
}

func decodeDict(b []byte, m int) ([]Value, error) {
	dn, k := binary.Uvarint(b)
	if k <= 0 {
		return nil, ErrCorruptColumn
	}
	b = b[k:]
	dict := make([]Value, dn)
	for i := range dict {
		v, n, ok := getValue(b)
		if !ok {
			return nil, ErrCorruptColumn
		}
		dict[i] = v
		b = b[n:]
	}
	if len(b) < 1 {
		return nil, ErrCorruptColumn
	}
	width := int(b[0])
	b = b[1:]
	codes, ok := unpackBits(b, width, m)
	if !ok {
		return nil, ErrCorruptColumn
	}
	out := make([]Value, m)
	for i, c := range codes {
		if int(c) >= len(dict) {
			return nil, ErrCorruptColumn
		}
		out[i] = dict[c]
	}
	return out, nil
}

// --- integer specializations ---

// homogeneousIntKind reports the restore kind (KindInt or KindBool) when every
// value shares it, so the integer encoders can run and decode unambiguously.
func homogeneousIntKind(vals []Value) (Kind, bool) {
	if len(vals) == 0 {
		return 0, false
	}
	k := vals[0].Kind
	if k != KindInt && k != KindBool {
		return 0, false
	}
	for _, v := range vals {
		if v.Kind != k {
			return 0, false
		}
	}
	return k, true
}

func encodeBitpack(vals []Value, rk Kind) []byte {
	zz := make([]uint64, len(vals))
	var max uint64
	for i, v := range vals {
		zz[i] = zigzag(v.I)
		if zz[i] > max {
			max = zz[i]
		}
	}
	width := bitWidth(max, 0)
	out := []byte{encBitpack, byte(rk), byte(width)}
	return append(out, packBits(zz, width)...)
}

func decodeBitpack(b []byte, m int) ([]Value, error) {
	if len(b) < 2 {
		return nil, ErrCorruptColumn
	}
	rk, width := Kind(b[0]), int(b[1])
	raw, ok := unpackBits(b[2:], width, m)
	if !ok {
		return nil, ErrCorruptColumn
	}
	out := make([]Value, m)
	for i, u := range raw {
		out[i] = Value{Kind: rk, I: unzigzag(u)}
	}
	return out, nil
}

func encodeFOR(vals []Value, rk Kind) []byte {
	base := vals[0].I
	for _, v := range vals {
		if v.I < base {
			base = v.I
		}
	}
	offs := make([]uint64, len(vals))
	var max uint64
	for i, v := range vals {
		offs[i] = uint64(v.I - base)
		if offs[i] > max {
			max = offs[i]
		}
	}
	width := bitWidth(max, 0)
	out := []byte{encFOR, byte(rk)}
	out = binary.AppendVarint(out, base)
	out = append(out, byte(width))
	return append(out, packBits(offs, width)...)
}

func decodeFOR(b []byte, m int) ([]Value, error) {
	if len(b) < 1 {
		return nil, ErrCorruptColumn
	}
	rk := Kind(b[0])
	base, k := binary.Varint(b[1:])
	if k <= 0 {
		return nil, ErrCorruptColumn
	}
	b = b[1+k:]
	if len(b) < 1 {
		return nil, ErrCorruptColumn
	}
	width := int(b[0])
	offs, ok := unpackBits(b[1:], width, m)
	if !ok {
		return nil, ErrCorruptColumn
	}
	out := make([]Value, m)
	for i, u := range offs {
		out[i] = Value{Kind: rk, I: base + int64(u)}
	}
	return out, nil
}

func encodeDelta(vals []Value, rk Kind) []byte {
	out := []byte{encDelta, byte(rk)}
	if len(vals) == 0 {
		out = binary.AppendVarint(out, 0)
		return append(out, 0)
	}
	out = binary.AppendVarint(out, vals[0].I)
	zz := make([]uint64, len(vals)-1)
	var max uint64
	for i := 1; i < len(vals); i++ {
		zz[i-1] = zigzag(vals[i].I - vals[i-1].I)
		if zz[i-1] > max {
			max = zz[i-1]
		}
	}
	width := bitWidth(max, 0)
	out = append(out, byte(width))
	return append(out, packBits(zz, width)...)
}

func decodeDelta(b []byte, m int) ([]Value, error) {
	rk := Kind(b[0])
	first, k := binary.Varint(b[1:])
	if k <= 0 {
		return nil, ErrCorruptColumn
	}
	b = b[1+k:]
	if len(b) < 1 {
		return nil, ErrCorruptColumn
	}
	width := int(b[0])
	if m == 0 {
		return nil, nil
	}
	deltas, ok := unpackBits(b[1:], width, m-1)
	if !ok {
		return nil, ErrCorruptColumn
	}
	out := make([]Value, m)
	out[0] = Value{Kind: rk, I: first}
	cur := first
	for i, u := range deltas {
		cur += unzigzag(u)
		out[i+1] = Value{Kind: rk, I: cur}
	}
	return out, nil
}

// --- bit-packing primitives ---

// bitWidth returns the number of bits to represent max (0 needs 0 bits). The unused
// second argument keeps a single call shape across the encoders.
func bitWidth(max uint64, _ int) int {
	w := 0
	for max > 0 {
		w++
		max >>= 1
	}
	return w
}

// packBits packs each value into width bits, low bit first, concatenated.
func packBits(vals []uint64, width int) []byte {
	if width == 0 {
		return nil
	}
	out := make([]byte, (width*len(vals)+7)/8)
	bit := 0
	for _, v := range vals {
		for b := 0; b < width; b++ {
			if v&(1<<uint(b)) != 0 {
				out[bit>>3] |= 1 << uint(bit&7)
			}
			bit++
		}
	}
	return out
}

// unpackBits reverses packBits, returning count values or ok=false if the buffer is
// too short for the declared width and count.
func unpackBits(data []byte, width, count int) ([]uint64, bool) {
	out := make([]uint64, count)
	if width == 0 {
		return out, true
	}
	if len(data) < (width*count+7)/8 {
		return nil, false
	}
	bit := 0
	for i := 0; i < count; i++ {
		var v uint64
		for b := 0; b < width; b++ {
			if data[bit>>3]&(1<<uint(bit&7)) != 0 {
				v |= 1 << uint(b)
			}
			bit++
		}
		out[i] = v
	}
	return out, true
}

func zigzag(v int64) uint64   { return uint64((v << 1) ^ (v >> 63)) }
func unzigzag(u uint64) int64 { return int64(u>>1) ^ -int64(u&1) }
