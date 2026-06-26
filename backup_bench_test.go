package doc

import (
	"context"
	"io"
	"testing"
)

// BenchmarkDBBackup measures a full physical backup through the public DB surface:
// checkpoint then stream every page to a discarding writer. The data is seeded once
// outside the timer so each iteration copies the same image.
func BenchmarkDBBackup(b *testing.B) {
	ctx := context.Background()
	db, err := Open(memoryPath, WithSyncLevel(SyncOff))
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	c := db.Database("d").Collection("c")
	for i := 0; i < 5000; i++ {
		if _, err := c.InsertOne(ctx, M{"_id": i, "n": i % 11}); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.Backup(ctx, io.Discard, BackupOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}
