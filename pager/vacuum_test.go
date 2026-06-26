package pager

import (
	"testing"

	"github.com/tamnd/doc/vfs"
)

// allocN allocates n pages, each stamped with a body byte derived from its index,
// commits, and returns the ids. The first page gets byte 1, the second 2, and so on.
func allocN(t *testing.T, p *Pager, n int) []uint64 {
	t.Helper()
	ids := make([]uint64, n)
	for i := range ids {
		ids[i] = allocFilled(t, p, byte(i+1))
	}
	if err := p.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return ids
}

// freePages frees the named pages and commits.
func freePages(t *testing.T, p *Pager, ids ...uint64) {
	t.Helper()
	for _, id := range ids {
		if err := p.Free(id); err != nil {
			t.Fatalf("free %d: %v", id, err)
		}
	}
	if err := p.Commit(); err != nil {
		t.Fatalf("commit free: %v", err)
	}
}

func TestIncrementalVacuumReclaimsTrailingPages(t *testing.T) {
	fs := vfs.NewMemFS()
	p := mustOpen(t, fs, Options{Sync: SyncFull, CheckpointFrames: 1 << 20})
	defer p.Close()

	ids := allocN(t, p, 6) // pages 1..6, PageCount 7
	if got := p.PageCount(); got != 7 {
		t.Fatalf("page count after alloc = %d, want 7", got)
	}
	// Free the three pages at the tail of the file.
	freePages(t, p, ids[5], ids[4], ids[3])

	reclaimed, err := p.IncrementalVacuum(0)
	if err != nil {
		t.Fatalf("vacuum: %v", err)
	}
	if reclaimed != 3 {
		t.Fatalf("reclaimed = %d, want 3", reclaimed)
	}
	if got := p.PageCount(); got != 4 {
		t.Fatalf("page count after vacuum = %d, want 4 (header + 3 live)", got)
	}
	if got := p.Stats().FreelistPages; got != 0 {
		t.Fatalf("freelist pages after vacuum = %d, want 0", got)
	}
	// The file on disk shrank to the new page count.
	wantBytes := int64(4) * int64(p.Stats().PageSize)
	if got := int64(len(fs.Snapshot(dbPath))); got != wantBytes {
		t.Fatalf("main file size = %d, want %d", got, wantBytes)
	}
	// The surviving live pages still read back correctly.
	readBody(t, p, ids[0], 1)
	readBody(t, p, ids[1], 2)
	readBody(t, p, ids[2], 3)
}

func TestIncrementalVacuumHonorsBound(t *testing.T) {
	fs := vfs.NewMemFS()
	p := mustOpen(t, fs, Options{Sync: SyncFull, CheckpointFrames: 1 << 20})
	defer p.Close()

	ids := allocN(t, p, 6)
	freePages(t, p, ids[5], ids[4], ids[3])

	reclaimed, err := p.IncrementalVacuum(2)
	if err != nil {
		t.Fatalf("vacuum: %v", err)
	}
	if reclaimed != 2 {
		t.Fatalf("reclaimed = %d, want 2 (bounded)", reclaimed)
	}
	if got := p.PageCount(); got != 5 {
		t.Fatalf("page count = %d, want 5", got)
	}
	// One free page is left below the new tail, still on the freelist.
	if got := p.Stats().FreelistPages; got != 1 {
		t.Fatalf("freelist pages = %d, want 1", got)
	}
}

func TestIncrementalVacuumSkipsLiveTail(t *testing.T) {
	fs := vfs.NewMemFS()
	p := mustOpen(t, fs, Options{Sync: SyncFull, CheckpointFrames: 1 << 20})
	defer p.Close()

	ids := allocN(t, p, 6)
	// Free a page in the middle; the tail page stays live so nothing is reclaimable.
	freePages(t, p, ids[2])

	reclaimed, err := p.IncrementalVacuum(0)
	if err != nil {
		t.Fatalf("vacuum: %v", err)
	}
	if reclaimed != 0 {
		t.Fatalf("reclaimed = %d, want 0 (tail is live)", reclaimed)
	}
	if got := p.PageCount(); got != 7 {
		t.Fatalf("page count = %d, want 7 (unchanged)", got)
	}
	// The freed middle page survived on the freelist and is recycled next.
	reused, _, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if reused != ids[2] {
		t.Fatalf("reused page = %d, want recycled middle page %d", reused, ids[2])
	}
}

// TestIncrementalVacuumRecovers checks that the reclaimed state is durable: after a
// vacuum, recovering from the surviving main and WAL bytes sees the lower page count
// and the live pages, with the trailing pages gone.
func TestIncrementalVacuumRecovers(t *testing.T) {
	fs := vfs.NewMemFS()
	p := mustOpen(t, fs, Options{Sync: SyncFull, CheckpointFrames: 1 << 20})
	ids := allocN(t, p, 6)
	freePages(t, p, ids[5], ids[4], ids[3])
	if _, err := p.IncrementalVacuum(0); err != nil {
		t.Fatalf("vacuum: %v", err)
	}
	mainBytes := fs.Snapshot(dbPath)
	walBytes := fs.Snapshot(dbPath + "-wal")
	p.Close()

	fs2 := loadFS(mainBytes, walBytes)
	p2 := mustOpen(t, fs2, Options{Sync: SyncFull, CheckpointFrames: 1 << 20})
	defer p2.Close()
	if got := p2.PageCount(); got != 4 {
		t.Fatalf("recovered page count = %d, want 4", got)
	}
	readBody(t, p2, ids[0], 1)
	readBody(t, p2, ids[1], 2)
	readBody(t, p2, ids[2], 3)
}

// TestIncrementalVacuumCrashBeforeTruncate models a crash after the freelist and
// header commit but before the file is truncated: the durable main file still holds
// the (now larger) trailing bytes, yet recovery reads the lowered page count, so the
// trailing pages are inert tail, never corruption. A later vacuum reclaims them.
func TestIncrementalVacuumCrashBeforeTruncate(t *testing.T) {
	fs := vfs.NewMemFS()
	p := mustOpen(t, fs, Options{Sync: SyncFull, CheckpointFrames: 1 << 20})
	ids := allocN(t, p, 6)
	freePages(t, p, ids[5], ids[4], ids[3])
	if _, err := p.IncrementalVacuum(0); err != nil {
		t.Fatalf("vacuum: %v", err)
	}
	pageSize := int64(p.Stats().PageSize)
	p.Close()

	// Re-grow the main file to its pre-truncate length, padding with zero bytes. This
	// is the on-disk image a crash between the durable commit and the truncate would
	// leave: a correct header reporting four pages, trailing bytes still present.
	mainBytes := fs.Snapshot(dbPath)
	padded := make([]byte, 7*pageSize)
	copy(padded, mainBytes)
	walBytes := fs.Snapshot(dbPath + "-wal")

	fs2 := loadFS(padded, walBytes)
	p2 := mustOpen(t, fs2, Options{Sync: SyncFull, CheckpointFrames: 1 << 20})
	defer p2.Close()
	if got := p2.PageCount(); got != 4 {
		t.Fatalf("page count = %d, want 4 (trailing bytes ignored)", got)
	}
	readBody(t, p2, ids[0], 1)
	readBody(t, p2, ids[1], 2)
	readBody(t, p2, ids[2], 3)
	// The database is fully usable: a fresh allocation grows from the recovered tail.
	grown := allocFilled(t, p2, 0x99)
	if grown != 4 {
		t.Fatalf("next allocation = %d, want 4 (grows from recovered tail)", grown)
	}
}
