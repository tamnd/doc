package index

import (
	"bytes"
	"testing"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/sys"
)

// keyDocSeeds returns valid BSON documents whose fields cover every encodable value type, so the
// robust fuzzer mutates from realistic key material rather than pure noise.
func keyDocSeeds() [][]byte {
	var seeds [][]byte
	seeds = append(seeds, bson.NewBuilder().AppendInt32("a", 1).Build())
	seeds = append(seeds, bson.NewBuilder().AppendString("s", "hello").Build())
	seeds = append(seeds, bson.NewBuilder().AppendDouble("d", 3.5).AppendInt64("l", 1<<40).Build())
	seeds = append(seeds, bson.NewBuilder().AppendObjectID("oid", sys.ObjectID{1, 2, 3}).Build())
	seeds = append(seeds, bson.NewBuilder().AppendBoolean("b", true).AppendNull("n").Build())
	seeds = append(seeds, bson.NewBuilder().AppendDateTime("dt", 1_700_000_000_000).AppendTimestamp("ts", 7).Build())
	seeds = append(seeds, bson.NewBuilder().AppendBinary("bin", 0x00, []byte{1, 2, 3}).Build())
	inner := bson.NewBuilder().AppendInt32("x", 1).Build()
	seeds = append(seeds, bson.NewBuilder().AppendDocument("doc", inner).Build())
	return seeds
}

// FuzzKeyEncoderRobust feeds arbitrary BSON documents to the value-key encoder. The contract is
// that EncodeValue (and the descending AppendField form) never panics, hangs, or reads out of
// bounds on any value, including the nested document, array, regex, and binary shapes. An error
// (unindexable or corrupt) is acceptable; a panic is not (spec 2061 doc 07 §3, doc 19 §18).
func FuzzKeyEncoderRobust(f *testing.F) {
	for _, s := range keyDocSeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		raw := bson.Raw(data)
		if raw.Validate() != nil {
			return // the BSON fuzzer owns malformed framing
		}
		elems, err := raw.Elements()
		if err != nil {
			return
		}
		for _, e := range elems {
			// Both orientations must stay in bounds; the result is irrelevant.
			if _, err := EncodeValue(e.Value); err != nil {
				continue
			}
			if _, err := EncodeField(e.Value, false); err != nil {
				t.Fatalf("EncodeValue accepted but EncodeField(asc) rejected %v", e.Value.Type)
			}
			if _, err := EncodeField(e.Value, true); err != nil {
				t.Fatalf("EncodeValue accepted but EncodeField(desc) rejected %v", e.Value.Type)
			}
		}
	})
}

// orderExactValue builds a value from one of the types whose key encoding is exactly order-
// preserving and rank-ordered against every other type in this menu: null, minKey, maxKey, bool,
// int32 (float64-exact), string, datetime, objectId. It deliberately excludes large int64 and
// double (float64 precision and NaN) and documents/arrays (injective but not order-finalized),
// where the general invariant does not hold byte for byte.
func orderExactValue(sel uint8, i int64, s string) bson.RawValue {
	b := bson.NewBuilder()
	switch sel % 8 {
	case 0:
		b.AppendValue("v", bson.RawValue{Type: bson.TypeMinKey})
	case 1:
		b.AppendNull("v")
	case 2:
		b.AppendInt32("v", int32(i))
	case 3:
		b.AppendString("v", s)
	case 4:
		b.AppendBoolean("v", i&1 == 1)
	case 5:
		b.AppendDateTime("v", i)
	case 6:
		var oid sys.ObjectID
		for k := 0; k < 8; k++ {
			oid[k] = byte(i >> (8 * k))
		}
		b.AppendObjectID("v", oid)
	case 7:
		b.AppendValue("v", bson.RawValue{Type: bson.TypeMaxKey})
	}
	v, _ := b.Build().Lookup("v")
	return v
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}

