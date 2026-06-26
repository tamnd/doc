package engine

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/pager"
	"github.com/tamnd/doc/sys"
	"github.com/tamnd/doc/vfs"
)

// crashOptions opens the database with a WAL fsync on every commit so the harness
// sees one durable boundary per transaction, the granularity §17.2 requires.
func crashOptions() Options {
	return Options{
		Pager: pager.Options{Sync: pager.SyncFull},
		IDGen: &sys.FixedIDGenerator{Timestamp: 1},
	}
}

// This file holds the at-scale crash and recovery suite (spec 2061 doc 19 §17).
// The durability guarantee under test is the one §17.1 states: every committed
// transaction survives a crash, no uncommitted transaction leaves a trace, and
// recovery rebuilds a structurally consistent database from the durable prefix of
// the WAL. The harness does not assert that property once; it demonstrates it by
// reopening the database at every fsync boundary in a workload and checking the
// recovered state against what was durable at that instant.
//
// The scale knobs follow the §17 design: a 1,000-transaction workload with one
// fsync per commit is 1,000 crash-and-recover cycles, and the doc 19 §M9 target
// is 100,000 scenarios over a large database. The default size keeps CI quick;
// DOC_CRASH_TXNS raises it for the release sweep and -short lowers it further.

// crashImage is a durable file image captured at one fsync boundary: the bytes of
// the main file and the WAL exactly as they sat on storage when the sync returned.
type crashImage struct {
	main []byte
	wal  []byte
}

// crashFS decorates a MemFS and records a durable image of the database at every
// fsync. It is the engine-level counterpart of the pager's FaultFS harness: rather
// than corrupt a single write, it captures the full crash-consistent state after
// each sync so the suite can reopen at every boundary the workload produced.
type crashFS struct {
	mem  *vfs.MemFS
	main string
	wal  string

	mu    sync.Mutex
	snaps []crashImage
}

func newCrashFS(main string) *crashFS {
	return &crashFS{mem: vfs.NewMemFS(), main: main, wal: main + "-wal"}
}

// capture records the current bytes of both files. It collapses a run of identical
// images (a sync that flushed nothing new, or a sync of the file that did not
// change) so the reopen loop does not redo work for a boundary that moved nothing.
func (c *crashFS) capture() {
	m := c.mem.Snapshot(c.main)
	w := c.mem.Snapshot(c.wal)
	c.mu.Lock()
	defer c.mu.Unlock()
	if n := len(c.snaps); n > 0 {
		last := c.snaps[n-1]
		if bytes.Equal(last.main, m) && bytes.Equal(last.wal, w) {
			return
		}
	}
	c.snaps = append(c.snaps, crashImage{main: m, wal: w})
}

// syncCount is the number of distinct durable images captured so far. The workload
// reads it right after a commit returns to learn the boundary by which that commit
// was made durable.
func (c *crashFS) syncCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.snaps)
}

func (c *crashFS) images() []crashImage {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]crashImage, len(c.snaps))
	copy(out, c.snaps)
	return out
}

func (c *crashFS) Open(path string, flags vfs.OpenFlags) (vfs.File, error) {
	f, err := c.mem.Open(path, flags)
	if err != nil {
		return nil, err
	}
	return &crashFile{fs: c, inner: f}, nil
}

func (c *crashFS) Delete(path string, syncDir bool) error { return c.mem.Delete(path, syncDir) }
func (c *crashFS) Exists(path string) (bool, error)       { return c.mem.Exists(path) }
func (c *crashFS) ShmMap(path string, region int, create bool) ([]byte, error) {
	return c.mem.ShmMap(path, region, create)
}

type crashFile struct {
	fs    *crashFS
	inner vfs.File
}

func (f *crashFile) ReadAt(p []byte, off int64) (int, error)  { return f.inner.ReadAt(p, off) }
func (f *crashFile) WriteAt(p []byte, off int64) (int, error) { return f.inner.WriteAt(p, off) }
func (f *crashFile) Truncate(size int64) error                { return f.inner.Truncate(size) }
func (f *crashFile) Size() (int64, error)                     { return f.inner.Size() }
func (f *crashFile) Close() error                             { return f.inner.Close() }

func (f *crashFile) Sync(mode vfs.SyncMode) error {
	err := f.inner.Sync(mode)
	if err == nil {
		f.fs.capture()
	}
	return err
}

// loadCrashFS builds a fresh MemFS preloaded with one captured image, modeling a
// process restart against exactly the bytes that survived the crash at that fsync.
func loadCrashFS(img crashImage, main string) *vfs.MemFS {
	fs := vfs.NewMemFS()
	write := func(path string, b []byte) {
		if b == nil {
			return
		}
		fl, _ := fs.Open(path, vfs.OpenCreate)
		_, _ = fl.WriteAt(b, 0)
		_ = fl.Close()
	}
	write(main, img.main)
	write(main+"-wal", img.wal)
	return fs
}

