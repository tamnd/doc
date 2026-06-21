package doc

import (
	"context"
	"testing"
)

// BenchmarkDBCheckpoint measures an online checkpoint through the public DB surface,
// dirtying one document per iteration so each checkpoint has a WAL to fold.
func BenchmarkDBCheckpoint(b *testing.B) {
	ctx := context.Background()
	db, err := Open(memoryPath, WithSyncLevel(SyncOff))
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	c := db.Database("d").Collection("c")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		if _, err := c.InsertOne(ctx, M{"v": i}); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if err := db.Checkpoint(ctx, ""); err != nil {
			b.Fatal(err)
		}
	}
}
