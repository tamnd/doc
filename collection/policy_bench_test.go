package collection

import (
	"testing"
	"time"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/catalog"
	"github.com/tamnd/doc/schema"
)

// benchValidator compiles the require-age $jsonSchema used by the validation
// benchmarks; a compile failure aborts the benchmark.
func benchValidator(b *testing.B) *schema.Validator {
	b.Helper()
	props := bson.NewBuilder().
		AppendDocument("score", bson.NewBuilder().AppendString("bsonType", "int").Build()).
		Build()
	js := bson.NewBuilder().
		AppendString("bsonType", "object").
		AppendArray("required", bson.NewBuilder().AppendString("0", "score").Build()).
		AppendDocument("properties", props).
		Build()
	raw := bson.NewBuilder().AppendDocument("$jsonSchema", js).Build()
	v, err := schema.Compile(raw)
	if err != nil {
		b.Fatalf("compile validator: %v", err)
	}
	return v
}

// BenchmarkInsertValidated measures the per-insert cost with a strict $jsonSchema
// validator installed, the overhead to compare against BenchmarkInsertOne.
func BenchmarkInsertValidated(b *testing.B) {
	c := benchColl(b)
	c.SetPolicy(Policy{
		Validator:        benchValidator(b),
		ValidationLevel:  catalog.ValidationStrict,
		ValidationAction: catalog.ValidationError,
	})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.InsertOne(benchDoc(int64(i))); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkInsertCapped measures inserts into a capped collection held at a small
// document cap, so every insert past the cap also evicts the oldest document.
func BenchmarkInsertCapped(b *testing.B) {
	c := benchColl(b)
	c.SetPolicy(Policy{Capped: true, CappedMaxDocs: 1000})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.InsertOne(benchDoc(int64(i))); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSweepTTL measures one full expiry pass over a collection where every
// document is already expired, the worst case for the read-then-delete sweeper.
func BenchmarkSweepTTL(b *testing.B) {
	const n = 1000
	now := time.UnixMilli(10_000_000)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		c := benchColl(b)
		if _, err := c.CreateIndex(IndexModel{
			Key:                []catalog.KeyPart{{Field: "createdAt"}},
			ExpireAfterSeconds: 1,
		}); err != nil {
			b.Fatalf("CreateIndex: %v", err)
		}
		for j := int64(0); j < n; j++ {
			d := bson.NewBuilder().
				AppendInt64("_id", j).
				AppendDateTime("createdAt", 0).
				Build()
			if _, err := c.InsertOne(d); err != nil {
				b.Fatalf("insert: %v", err)
			}
		}
		b.StartTimer()
		if _, err := c.SweepTTL(now); err != nil {
			b.Fatalf("SweepTTL: %v", err)
		}
	}
}
