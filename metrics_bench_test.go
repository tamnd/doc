package doc

import (
	"context"
	"io"
	"testing"
	"time"
)

func BenchmarkRecordOp(b *testing.B) {
	m := newDBMetrics()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		m.recordOp("find", "app.users", 250*time.Microsecond, 10, 5, 10, 100*time.Millisecond)
	}
}

func BenchmarkObserve(b *testing.B) {
	db, err := Open(memoryPath)
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	c := db.Database("app").Collection("users")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		rec := c.observe("find")
		rec(0, 1, 0)
	}
}

func BenchmarkRefreshMetrics(b *testing.B) {
	db, err := Open(memoryPath)
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	c := db.Database("app").Collection("users")
	for i := 0; i < 100; i++ {
		_, _ = c.InsertOne(context.Background(), M{"i": i})
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		db.refreshMetrics()
	}
}

func BenchmarkWritePrometheusDB(b *testing.B) {
	db, err := Open(memoryPath)
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	c := db.Database("app").Collection("users")
	for i := 0; i < 100; i++ {
		_, _ = c.InsertOne(context.Background(), M{"i": i})
	}
	ctx := context.Background()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = db.WritePrometheus(ctx, io.Discard)
	}
}
