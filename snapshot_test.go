package doc

import (
	"context"
	"errors"
	"testing"
)

func TestWithSnapshotReadsConsistently(t *testing.T) {
	db := openTestDB(t)
	coll := db.Database("shop").Collection("items")
	ctx := context.Background()
	for i := range 3 {
		if _, err := coll.InsertOne(ctx, M{"_id": i, "n": i}); err != nil {
			t.Fatalf("seed insert %d: %v", i, err)
		}
	}

	var seen int64
	err := db.WithSnapshot(ctx, func(sctx context.Context) error {
		n, err := coll.CountDocuments(sctx, M{})
		if err != nil {
			return err
		}
		// A write committed on a separate context after the snapshot opened must
		// not change what the snapshot sees.
		if _, err := db.Database("shop").Collection("items").InsertOne(ctx, M{"_id": 99}); err != nil {
			return err
		}
		after, err := coll.CountDocuments(sctx, M{})
		if err != nil {
			return err
		}
		if n != after {
			t.Errorf("snapshot count changed mid-read: %d then %d", n, after)
		}
		seen = n
		return nil
	})
	if err != nil {
		t.Fatalf("WithSnapshot: %v", err)
	}
	if seen != 3 {
		t.Fatalf("snapshot saw %d documents, want 3", seen)
	}
}

func TestWithSnapshotCommitsWrites(t *testing.T) {
	db := openTestDB(t)
	coll := db.Database("shop").Collection("items")
	ctx := context.Background()
	err := db.WithSnapshot(ctx, func(sctx context.Context) error {
		_, err := coll.InsertOne(sctx, M{"_id": 1, "n": 1})
		return err
	})
	if err != nil {
		t.Fatalf("WithSnapshot: %v", err)
	}
	n, err := coll.CountDocuments(ctx, M{})
	if err != nil {
		t.Fatalf("CountDocuments: %v", err)
	}
	if n != 1 {
		t.Fatalf("write inside snapshot not committed, count = %d", n)
	}
}

func TestWithSnapshotRollsBackOnError(t *testing.T) {
	db := openTestDB(t)
	coll := db.Database("shop").Collection("items")
	ctx := context.Background()
	sentinel := errors.New("boom")
	err := db.WithSnapshot(ctx, func(sctx context.Context) error {
		if _, err := coll.InsertOne(sctx, M{"_id": 1}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithSnapshot error = %v, want sentinel", err)
	}
	n, err := coll.CountDocuments(ctx, M{})
	if err != nil {
		t.Fatalf("CountDocuments: %v", err)
	}
	if n != 0 {
		t.Fatalf("write inside aborted snapshot leaked, count = %d", n)
	}
}

func BenchmarkWithSnapshot(b *testing.B) {
	db, err := Open(memoryPath)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	coll := db.Database("shop").Collection("items")
	ctx := context.Background()
	for i := range 1000 {
		if _, err := coll.InsertOne(ctx, M{"_id": i}); err != nil {
			b.Fatalf("seed: %v", err)
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		err := db.WithSnapshot(ctx, func(sctx context.Context) error {
			_, err := coll.CountDocuments(sctx, M{})
			return err
		})
		if err != nil {
			b.Fatalf("WithSnapshot: %v", err)
		}
	}
}

func TestWithSnapshotClosedDB(t *testing.T) {
	db, err := Open(memoryPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := db.WithSnapshot(context.Background(), func(context.Context) error { return nil }); err == nil {
		t.Fatal("WithSnapshot on a closed DB should fail")
	}
}
