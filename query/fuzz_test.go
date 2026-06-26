package query

import (
	"testing"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/sys"
)

// filterSeeds returns a spread of valid MQL filter documents covering equality, comparison
// operators, logical combinators, element and array operators, and nesting, so the fuzzer has a
// structured base to mutate from rather than starting from random noise.
func filterSeeds() [][]byte {
	var seeds [][]byte

	seeds = append(seeds, bson.NewBuilder().Build())
	seeds = append(seeds, bson.NewBuilder().AppendInt32("a", 1).Build())
	seeds = append(seeds, bson.NewBuilder().AppendString("name", "bob").Build())

	gt := bson.NewBuilder().AppendInt32("$gt", 5).AppendInt32("$lte", 100).Build()
	seeds = append(seeds, bson.NewBuilder().AppendDocument("age", gt).Build())

	in := bson.NewBuilder().AppendInt32("0", 1).AppendInt32("1", 2).AppendInt32("2", 3).Build()
	inOp := bson.NewBuilder().AppendArray("$in", in).Build()
	seeds = append(seeds, bson.NewBuilder().AppendDocument("x", inOp).Build())

	c1 := bson.NewBuilder().AppendInt32("a", 1).Build()
	c2 := bson.NewBuilder().AppendString("b", "y").Build()
	andArr := bson.NewBuilder().AppendDocument("0", c1).AppendDocument("1", c2).Build()
	seeds = append(seeds, bson.NewBuilder().AppendArray("$and", andArr).Build())
	seeds = append(seeds, bson.NewBuilder().AppendArray("$or", andArr).Build())

	exists := bson.NewBuilder().AppendBoolean("$exists", true).Build()
	seeds = append(seeds, bson.NewBuilder().AppendDocument("opt", exists).Build())

	typeOp := bson.NewBuilder().AppendInt32("$type", 2).Build()
	seeds = append(seeds, bson.NewBuilder().AppendDocument("v", typeOp).Build())

	regex := bson.NewBuilder().AppendString("$regex", "^foo").AppendString("$options", "i").Build()
	seeds = append(seeds, bson.NewBuilder().AppendDocument("s", regex).Build())

	dotted := bson.NewBuilder().AppendInt32("a.b.c", 7).Build()
	seeds = append(seeds, dotted)

	return seeds
}

// sampleDocs returns a handful of documents to match compiled filters against, so the fuzzer
// drives the evaluator (Match), not just the compiler. The shapes deliberately overlap with the
// fields the filter seeds reference.
func sampleDocs() []bson.Raw {
	nested := bson.NewBuilder().AppendInt32("c", 7).Build()
	b := bson.NewBuilder().AppendInt32("b", 0).Build()
	arr := bson.NewBuilder().AppendInt32("0", 1).AppendInt32("1", 2).Build()
	return []bson.Raw{
		bson.NewBuilder().Build(),
		bson.NewBuilder().AppendInt32("a", 1).AppendString("name", "bob").AppendInt32("age", 42).Build(),
		bson.NewBuilder().AppendString("name", "alice").AppendArray("x", arr).Build(),
		bson.NewBuilder().AppendDocument("a", bson.NewBuilder().AppendDocument("b", nested).Build()).Build(),
		bson.NewBuilder().AppendObjectID("_id", sys.ObjectID{1}).AppendDouble("v", 3.5).AppendBoolean("opt", true).Build(),
		bson.NewBuilder().AppendDocument("a", b).AppendString("s", "foobar").Build(),
	}
}

// FuzzMQLFilter compiles arbitrary bytes as an MQL filter and, when compilation succeeds,
// evaluates the matcher against every sample document. The contract from spec 19 §18 is that
// neither Compile nor Match may panic, hang, or read out of bounds on any input. A compile error
// on a malformed filter is the correct outcome; a panic is a bug.
func FuzzMQLFilter(f *testing.F) {
	for _, s := range filterSeeds() {
		f.Add(s)
	}
	docs := sampleDocs()
	f.Fuzz(func(t *testing.T, data []byte) {
		raw := bson.Raw(data)
		// A filter must be a structurally valid BSON document before it can be a filter. Skip
		// inputs that are not even well formed BSON; the BSON fuzzer owns that surface.
		if raw.Validate() != nil {
			return
		}
		m, err := Compile(raw)
		if err != nil {
			return // a rejected filter is fine, as long as it did not panic
		}
		for _, d := range docs {
			// The result value is irrelevant; we only require that evaluation stays in bounds.
			_ = m.Match(d)
		}
	})
}
