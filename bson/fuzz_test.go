package bson

import (
	"bytes"
	"testing"

	"github.com/tamnd/doc/sys"
)

// bsonSeeds returns a spread of valid documents for the fuzzers to mutate from: empty, every
// scalar type, nested documents, arrays, binary with a subtype, and a deeply nested chain.
func bsonSeeds() [][]byte {
	var seeds [][]byte

	seeds = append(seeds, NewBuilder().Build())

	scalars := NewBuilder().
		AppendDouble("d", 3.5).
		AppendString("s", "hello").
		AppendInt32("i32", -7).
		AppendInt64("i64", 1<<40).
		AppendBoolean("b", true).
		AppendNull("nul").
		AppendDateTime("dt", 1_700_000_000_000).
		AppendTimestamp("ts", 0x7fff_0001).
		AppendObjectID("oid", sys.ObjectID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}).
		AppendBinary("bin", 0x04, []byte{0xde, 0xad, 0xbe, 0xef}).
		Build()
	seeds = append(seeds, scalars)

	inner := NewBuilder().AppendString("k", "v").AppendInt32("n", 1).Build()
	arr := NewBuilder().AppendInt32("0", 10).AppendInt32("1", 20).AppendString("2", "x").Build()
	nested := NewBuilder().
		AppendDocument("doc", inner).
		AppendArray("arr", arr).
		Build()
	seeds = append(seeds, nested)

	// A chain of nested documents, near but under MaxDepth, to exercise the recursive walk.
	deep := NewBuilder().AppendInt32("leaf", 1).Build()
	for i := 0; i < 40; i++ {
		deep = NewBuilder().AppendDocument("n", deep).Build()
	}
	seeds = append(seeds, deep)

	return seeds
}

// FuzzRawValidate feeds arbitrary bytes to the document validator and the structural walk. The
// parser is the primary input boundary, so the contract is simple: never panic, never hang,
// never read out of bounds. An error on malformed input is the correct outcome.
func FuzzRawValidate(f *testing.F) {
	for _, s := range bsonSeeds() {
		f.Add(s)
	}
	// A few deliberately broken inputs so the corpus carries the error paths too.
	f.Add([]byte{})
	f.Add([]byte{0x05, 0x00, 0x00, 0x00, 0x00}) // valid empty doc
	f.Add([]byte{0xff, 0xff, 0xff, 0x7f, 0x00}) // huge length, truncated
	f.Add([]byte{0x05, 0x00, 0x00, 0x00, 0x01}) // length ok, missing terminator

	f.Fuzz(func(t *testing.T, data []byte) {
		r := Raw(data)
		// Validate does the deep check (UTF-8, nesting, terminators). It must return cleanly
		// either way.
		validateErr := r.Validate()

		// Elements does the shallow structural walk. It must also stay in bounds.
		elems, elemErr := r.Elements()

		// If the document validated, the structural walk must agree that it is well formed, and
		// walking each value (including descending into nested documents and arrays) must not
		// panic.
		if validateErr == nil {
			if elemErr != nil {
				t.Fatalf("Validate passed but Elements failed: %v", elemErr)
			}
			for _, e := range elems {
				walkValue(t, e.Value, 0)
			}
		}
	})
}

// walkValue descends into a RawValue, recursing through documents and arrays so the fuzzer
// exercises the nested scan paths. It bounds its own depth so a hostile-but-valid input cannot
// drive the test goroutine into its own stack overflow.
func walkValue(t *testing.T, v RawValue, depth int) {
	t.Helper()
	if depth > MaxDepth+2 {
		return
	}
	switch v.Type {
	case TypeDocument, TypeArray:
		// Document() returns the raw bytes for both embedded documents and arrays.
		sub := v.Document()
		els, err := sub.Elements()
		if err != nil {
			return
		}
		for _, e := range els {
			walkValue(t, e.Value, depth+1)
		}
	}
}

// FuzzRawRoundTrip checks the encode(decode(bytes)) == bytes invariant from spec 19 §18.1: a
// document that validates must re-encode to exactly its own bytes when its top-level elements
// are appended verbatim to a fresh builder. A mismatch means the framing is not canonical.
func FuzzRawRoundTrip(f *testing.F) {
	for _, s := range bsonSeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		r := Raw(data)
		if r.Validate() != nil {
			return // only valid documents carry the round-trip guarantee
		}
		elems, err := r.Elements()
		if err != nil {
			t.Fatalf("validated document failed Elements: %v", err)
		}
		b := NewBuilder()
		for _, e := range elems {
			b.AppendValue(e.Key, e.Value)
		}
		got := b.Build()
		if !bytes.Equal(got, r) {
			t.Fatalf("round-trip mismatch:\n in  = %x\n out = %x", []byte(r), []byte(got))
		}
	})
}