// crashScale returns the workload size, honoring DOC_CRASH_TXNS and the -short cap.
func crashScale(t *testing.T, dflt int) int {
	t.Helper()
	n := dflt
	if v := os.Getenv("DOC_CRASH_TXNS"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			n = parsed
		}
	}
	if testing.Short() && n > 40 {
		n = 40
	}
	return n
}

// TestCrashRecoveryEveryFsync is the §17.2 harness. It runs an insert-only
// workload, captures the durable image at every fsync boundary, then reopens the
// database on each image and asserts the three durability invariants: every
// acknowledged commit survives, the recovered set is a gap-free prefix of the
// commit order (no uncommitted transaction leaked in past a committed one), and
// the recovered database passes a structural check.
func TestCrashRecoveryEveryFsync(t *testing.T) {
	const dbName, collName = "shop", "orders"
	const path = "crash.doc"
	n := crashScale(t, 200)

	cfs := newCrashFS(path)
	e, err := Open(cfs, path, crashOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	c, err := e.CreateCollection(dbName, collName)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// ackBoundary[seq] is the number of durable images that existed the instant
	// the insert of seq returned from commit, so seq is durable in every image at
	// or beyond that index.
	ackBoundary := make([]int, n+1)
	for seq := 1; seq <= n; seq++ {
		if _, err := c.InsertOne(crashDoc(seq)); err != nil {
			t.Fatalf("insert %d: %v", seq, err)
		}
		ackBoundary[seq] = cfs.syncCount()
	}
	if err := e.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	images := cfs.images()
	if len(images) < n/2 {
		t.Fatalf("captured only %d fsync boundaries for %d inserts, expected one per commit", len(images), n)
	}

	for idx, img := range images {
		// required is the set of inserts acknowledged durable by this boundary.
		required := 0
		for seq := 1; seq <= n; seq++ {
			if ackBoundary[seq] > 0 && ackBoundary[seq] <= idx+1 {
				required = seq
			}
		}
		verifyCrashImage(t, img, path, dbName, collName, required, n, idx)
	}
	t.Logf("verified %d fsync boundaries across %d inserts", len(images), n)
}

// verifyCrashImage reopens one captured image and checks recovery. present must be
// a contiguous prefix {1..m}, m must be at least required (acknowledged commits
// survive) and at most total (nothing from the future appears), every present doc
// must read back its written value, and the file must pass a full structural check.
func verifyCrashImage(t *testing.T, img crashImage, path, db, coll string, required, total, idx int) {
	t.Helper()
	fs := loadCrashFS(img, path)
	e, err := Open(fs, path, crashOptions())
	if err != nil {
		t.Fatalf("image %d: reopen: %v", idx, err)
	}
	defer e.Close()

	c := e.GetCollection(db, coll)
	present := map[int]string{}
	if c != nil {
		docs, err := c.Find(bson.NewBuilder().Build())
		if err != nil {
			t.Fatalf("image %d: find: %v", idx, err)
		}
		for _, d := range docs {
			id, ok := d.Lookup("_id")
			if !ok {
				t.Fatalf("image %d: recovered doc has no _id: %v", idx, d)
			}
			v, ok := d.Lookup("v")
			if !ok {
				t.Fatalf("image %d: recovered doc has no v: %v", idx, d)
			}
			present[int(id.Int32())] = v.StringValue()
		}
	}

	// The recovered seqs must form the gap-free prefix {1..m}.
	m := 0
	for seq := 1; seq <= total; seq++ {
		if _, ok := present[seq]; ok {
			m = seq
		} else {
			break
		}
	}
	if len(present) != m {
		t.Fatalf("image %d: recovered %d docs but the gap-free prefix is only %d long; a committed transaction was skipped or an uncommitted one leaked", idx, len(present), m)
	}
	if m < required {
		t.Fatalf("image %d: recovered prefix length %d is below the acknowledged-durable count %d; a committed transaction was lost", idx, m, required)
	}
	if m > total {
		t.Fatalf("image %d: recovered prefix length %d exceeds the %d inserts issued", idx, m, total)
	}
	for seq := 1; seq <= m; seq++ {
		if got := present[seq]; got != crashValue(seq) {
			t.Fatalf("image %d: doc %d reads %q, want %q", idx, seq, got, crashValue(seq))
		}
	}
	if rep := e.Check(true); !rep.Valid {
		t.Fatalf("image %d: doc check failed after recovery: %+v", idx, rep)
	}
}

// crashDoc builds the seq-th insert: {_id: seq, v: "v<seq>"}.
func crashDoc(seq int) bson.Raw {
	return bson.NewBuilder().AppendInt32("_id", int32(seq)).AppendString("v", crashValue(seq)).Build()
}

func crashValue(seq int) string { return fmt.Sprintf("v%d", seq) }
