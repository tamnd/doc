package doc

import (
	"context"
	"testing"

	"github.com/tamnd/doc/options"
)

func TestCollectionStats(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	c := db.Database("shop").Collection("orders")

	for i := range 50 {
		if _, err := c.InsertOne(ctx, M{"_id": i, "qty": i, "sku": "abc"}); err != nil {
			t.Fatalf("InsertOne: %v", err)
		}
	}
	if _, err := c.Indexes().CreateOne(ctx, IndexModel{Keys: M{"qty": 1}}); err != nil {
		t.Fatalf("CreateOne: %v", err)
	}

	st, err := c.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.Namespace != "shop.orders" {
		t.Fatalf("Namespace = %q, want shop.orders", st.Namespace)
	}
	if st.DocumentCount != 50 {
		t.Fatalf("DocumentCount = %d, want 50", st.DocumentCount)
	}
	if st.StorageSize <= 0 {
		t.Fatalf("StorageSize = %d, want > 0", st.StorageSize)
	}
	if st.IndexSize <= 0 {
		t.Fatalf("IndexSize = %d, want > 0", st.IndexSize)
	}
	if st.TotalSize != st.StorageSize+st.IndexSize {
		t.Fatalf("TotalSize = %d, want %d", st.TotalSize, st.StorageSize+st.IndexSize)
	}
	if _, ok := st.IndexSizes["_id_"]; !ok {
		t.Fatalf("IndexSizes missing _id_: %v", st.IndexSizes)
	}
	if _, ok := st.IndexSizes["qty_1"]; !ok {
		t.Fatalf("IndexSizes missing qty_1: %v", st.IndexSizes)
	}
}

func TestCollectionStatsMissing(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Database("shop").Collection("ghost").Stats(context.Background()); err == nil {
		t.Fatal("Stats on a missing collection should error")
	}
}

func TestCappedStatsReportCap(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	d := db.Database("shop")
	if err := d.CreateCollection(ctx, "log", options.CreateCollection().SetCapped(true).SetMaxDocuments(5).SetSizeInBytes(1<<16)); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	c := d.Collection("log")
	for i := range 20 {
		if _, err := c.InsertOne(ctx, M{"_id": i}); err != nil {
			t.Fatalf("InsertOne: %v", err)
		}
	}
	st, err := c.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if !st.Capped {
		t.Fatal("Capped = false, want true")
	}
	if st.MaxDocuments != 5 {
		t.Fatalf("MaxDocuments = %d, want 5", st.MaxDocuments)
	}
	if st.DocumentCount != 5 {
		t.Fatalf("DocumentCount = %d, want 5 (ring evicted)", st.DocumentCount)
	}
}

func TestDatabaseStats(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	d := db.Database("shop")
	for _, name := range []string{"a", "b", "c"} {
		for i := range 10 {
			if _, err := d.Collection(name).InsertOne(ctx, M{"_id": i, "v": i}); err != nil {
				t.Fatalf("InsertOne: %v", err)
			}
		}
	}
	st, err := d.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.Database != "shop" {
		t.Fatalf("Database = %q, want shop", st.Database)
	}
	if st.Collections != 3 {
		t.Fatalf("Collections = %d, want 3", st.Collections)
	}
	if st.DocumentCount != 30 {
		t.Fatalf("DocumentCount = %d, want 30", st.DocumentCount)
	}
	if st.Indexes != 3 {
		t.Fatalf("Indexes = %d, want 3 (one _id per collection)", st.Indexes)
	}
	if st.TotalSize != st.StorageSize+st.IndexSize {
		t.Fatalf("TotalSize = %d, want %d", st.TotalSize, st.StorageSize+st.IndexSize)
	}
}

func BenchmarkCollectionStats(b *testing.B) {
	ctx := context.Background()
	db, err := Open(memoryPath)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	c := db.Database("shop").Collection("orders")
	for i := range 1000 {
		if _, err := c.InsertOne(ctx, M{"_id": i, "v": i}); err != nil {
			b.Fatalf("InsertOne: %v", err)
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := c.Stats(ctx); err != nil {
			b.Fatalf("Stats: %v", err)
		}
	}
}
