package pager

import (
	"testing"

	"github.com/tamnd/doc/format"
	"github.com/tamnd/doc/vfs"
)

const dbPath = "test.doc"

func bodyRange(pageSize int) (int, int) {
	return format.PageHeaderSize, pageSize - format.ChecksumSize
}

// fillBody stamps a heap page with a recognizable byte pattern across its body.
func fillBody(f *Frame, b byte) {
	format.InitPage(f.Buf, format.PageHeap, 1)
	lo, hi := bodyRange(len(f.Buf))
	for i := lo; i < hi; i++ {
		f.Buf[i] = b
	}
}

func checkBody(t *testing.T, f *Frame, b byte) {
	t.Helper()
	lo, hi := bodyRange(len(f.Buf))
	for i := lo; i < hi; i++ {
		if f.Buf[i] != b {
			t.Fatalf("page %d body byte %d = %#x, want %#x", f.PageID, i, f.Buf[i], b)
			return
		}
	}
}

func mustOpen(t *testing.T, fs vfs.FS, opts Options) *Pager {
	t.Helper()
	p, err := Open(fs, dbPath, opts)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return p
}

// allocFilled allocates a page, fills its body with b, marks it dirty, and
// unpins it. It returns the new page id.
func allocFilled(t *testing.T, p *Pager, b byte) uint64 {
	t.Helper()
	id, f, err := p.Allocate()
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	fillBody(f, b)
	p.MarkDirty(f)
	p.Unpin(f)
	return id
}

func readBody(t *testing.T, p *Pager, id uint64, want byte) {
	t.Helper()
	f, err := p.Fetch(id, false)
	if err != nil {
		t.Fatalf("fetch %d: %v", id, err)
	}
	checkBody(t, f, want)
	p.Unpin(f)
}

func TestCreateReopenRoundTrip(t *testing.T) {
	fs := vfs.NewMemFS()
	p := mustOpen(t, fs, Options{Sync: SyncFull})
	id1 := allocFilled(t, p, 0xA1)
	id2 := allocFilled(t, p, 0xB2)
	if err := p.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	p2 := mustOpen(t, fs, Options{Sync: SyncFull})
	defer p2.Close()
	readBody(t, p2, id1, 0xA1)
	readBody(t, p2, id2, 0xB2)
	if got := p2.PageCount(); got != 3 {
		t.Fatalf("page count = %d, want 3 (header + 2)", got)
	}
}

func TestReopenSeesNothingUncommitted(t *testing.T) {
	fs := vfs.NewMemFS()
	p := mustOpen(t, fs, Options{Sync: SyncFull})
	allocFilled(t, p, 0xCC) // never committed
	// Crash before commit: snapshot the durable bytes and recover from them.
	mainBytes := fs.Snapshot(dbPath)
	walBytes := fs.Snapshot(dbPath + "-wal")

	fs2 := loadFS(mainBytes, walBytes)
	p2 := mustOpen(t, fs2, Options{Sync: SyncFull})
	defer p2.Close()
	if got := p2.PageCount(); got != 1 {
		t.Fatalf("page count = %d, want 1 (only the header); uncommitted alloc leaked", got)
	}
}

func TestEvictionWriteBackAndReread(t *testing.T) {
	fs := vfs.NewMemFS()
	// Tiny pool forces eviction: header frame + a couple of slots.
	p := mustOpen(t, fs, Options{Sync: SyncFull, PoolPages: 4, CheckpointFrames: 1 << 20})
	const n = 40
	ids := make([]uint64, n)
	for i := range ids {
		ids[i] = allocFilled(t, p, byte(i+1))
		if err := p.Commit(); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}
	// Every page must read back correctly despite having been evicted to and
	// re-read from the main file.
	for i, id := range ids {
		readBody(t, p, id, byte(i+1))
	}
	p.Close()
}

