package colstore

import (
	"testing"

	"github.com/tamnd/doc/bson"
)

// buildStore fills a store with n rows: a 20-way group category and an amount in a
// tight range, the shape an analytical $group hits (spec 2061 doc 19 §22 target:
// $group over 1M docs under 50 ms p50 with the column store on).
func buildStore(n int) *Store {
	s := New(ModeTransactional, []string{"cat", "amt"})
	cats := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j",
		"k", "l", "m", "n", "o", "p", "q", "r", "s", "t"}
	for i := 0; i < n; i++ {
		d := bson.NewBuilder().
			AppendInt32("_id", int32(i)).
			AppendString("cat", cats[i%len(cats)]).
			AppendInt32("amt", int32(i%1000)).
			Build()
		s.Insert(d, uint64(i+1))
	}
	s.Flush()
	return s
}

func BenchmarkGroupSum1M(b *testing.B) {
	s := buildStore(1_000_000)
	snap := uint64(2_000_000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g := s.GroupBy(snap, "cat", "amt", nil)
		if len(g) != 20 {
			b.Fatalf("got %d groups, want 20", len(g))
		}
	}
}

func BenchmarkSumField1M(b *testing.B) {
	s := buildStore(1_000_000)
	snap := uint64(2_000_000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sum, count := s.SumField(snap, "amt", nil)
		if count != 1_000_000 {
			b.Fatalf("count = %d", count)
		}
		_ = sum
	}
}

func BenchmarkSumFieldPredicate1M(b *testing.B) {
	s := buildStore(1_000_000)
	snap := uint64(2_000_000)
	pred := &RangePred{Field: "amt", Op: "$gte", Bound: Value{Kind: KindInt, I: 500}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.SumField(snap, "amt", pred)
	}
}

func BenchmarkEncodeColumnInts(b *testing.B) {
	vals := make([]Value, SegmentSize)
	for i := range vals {
		vals[i] = Value{Kind: KindInt, I: int64(i % 100)}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = EncodeColumn(vals)
	}
}
