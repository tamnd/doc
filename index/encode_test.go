package index

import (
	"bytes"
	"testing"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/sys"
)

// keyOf encodes a single-field document's value as an index key.
func keyOf(t *testing.T, build func(*bson.Builder) *bson.Builder) []byte {
	t.Helper()
	doc := build(bson.NewBuilder()).Build()
	v, ok := doc.Lookup("k")
	if !ok {
		t.Fatalf("field k missing")
	}
	k, err := EncodeValue(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return k
}

// TestEncodeValueMatchesTypedHelpers checks the general EncodeValue agrees byte
// for byte with the typed Encode* helpers for the _id types M1 already shipped.
func TestEncodeValueMatchesTypedHelpers(t *testing.T) {
	oid := sys.ObjectID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	cases := []struct {
		name string
		got  []byte
		want []byte
	}{
		{"objectid", keyOf(t, func(b *bson.Builder) *bson.Builder { return b.AppendObjectID("k", oid) }), EncodeObjectID(oid)},
		{"int32", keyOf(t, func(b *bson.Builder) *bson.Builder { return b.AppendInt32("k", 7) }), EncodeInt64(7)},
		{"int64", keyOf(t, func(b *bson.Builder) *bson.Builder { return b.AppendInt64("k", 7) }), EncodeInt64(7)},
		{"double", keyOf(t, func(b *bson.Builder) *bson.Builder { return b.AppendDouble("k", 7) }), EncodeFloat64(7)},
		{"bool", keyOf(t, func(b *bson.Builder) *bson.Builder { return b.AppendBoolean("k", true) }), EncodeBool(true)},
		{"null", keyOf(t, func(b *bson.Builder) *bson.Builder { return b.AppendNull("k") }), EncodeNull()},
		{"string", keyOf(t, func(b *bson.Builder) *bson.Builder { return b.AppendString("k", "hi") }), EncodeString("hi")},
	}
	for _, c := range cases {
		if !bytes.Equal(c.got, c.want) {
			t.Errorf("%s: EncodeValue=%x typed=%x", c.name, c.got, c.want)
		}
	}
}

// TestNumericOrderPreserved checks the sign-flip transform orders numbers
// bytewise the same way they order arithmetically, including negatives and the
// Int32/Int64/Double cross-type agreement.
func TestNumericOrderPreserved(t *testing.T) {
	nums := []float64{-1e300, -42.5, -1, 0, 0.5, 1, 42, 1e300}
	var prev []byte
	for i, n := range nums {
		k := keyOf(t, func(b *bson.Builder) *bson.Builder { return b.AppendDouble("k", n) })
		if i > 0 && bytes.Compare(prev, k) > 0 {
			t.Fatalf("order broken at %v: %x !< %x", n, prev, k)
		}
		prev = k
	}
	// Equal value across numeric types encodes identically.
	a := keyOf(t, func(b *bson.Builder) *bson.Builder { return b.AppendInt32("k", 100) })
	b32 := keyOf(t, func(b *bson.Builder) *bson.Builder { return b.AppendInt64("k", 100) })
	c := keyOf(t, func(b *bson.Builder) *bson.Builder { return b.AppendDouble("k", 100) })
	if !bytes.Equal(a, b32) || !bytes.Equal(b32, c) {
		t.Fatalf("cross-type numeric mismatch: %x %x %x", a, b32, c)
	}
}

// TestStringOrderAndEscaping checks lexicographic order and that an embedded NUL
// is escaped so it cannot collide with the terminator.
func TestStringOrderAndEscaping(t *testing.T) {
	strs := []string{"", "a", "ab", "b", "ba"}
	var prev []byte
	for i, s := range strs {
		k := keyOf(t, func(b *bson.Builder) *bson.Builder { return b.AppendString("k", s) })
		if i > 0 && bytes.Compare(prev, k) >= 0 {
			t.Fatalf("string order broken at %q", s)
		}
		prev = k
	}
	// "a\x00" must not encode as a prefix-ambiguous form of "a".
	withNul := keyOf(t, func(b *bson.Builder) *bson.Builder { return b.AppendString("k", "a\x00") })
	plain := keyOf(t, func(b *bson.Builder) *bson.Builder { return b.AppendString("k", "a") })
	if bytes.Equal(withNul, plain) {
		t.Fatalf("NUL escaping collision")
	}
}

// TestTypeBracketOrdering checks values sort by type bracket: null < number <
// string < objectId < bool < date per the tag bytes (spec doc 07 §3.3).
func TestTypeBracketOrdering(t *testing.T) {
	keys := [][]byte{
		keyOf(t, func(b *bson.Builder) *bson.Builder { return b.AppendNull("k") }),
		keyOf(t, func(b *bson.Builder) *bson.Builder { return b.AppendInt32("k", 9999) }),
		keyOf(t, func(b *bson.Builder) *bson.Builder { return b.AppendString("k", "z") }),
		keyOf(t, func(b *bson.Builder) *bson.Builder {
			return b.AppendObjectID("k", sys.ObjectID{0xff})
		}),
		keyOf(t, func(b *bson.Builder) *bson.Builder { return b.AppendBoolean("k", true) }),
		keyOf(t, func(b *bson.Builder) *bson.Builder { return b.AppendDateTime("k", 1) }),
	}
	for i := 1; i < len(keys); i++ {
		if bytes.Compare(keys[i-1], keys[i]) >= 0 {
			t.Fatalf("type bracket order broken at index %d: %x !< %x", i, keys[i-1], keys[i])
		}
	}
}

// TestDocumentKeyInjective checks distinct embedded documents encode to distinct
// keys and that field order does not change the encoding (documents are sorted).
func TestDocumentKeyInjective(t *testing.T) {
	d1 := bson.NewBuilder().AppendInt32("a", 1).AppendInt32("b", 2).Build()
	d2 := bson.NewBuilder().AppendInt32("b", 2).AppendInt32("a", 1).Build()
	d3 := bson.NewBuilder().AppendInt32("a", 1).AppendInt32("b", 3).Build()

	k1 := keyOf(t, func(b *bson.Builder) *bson.Builder { return b.AppendDocument("k", d1) })
	k2 := keyOf(t, func(b *bson.Builder) *bson.Builder { return b.AppendDocument("k", d2) })
	k3 := keyOf(t, func(b *bson.Builder) *bson.Builder { return b.AppendDocument("k", d3) })

	if !bytes.Equal(k1, k2) {
		t.Fatalf("field order changed document key: %x vs %x", k1, k2)
	}
	if bytes.Equal(k1, k3) {
		t.Fatalf("distinct documents collided: %x", k1)
	}
}

// TestUnindexableTypeRejected checks the deprecated wire types have no key.
func TestUnindexableTypeRejected(t *testing.T) {
	v := bson.RawValue{Type: bson.TypeDBPointer, Data: []byte{0x01, 0x00, 0x00, 0x00, 0x00}}
	if _, err := EncodeValue(v); err != ErrUnindexableType {
		t.Fatalf("DBPointer: got %v, want ErrUnindexableType", err)
	}
}
