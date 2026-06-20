package collection

import (
	"testing"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/sys"
	"github.com/tamnd/doc/vfs"
)

func benchColl(b *testing.B) *Collection {
	b.Helper()
	c, err := Open(vfs.NewMemFS(), "bench.doc", Options{IDGen: &sys.FixedIDGenerator{Timestamp: 1}})
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { _ = c.Close() })
	return c
}

func benchDoc(id int64) bson.Raw {
	return bson.NewBuilder().
		AppendInt64("_id", id).
		AppendString("name", "benchmark document").
		AppendInt32("score", 42).
		Build()
}

func BenchmarkInsertOne(b *testing.B) {
	c := benchColl(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.InsertOne(benchDoc(int64(i))); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFindOneByID(b *testing.B) {
	c := benchColl(b)
	const n = 10000
	for i := 0; i < n; i++ {
		if _, err := c.InsertOne(benchDoc(int64(i))); err != nil {
			b.Fatal(err)
		}
	}
	filter := bson.NewBuilder().AppendInt64("_id", n/2).Build()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := c.FindOne(filter)
		if err != nil {
			b.Fatal(err)
		}
		if got == nil {
			b.Fatal("missing document")
		}
	}
}

func BenchmarkUpdateOne(b *testing.B) {
	c := benchColl(b)
	const n = 10000
	for i := 0; i < n; i++ {
		if _, err := c.InsertOne(benchDoc(int64(i))); err != nil {
			b.Fatal(err)
		}
	}
	filter := bson.NewBuilder().AppendInt64("_id", n/2).Build()
	upd := bson.NewBuilder().AppendDocument("$inc",
		bson.NewBuilder().AppendInt32("score", 1).Build()).Build()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := c.UpdateOne(filter, upd)
		if err != nil {
			b.Fatal(err)
		}
		if res.Modified != 1 {
			b.Fatalf("modified = %d, want 1", res.Modified)
		}
	}
}

func BenchmarkCountDocuments(b *testing.B) {
	c := benchColl(b)
	const n = 10000
	for i := 0; i < n; i++ {
		if _, err := c.InsertOne(benchDoc(int64(i))); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got, _ := c.CountDocuments(nil); got != n {
			b.Fatalf("count: got %d, want %d", got, n)
		}
	}
}