// FuzzKeyEncoderOrder is the core order-preservation property from spec 19 §18: for any two
// values drawn from the order-exact menu, the byte comparison of their encoded keys must agree in
// sign with bson.Compare. A mismatch means the index would order keys differently from the query
// engine, which silently corrupts range scans.
func FuzzKeyEncoderOrder(f *testing.F) {
	f.Add(uint8(2), int64(1), uint8(2), int64(2), "x", "y")
	f.Add(uint8(3), int64(0), uint8(3), int64(0), "abc", "abd")
	f.Add(uint8(0), int64(0), uint8(7), int64(0), "", "")
	f.Add(uint8(5), int64(-5), uint8(5), int64(5), "", "")
	f.Add(uint8(6), int64(100), uint8(2), int64(100), "", "")
	f.Fuzz(func(t *testing.T, selA uint8, iA int64, selB uint8, iB int64, sA, sB string) {
		a := orderExactValue(selA, iA, sA)
		b := orderExactValue(selB, iB, sB)

		ka, err := EncodeValue(a)
		if err != nil {
			t.Fatalf("order-exact value %v failed to encode: %v", a.Type, err)
		}
		kb, err := EncodeValue(b)
		if err != nil {
			t.Fatalf("order-exact value %v failed to encode: %v", b.Type, err)
		}

		want := sign(bson.Compare(a, b))
		got := sign(bytes.Compare(ka, kb))
		if got != want {
			t.Fatalf("order mismatch: Compare(%v,%v)=%d but bytes.Compare(key,key)=%d", a.Type, b.Type, want, got)
		}

		// Descending must invert the order exactly, except for equal keys which stay equal.
		da, err := EncodeField(a, true)
		if err != nil {
			t.Fatal(err)
		}
		db, err := EncodeField(b, true)
		if err != nil {
			t.Fatal(err)
		}
		if got := sign(bytes.Compare(da, db)); got != -want {
			t.Fatalf("descending order mismatch: want %d, got %d for %v vs %v", -want, got, a.Type, b.Type)
		}
	})
}

// coverableValue builds a value from the types AppendDecodedField can reconstruct, so the round-
// trip fuzzer only asserts reversibility where the encoding is documented to be reversible.
// Numbers collapse to double on decode, so it uses an int32 whose float64 image is exact and
// compares back by value (bson.Compare treats int and double of equal magnitude as equal).
func coverableValue(sel uint8, i int64, s string, raw []byte) bson.RawValue {
	b := bson.NewBuilder()
	switch sel % 9 {
	case 0:
		b.AppendValue("v", bson.RawValue{Type: bson.TypeMinKey})
	case 1:
		b.AppendValue("v", bson.RawValue{Type: bson.TypeMaxKey})
	case 2:
		b.AppendNull("v")
	case 3:
		b.AppendBoolean("v", i&1 == 1)
	case 4:
		b.AppendInt32("v", int32(i))
	case 5:
		b.AppendString("v", s)
	case 6:
		b.AppendDateTime("v", i)
	case 7:
		b.AppendTimestamp("v", uint64(i))
	case 8:
		b.AppendBinary("v", byte(i), raw)
	}
	v, _ := b.Build().Lookup("v")
	return v
}

// FuzzKeyRoundTrip checks decode(encode(v)) == v for the coverable types, in both orientations:
// the covered-scan fast path reads values straight back out of the index key, so a value that
// does not survive the round trip would be silently corrupted on a covered read (spec 2061 doc 11
// §5.6). Equality is by bson.Compare, which lets an int32 round-trip through its double image.
func FuzzKeyRoundTrip(f *testing.F) {
	f.Add(uint8(4), int64(42), "x", []byte{1, 2})
	f.Add(uint8(5), int64(0), "hello\x00world", []byte(nil))
	f.Add(uint8(6), int64(1_700_000_000_000), "", []byte(nil))
	f.Add(uint8(8), int64(0), "", []byte{0xde, 0xad, 0xbe, 0xef})
	f.Fuzz(func(t *testing.T, sel uint8, i int64, s string, raw []byte) {
		v := coverableValue(sel, i, s, raw)
		for _, desc := range []bool{false, true} {
			key, err := EncodeField(v, desc)
			if err != nil {
				t.Fatalf("coverable value %v failed to encode (desc=%v): %v", v.Type, desc, err)
			}
			bldr := bson.NewBuilder()
			n, err := AppendDecodedField(bldr, "v", key, desc)
			if err != nil {
				t.Fatalf("decode of %v failed (desc=%v): %v", v.Type, desc, err)
			}
			if n != len(key) {
				t.Fatalf("decode consumed %d of %d bytes for %v (desc=%v)", n, len(key), v.Type, desc)
			}
			got, ok := bldr.Build().Lookup("v")
			if !ok {
				t.Fatalf("decoded document missing the field for %v", v.Type)
			}
			if bson.Compare(v, got) != 0 {
				t.Fatalf("round-trip changed value: %v in, %v out (desc=%v)", v.Type, got.Type, desc)
			}
		}
	})
}