func TestFreelistRecyclesPages(t *testing.T) {
	fs := vfs.NewMemFS()
	p := mustOpen(t, fs, Options{Sync: SyncFull})
	defer p.Close()

	a := allocFilled(t, p, 0x11)
	b := allocFilled(t, p, 0x22)
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	// Free b, then allocate again: the freelist must hand back b's page id.
	fb, _ := p.Fetch(b, true)
	_ = fb
	if err := p.Free(b); err != nil {
		t.Fatalf("free: %v", err)
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	reused, _, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if reused != b {
		t.Fatalf("reused page = %d, want recycled %d", reused, b)
	}
	if reused == a {
		t.Fatal("recycled a live page")
	}
}

func TestCheckpointShrinksWALAndPersists(t *testing.T) {
	fs := vfs.NewMemFS()
	p := mustOpen(t, fs, Options{Sync: SyncFull, CheckpointFrames: 1 << 20})
	id := allocFilled(t, p, 0x7E)
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	walBefore := len(fs.Snapshot(dbPath + "-wal"))
	if err := p.Checkpoint(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	walAfter := len(fs.Snapshot(dbPath + "-wal"))
	if walAfter >= walBefore {
		t.Fatalf("checkpoint did not shrink WAL: before=%d after=%d", walBefore, walAfter)
	}
	// Data is in the main file now: a recovery from a snapshot with an empty WAL
	// still sees it.
	mainBytes := fs.Snapshot(dbPath)
	walBytes := fs.Snapshot(dbPath + "-wal")
	p.Close()

	fs2 := loadFS(mainBytes, walBytes)
	p2 := mustOpen(t, fs2, Options{Sync: SyncFull})
	defer p2.Close()
	readBody(t, p2, id, 0x7E)
}

// TestRecoveryAtEveryCommitBoundary is the pager-level M1 exit criterion: after
// each committed insert, the durable bytes (main + WAL) must recover to exactly
// the committed set of pages — no more, no less.
func TestRecoveryAtEveryCommitBoundary(t *testing.T) {
	fs := vfs.NewMemFS()
	p := mustOpen(t, fs, Options{Sync: SyncFull, PoolPages: 8, CheckpointFrames: 1 << 20})

	const n = 60
	ids := make([]uint64, n)
	type snap struct{ main, wal []byte }
	snaps := make([]snap, n)
	for i := 0; i < n; i++ {
		ids[i] = allocFilled(t, p, byte(i+1))
		if err := p.Commit(); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
		snaps[i] = snap{fs.Snapshot(dbPath), fs.Snapshot(dbPath + "-wal")}
	}
	// Abandon p without a clean close (that would checkpoint); each snapshot is a
	// crash exactly at that commit boundary.
	for k := 0; k < n; k++ {
		fs2 := loadFS(snaps[k].main, snaps[k].wal)
		p2, err := Open(fs2, dbPath, Options{Sync: SyncFull, PoolPages: 8, CheckpointFrames: 1 << 20})
		if err != nil {
			t.Fatalf("recover at boundary %d: %v", k, err)
		}
		if got := p2.PageCount(); got != uint32(k+2) {
			t.Fatalf("boundary %d: page count = %d, want %d", k, got, k+2)
		}
		for j := 0; j <= k; j++ {
			readBody(t, p2, ids[j], byte(j+1))
		}
		p2.Close()
	}
}

// TestTornMainPageRepairedFromWAL models a torn page write to the main file that
// was never followed by a checkpoint: recovery replays the WAL full-page image
// over the damaged page (spec 2061 doc 05 §12).
func TestTornMainPageRepairedFromWAL(t *testing.T) {
	fs := vfs.NewMemFS()
	p := mustOpen(t, fs, Options{Sync: SyncFull, CheckpointFrames: 1 << 20})
	id := allocFilled(t, p, 0x5A)
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	mainBytes := fs.Snapshot(dbPath)
	walBytes := fs.Snapshot(dbPath + "-wal")
	p.Close()

	// Simulate a torn steal: the page reached the main file with its second half
	// zeroed (a partial write), which fails the page checksum.
	off := int(id) * 8192
	if off+8192 <= len(mainBytes) {
		for i := off + 4096; i < off+8192; i++ {
			mainBytes[i] = 0
		}
	}
	fs2 := loadFS(mainBytes, walBytes)
	p2 := mustOpen(t, fs2, Options{Sync: SyncFull})
	defer p2.Close()
	// Recovery replayed the WAL image over the torn page; it reads back intact.
	readBody(t, p2, id, 0x5A)
}

func TestReadOnlyRejectsWrites(t *testing.T) {
	fs := vfs.NewMemFS()
	p := mustOpen(t, fs, Options{Sync: SyncFull})
	allocFilled(t, p, 0x01)
	p.Commit()
	p.Close()

	ro := mustOpen(t, fs, Options{ReadOnly: true})
	defer ro.Close()
	if _, _, err := ro.Allocate(); err != ErrReadOnly {
		t.Fatalf("allocate on read-only: err = %v, want ErrReadOnly", err)
	}
	if err := ro.Commit(); err != ErrReadOnly {
		t.Fatalf("commit on read-only: err = %v, want ErrReadOnly", err)
	}
}

func TestClosedRejectsOps(t *testing.T) {
	fs := vfs.NewMemFS()
	p := mustOpen(t, fs, Options{Sync: SyncFull})
	p.Close()
	if _, err := p.Fetch(0, false); err != ErrClosed {
		t.Fatalf("fetch after close: err = %v, want ErrClosed", err)
	}
}

func TestSyncLevels(t *testing.T) {
	for _, lvl := range []SyncLevel{SyncOff, SyncNormal, SyncFull} {
		fs := vfs.NewMemFS()
		p := mustOpen(t, fs, Options{Sync: lvl})
		id := allocFilled(t, p, 0x3C)
		if err := p.Commit(); err != nil {
			t.Fatalf("commit at level %d: %v", lvl, err)
		}
		readBody(t, p, id, 0x3C)
		p.Close()
	}
}

// loadFS builds a fresh MemFS preloaded with the given durable file images,
// modeling a process restart against the bytes that survived a crash.
func loadFS(mainBytes, walBytes []byte) *vfs.MemFS {
	fs := vfs.NewMemFS()
	if mainBytes != nil {
		f, _ := fs.Open(dbPath, vfs.OpenCreate)
		f.WriteAt(mainBytes, 0)
		f.Close()
	}
	if walBytes != nil {
		f, _ := fs.Open(dbPath+"-wal", vfs.OpenCreate)
		f.WriteAt(walBytes, 0)
		f.Close()
	}
	return fs
}
