package colstore

import (
	"bytes"
	"math"
	"testing"
)

// valuesEqual compares two decoded values for exact equality, including kind.
func valuesEqual(a, b Value) bool {
	return a.strictKey() == b.strictKey()
}

func columnsEqual(a, b []Value) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !valuesEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

// roundTrip encodes then decodes a column and asserts it comes back unchanged.
func roundTrip(t *testing.T, name string, vals []Value) {
	t.Helper()
	enc := EncodeColumn(vals)
	got, err := DecodeColumn(enc)
	if err != nil {
		t.Fatalf("%s: decode failed: %v", name, err)
	}
	if !columnsEqual(vals, got) {
		t.Fatalf("%s: round-trip mismatch\n in  = %v\n out = %v", name, vals, got)
	}
}

func ints(xs ...int64) []Value {
	out := make([]Value, len(xs))
	for i, x := range xs {
		out[i] = Value{Kind: KindInt, I: x}
	}
	return out
}

func strs(xs ...string) []Value {
	out := make([]Value, len(xs))
	for i, x := range xs {
		out[i] = Value{Kind: KindString, S: x}
	}
	return out
}

func TestColumnRoundTripShapes(t *testing.T) {
	roundTrip(t, "empty", nil)
	roundTrip(t, "single int", ints(42))
	roundTrip(t, "monotone ints", ints(1, 2, 3, 4, 5, 6, 7, 8))
	roundTrip(t, "wide ints", ints(0, 1<<40, -1<<40, math.MaxInt64, math.MinInt64))
	roundTrip(t, "repeated ints", ints(7, 7, 7, 7, 3, 3, 9))
	roundTrip(t, "small range", ints(100, 101, 100, 102, 103, 101))
	roundTrip(t, "strings", strs("a", "bb", "ccc", "a", "bb"))
	roundTrip(t, "low card strings", strs("red", "green", "red", "red", "green", "blue"))

	floats := []Value{{Kind: KindFloat, F: 3.5}, {Kind: KindFloat, F: -1.25}, {Kind: KindFloat, F: math.Inf(1)}}
	roundTrip(t, "floats", floats)

	bools := []Value{{Kind: KindBool, I: 1}, {Kind: KindBool, I: 0}, {Kind: KindBool, I: 1}, {Kind: KindBool, I: 1}}
	roundTrip(t, "bools", bools)

	withNulls := []Value{{Kind: KindInt, I: 5}, NullValue, {Kind: KindInt, I: 9}, NullValue, NullValue, {Kind: KindInt, I: 1}}
	roundTrip(t, "with nulls", withNulls)

	allNull := []Value{NullValue, NullValue, NullValue}
	roundTrip(t, "all null", allNull)

	mixed := []Value{{Kind: KindInt, I: 1}, {Kind: KindString, S: "x"}, {Kind: KindBool, I: 1}, NullValue, {Kind: KindFloat, F: 2.5}}
	roundTrip(t, "mixed kinds", mixed)

	other := []Value{{Kind: KindOther, S: "\x01\x02", tag: 0x05}, {Kind: KindOther, S: "\x01\x02", tag: 0x05}}
	roundTrip(t, "other repeated", other)
}

// TestEachEncodingDecodes forces every encoder to run and confirms its bytes decode
// back to the same column, so a decoder bug cannot hide behind the size chooser
// picking a different encoding.
func TestEachEncodingDecodes(t *testing.T) {
	vals := ints(100, 101, 102, 100, 105, 110, 100, 100)
	rk := KindInt
	cases := map[string][]byte{
		"plain":   encodePlain(vals),
		"rle":     encodeRLE(vals),
		"dict":    encodeDict(vals),
		"bitpack": encodeBitpack(vals, rk),
		"for":     encodeFOR(vals, rk),
		"delta":   encodeDelta(vals, rk),
	}
	for name, stream := range cases {
		got, err := decodeStream(stream, len(vals))
		if err != nil {
			t.Fatalf("%s: decodeStream failed: %v", name, err)
		}
		if !columnsEqual(vals, got) {
			t.Fatalf("%s: mismatch\n in  = %v\n out = %v", name, vals, got)
		}
	}
}

// TestChooserPicksCompact checks the chooser actually compresses: a low-cardinality
// run-heavy column must encode far smaller than plain, and a tight-range column must
// beat plain through frame-of-reference or bit-packing.
func TestChooserPicksCompact(t *testing.T) {
	runHeavy := make([]Value, 1024)
	for i := range runHeavy {
		runHeavy[i] = Value{Kind: KindInt, I: int64(i / 256)} // 4 distinct, long runs
	}
	enc := EncodeColumn(runHeavy)
	plain := encodePlain(runHeavy)
	if len(enc) >= len(plain)/4 {
		t.Fatalf("run-heavy column did not compress: enc=%d plain=%d", len(enc), len(plain))
	}

	tight := make([]Value, 1024)
	for i := range tight {
		tight[i] = Value{Kind: KindInt, I: 1000 + int64(i%8)} // 3-bit range
	}
	enc = EncodeColumn(tight)
	plain = encodePlain(tight)
	if len(enc) >= len(plain)/2 {
		t.Fatalf("tight-range column did not compress: enc=%d plain=%d", len(enc), len(plain))
	}
}

// TestDecodeCorrupt confirms the decoder rejects malformed input instead of
// panicking, the contract the fuzz target in segment_test relies on.
func TestDecodeCorrupt(t *testing.T) {
	bad := [][]byte{
		{},
		{0xff},                           // n only, truncated
		{0x02, 0x00, 0x09},               // n=2, no bitmap room
		append([]byte{0x03, 0x00}, 0x7f), // unknown tag region
	}
	for i, b := range bad {
		if _, err := DecodeColumn(b); err == nil {
			t.Fatalf("case %d: expected error on corrupt input %x", i, b)
		}
	}
}

// TestBitPackPrimitive checks the packer over a spread of widths and values.
func TestBitPackPrimitive(t *testing.T) {
	for width := 1; width <= 17; width++ {
		mask := uint64(1)<<uint(width) - 1
		vals := make([]uint64, 50)
		for i := range vals {
			vals[i] = uint64(i*2654435761) & mask
		}
		packed := packBits(vals, width)
		got, ok := unpackBits(packed, width, len(vals))
		if !ok {
			t.Fatalf("width %d: unpack failed", width)
		}
		for i := range vals {
			if got[i] != vals[i] {
				t.Fatalf("width %d index %d: got %d want %d", width, i, got[i], vals[i])
			}
		}
	}
}

func TestZigzag(t *testing.T) {
	for _, v := range []int64{0, 1, -1, 2, -2, math.MaxInt64, math.MinInt64, 1 << 40, -(1 << 40)} {
		if got := unzigzag(zigzag(v)); got != v {
			t.Fatalf("zigzag round-trip: got %d want %d", got, v)
		}
	}
}

// TestNullBitmapBytes confirms the framing puts the null bitmap where the decoder
// expects it.
func TestNullBitmapBytes(t *testing.T) {
	vals := []Value{NullValue, {Kind: KindInt, I: 1}, NullValue}
	enc := EncodeColumn(vals)
	// n=3 (1 byte uvarint), then 1 bitmap byte with bits 0 and 2 set = 0b101 = 0x05.
	if enc[0] != 0x03 || enc[1] != 0x05 {
		t.Fatalf("framing: n=%#x bitmap=%#x, want 0x03 0x05", enc[0], enc[1])
	}
	if !bytes.Equal(enc[:2], []byte{0x03, 0x05}) {
		t.Fatal("unexpected framing bytes")
	}
}
