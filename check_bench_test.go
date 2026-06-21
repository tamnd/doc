package doc

import (
	"context"
	"testing"
)

// benchSeed fills a database with n documents and a secondary index, the working
// set the check and compact benchmarks walk.
func benchSeed(b *testing.B, n int) *DB {
	b.Helper()
	db, err := Open(memoryPath)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	c := db.Database("d").Collection("c")
	for i := 0; i < n; i++ {
		if _, err := c.InsertOne(ctx, M{"_id": i, "age": i % 50}); err != nil {
			b.Fatalf("insert: %v", err)
		}
	}
	if _, err := c.Indexes().CreateOne(ctx, IndexModel{Keys: M{"age": 1}}); err != nil {
		b.Fatalf("index: %v", err)
	}
	return db
}

// BenchmarkCheckStructural measures the cheap check, which walks the heap and the
// index B-trees but does not re-read every page for its checksum.
func BenchmarkCheckStructural(b *testing.B) {
	db := benchSeed(b, 10000)
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rep, err := db.Check(ctx, false)
		if err != nil || !rep.Valid {
			b.Fatalf("check: err=%v valid=%v", err, rep.Valid)
		}
	}
}

// BenchmarkCheckFull measures the full check, which adds a whole-file checksum
// sweep on top of the structural walk.
func BenchmarkCheckFull(b *testing.B) {
	db := benchSeed(b, 10000)
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rep, err := db.Check(ctx, true)
		if err != nil || !rep.Valid {
			b.Fatalf("check: err=%v valid=%v", err, rep.Valid)
		}
	}
}

// BenchmarkCompact measures an offline rebuild of a database whose live set is a
// fraction of what it once held, the case compaction exists for.
func BenchmarkCompact(b *testing.B) {
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		db := benchSeed(b, 10000)
		c := db.Database("d").Collection("c")
		for j := 0; j < 10000; j++ {
			if j%4 != 0 {
				if _, err := c.DeleteOne(ctx, M{"_id": j}); err != nil {
					b.Fatalf("delete: %v", err)
				}
			}
		}
		b.StartTimer()
		if err := db.Compact(ctx); err != nil {
			b.Fatalf("compact: %v", err)
		}
		b.StopTimer()
		_ = db.Close()
	}
}
