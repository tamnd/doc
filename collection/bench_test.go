package collection

import (
	"testing"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/catalog"
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

// benchKeyDoc builds {_id: id, k: id, name: ...} so an index on k is unique and
// selective, the shape the planner benchmarks compare access paths over.
func benchKeyDoc(id int64) bson.Raw {
	return bson.NewBuilder().
		AppendInt64("_id", id).
		AppendInt64("k", id).
		AppendString("name", "benchmark document").
		Build()
}

// benchKeyColl fills a collection with n documents keyed by k, optionally building
// a secondary index on k first so inserts maintain it.
func benchKeyColl(b *testing.B, n int, withIndex bool) *Collection {
	b.Helper()
	c := benchColl(b)
	if withIndex {
		if _, err := c.CreateIndex(IndexModel{Key: []catalog.KeyPart{{Field: "k"}}}); err != nil {
			b.Fatalf("CreateIndex: %v", err)
		}
	}
	for i := 0; i < n; i++ {
		if _, err := c.InsertOne(benchKeyDoc(int64(i))); err != nil {
			b.Fatal(err)
		}
	}
	return c
}

// BenchmarkFindEqualityCollscan measures a selective equality find with no helpful
// index, so the planner scans the whole collection.
func BenchmarkFindEqualityCollscan(b *testing.B) {
	const n = 10000
	c := benchKeyColl(b, n, false)
	filter := bson.NewBuilder().AppendInt64("k", n/2).Build()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := c.Find(filter)
		if err != nil {
			b.Fatal(err)
		}
		if len(got) != 1 {
			b.Fatalf("got %d docs, want 1", len(got))
		}
	}
}

// BenchmarkFindEqualityIndexScan measures the same find with an index on k, so the
// planner uses an index scan plus a heap fetch.
func BenchmarkFindEqualityIndexScan(b *testing.B) {
	const n = 10000
	c := benchKeyColl(b, n, true)
	filter := bson.NewBuilder().AppendInt64("k", n/2).Build()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := c.Find(filter)
		if err != nil {
			b.Fatal(err)
		}
		if len(got) != 1 {
			b.Fatalf("got %d docs, want 1", len(got))
		}
	}
}

// BenchmarkFindCoveredScan measures a covered find: an index on k answers a
// projection of just k without fetching from the heap.
func BenchmarkFindCoveredScan(b *testing.B) {
	const n = 10000
	c := benchKeyColl(b, n, true)
	filter := bson.NewBuilder().AppendInt64("k", n/2).Build()
	proj := bson.NewBuilder().AppendInt32("k", 1).AppendInt32("_id", 0).Build()
	opts := FindOptions{Projection: proj}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := c.FindWith(filter, opts)
		if err != nil {
			b.Fatal(err)
		}
		if len(got) != 1 {
			b.Fatalf("got %d docs, want 1", len(got))
		}
	}
}

// BenchmarkInsertMany measures a batch insert of 100 documents in one
// transaction against the per-document InsertOne loop the same data would take.
func BenchmarkInsertMany(b *testing.B) {
	const batch = 100
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		c := benchColl(b)
		docs := make([]bson.Raw, batch)
		for j := 0; j < batch; j++ {
			docs[j] = benchDoc(int64(j))
		}
		b.StartTimer()
		if _, err := c.InsertMany(docs, true); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkBulkWriteMixed measures a mixed batch of inserts and updates committed
// together.
func BenchmarkBulkWriteMixed(b *testing.B) {
	const n = 1000
	c := benchColl(b)
	for i := 0; i < n; i++ {
		if _, err := c.InsertOne(benchDoc(int64(i))); err != nil {
			b.Fatal(err)
		}
	}
	upd := bson.NewBuilder().AppendDocument("$inc",
		bson.NewBuilder().AppendInt32("score", 1).Build()).Build()
	ops := make([]BulkOp, 0, 20)
	for i := 0; i < 20; i++ {
		ops = append(ops, UpdateOneOp{Filter: bson.NewBuilder().AppendInt64("_id", int64(i)).Build(), Update: upd})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.BulkWrite(ops, true); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkUpsertInsert measures the upsert insert branch building a document from
// the filter and update.
func BenchmarkUpsertInsert(b *testing.B) {
	upd := bson.NewBuilder().AppendDocument("$set",
		bson.NewBuilder().AppendInt32("score", 1).Build()).Build()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		c := benchColl(b)
		filter := bson.NewBuilder().AppendInt64("k", int64(i)).Build()
		b.StartTimer()
		if _, err := c.UpdateOneWith(filter, upd, UpdateOptions{Upsert: true}); err != nil {
			b.Fatal(err)
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

// BenchmarkWithTransactionSingleWrite measures the session-managed transaction path
// for one write: the per-call cost of StartSession, BeginTx, the body, commit, and
// EndSession over the bare InsertOne the same write would take.
func BenchmarkWithTransactionSingleWrite(b *testing.B) {
	c := benchColl(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := int64(i)
		if err := c.WithTransaction(func(tx *Txn) error {
			_, err := tx.InsertOne(benchDoc(id))
			return err
		}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWithTransactionMultiWrite measures a multi-document transaction: ten
// writes committed atomically through one session-managed transaction, the unit of
// work the session API exists to make cheap relative to ten separate commits.
func BenchmarkWithTransactionMultiWrite(b *testing.B) {
	const batch = 10
	c := benchColl(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		base := int64(i) * batch
		if err := c.WithTransaction(func(tx *Txn) error {
			for j := int64(0); j < batch; j++ {
				if _, err := tx.InsertOne(benchDoc(base + j)); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSessionReuse measures reusing one session across many transactions, the
// path that amortizes session allocation when a caller drives a stream of
// transactions back to back.
func BenchmarkSessionReuse(b *testing.B) {
	c := benchColl(b)
	s := c.StartSession()
	defer s.EndSession()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.StartTransaction(); err != nil {
			b.Fatal(err)
		}
		if _, err := s.Transaction().InsertOne(benchDoc(int64(i))); err != nil {
			b.Fatal(err)
		}
		if err := s.CommitTransaction(); err != nil {
			b.Fatal(err)
		}
	}
}
