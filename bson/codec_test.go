package bson

import (
	"bytes"
	"errors"
	"testing"

	"github.com/tamnd/doc/sys"
)

// TestWorkedHexExample reproduces the {a: int32 1, b: "hello"} document from the
// spec (doc 02 §4.4) byte for byte, then reads it back.
func TestWorkedHexExample(t *testing.T) {
	want := []byte{
		0x19, 0x00, 0x00, 0x00, // length 25
		0x10, 0x61, 0x00, 0x01, 0x00, 0x00, 0x00, // int32 a = 1
		0x02, 0x62, 0x00, 0x06, 0x00, 0x00, 0x00, 0x68, 0x65, 0x6c, 0x6c, 0x6f, 0x00, // string b = "hello"
		0x00, // terminator
	}
	got := NewBuilder().AppendInt32("a", 1).AppendString("b", "hello").Build()
	if !bytes.Equal(got, want) {
		t.Fatalf("encoded\n got %x\nwant %x", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	a, ok := got.Lookup("a")
	if !ok || a.Type != TypeInt32 || a.Int32() != 1 {
		t.Fatalf("lookup a = %+v ok=%v", a, ok)
	}
	b, ok := got.Lookup("b")
	if !ok || b.Type != TypeString || b.StringValue() != "hello" {
		t.Fatalf("lookup b = %+v ok=%v", b, ok)
	}
}

func TestRoundTripAllScalarTypes(t *testing.T) {
	oid := sys.ObjectID{0x50, 0x7f, 0x1f, 0x77, 0xbc, 0xf8, 0x6c, 0xd7, 0x99, 0x43, 0x90, 0x11}
	nested := NewBuilder().AppendInt32("x", 7).Build()
	doc := NewBuilder().
		AppendDouble("d", 3.14).
		AppendString("s", "héllo").
		AppendInt32("i32", -5).
		AppendInt64("i64", 1<<40).
		AppendObjectID("oid", oid).
		AppendBoolean("bt", true).
		AppendBoolean("bf", false).
		AppendDateTime("dt", 1350000000000).
		AppendTimestamp("ts", (uint64(42)<<32)|7).
		AppendNull("nil").
		AppendBinary("bin", 0x04, []byte{1, 2, 3, 4}).
		AppendDocument("sub", nested).
		Build()

	if err := doc.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	elems, err := doc.Elements()
	if err != nil {
		t.Fatalf("elements: %v", err)
	}
	if len(elems) != 12 {
		t.Fatalf("got %d elements, want 12", len(elems))
	}

	check := func(key string, fn func(RawValue) bool) {
		v, ok := doc.Lookup(key)
		if !ok || !fn(v) {
			t.Fatalf("field %q failed: %+v ok=%v", key, v, ok)
		}
	}
	check("d", func(v RawValue) bool { return v.Double() == 3.14 })
	check("s", func(v RawValue) bool { return v.StringValue() == "héllo" })
	check("i32", func(v RawValue) bool { return v.Int32() == -5 })
	check("i64", func(v RawValue) bool { return v.Int64() == 1<<40 })
	check("oid", func(v RawValue) bool { return v.ObjectID() == oid })
	check("bt", func(v RawValue) bool { return v.Boolean() })
	check("bf", func(v RawValue) bool { return !v.Boolean() })
	check("dt", func(v RawValue) bool { return v.DateTime() == 1350000000000 })
	check("ts", func(v RawValue) bool { return v.Timestamp() == (uint64(42)<<32)|7 })
	check("nil", func(v RawValue) bool { return v.Type == TypeNull })
	check("bin", func(v RawValue) bool {
		sub, data, ok := v.Binary()
		return ok && sub == 0x04 && bytes.Equal(data, []byte{1, 2, 3, 4})
	})
	check("sub", func(v RawValue) bool {
		s := v.Document()
		x, ok := s.Lookup("x")
		return ok && x.Int32() == 7
	})
}

func TestValidateRejectsBadDocuments(t *testing.T) {
	// Invalid UTF-8 in a string value.
	bad := NewBuilder().AppendString("s", string([]byte{0xff, 0xfe})).Build()
	if err := bad.Validate(); !errors.Is(err, ErrInvalidUTF8) {
		t.Fatalf("invalid utf8: got %v", err)
	}
	// A length prefix that disagrees with the slice.
	short := Raw{0x06, 0x00, 0x00, 0x00, 0x00}
	if err := short.Validate(); !errors.Is(err, ErrLengthMismatch) {
		t.Fatalf("length mismatch: got %v", err)
	}
	// Deep nesting beyond MaxDepth.
	deep := Empty
	for i := 0; i <= MaxDepth; i++ {
		deep = NewBuilder().AppendDocument("a", deep).Build()
	}
	if err := deep.Validate(); !errors.Is(err, ErrTooDeep) {
		t.Fatalf("too deep: got %v", err)
	}
}

func TestEnsureIDGeneratesAndMovesFirst(t *testing.T) {
	gen := &sys.FixedIDGenerator{Timestamp: 1000}

	// No _id: one is minted and placed first.
	doc := NewBuilder().AppendString("name", "ada").Build()
	out, idv, err := EnsureID(doc, gen)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if idv.Type != TypeObjectID {
		t.Fatalf("minted id type = %v", idv.Type)
	}
	if first, _ := out.Elements(); first[0].Key != "_id" {
		t.Fatalf("first field = %q, want _id", first[0].Key)
	}

	// _id present but not first: it moves to first, value preserved.
	doc2 := NewBuilder().AppendString("name", "ada").AppendInt32("_id", 42).Build()
	out2, idv2, err := EnsureID(doc2, gen)
	if err != nil {
		t.Fatalf("ensure2: %v", err)
	}
	if idv2.Type != TypeInt32 || idv2.Int32() != 42 {
		t.Fatalf("preserved id = %+v", idv2)
	}
	elems, _ := out2.Elements()
	if elems[0].Key != "_id" || elems[1].Key != "name" {
		t.Fatalf("field order = %q,%q", elems[0].Key, elems[1].Key)
	}
}

func TestEnsureIDRejectsBadIDType(t *testing.T) {
	gen := &sys.FixedIDGenerator{}
	doc := NewBuilder().AppendNull("_id").Build()
	if _, _, err := EnsureID(doc, gen); !errors.Is(err, ErrInvalidIDType) {
		t.Fatalf("null _id: got %v, want ErrInvalidIDType", err)
	}
}
