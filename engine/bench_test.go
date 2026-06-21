package engine

import (
	"fmt"
	"testing"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/sys"
	"github.com/tamnd/doc/vfs"
)

func benchEngine(b *testing.B) *Engine {
	b.Helper()
	e, err := Open(vfs.NewMemFS(), "bench.doc", Options{IDGen: &sys.FixedIDGenerator{Timestamp: 1}})
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	b.Cleanup(func() { _ = e.Close() })
	return e
}

func benchDoc(id int64) bson.Raw {
	return bson.NewBuilder().
		AppendInt64("_id", id).
		AppendString("name", "benchmark document").
		Build()
}

// BenchmarkInsertSingleCollection measures the per-insert overhead the engine adds
// over a bare collection: one catalog lookup plus the shared-oracle commit.
func BenchmarkInsertSingleCollection(b *testing.B) {
	e := benchEngine(b)
	c, _ := e.CreateCollection("db", "c")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.InsertOne(benchDoc(int64(i))); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkInsertRoundRobin spreads inserts across many collections in one file,
// exercising the multiplexing path and the shared oracle under interleaved commits.
func BenchmarkInsertRoundRobin(b *testing.B) {
	const nColls = 16
	e := benchEngine(b)
	colls := make([]interface {
		InsertOne(bson.Raw) (bson.RawValue, error)
	}, nColls)
	for i := 0; i < nColls; i++ {
		c, err := e.CreateCollection("db", fmt.Sprintf("c%d", i))
		if err != nil {
			b.Fatal(err)
		}
		colls[i] = c
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := colls[i%nColls].InsertOne(benchDoc(int64(i))); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkReopen measures catalog bootstrap and per-collection recovery cost as a
// function of how many collections a file holds.
func BenchmarkReopen(b *testing.B) {
	for _, n := range []int{1, 16, 64} {
		b.Run(fmt.Sprintf("colls=%d", n), func(b *testing.B) {
			fs := vfs.NewMemFS()
			e, _ := Open(fs, "bench.doc", Options{IDGen: &sys.FixedIDGenerator{Timestamp: 1}})
			for i := 0; i < n; i++ {
				c, _ := e.CreateCollection("db", fmt.Sprintf("c%d", i))
				c.InsertOne(benchDoc(1))
			}
			e.Close()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				e2, err := Open(fs, "bench.doc", Options{IDGen: &sys.FixedIDGenerator{Timestamp: 1}})
				if err != nil {
					b.Fatal(err)
				}
				e2.Close()
			}
		})
	}
}
