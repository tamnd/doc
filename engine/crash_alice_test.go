package engine

import (
	"testing"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/vfs"
	"github.com/tamnd/doc/wal"
)

// This file implements the simplified ALICE-style crash explorer of spec 2061 doc
// 19 §17.6. ALICE enumerates the crash states a workload can leave behind by
// tracking every persistent write and exploring the prefix closure. The WAL design
// shrinks that space: only whole WAL frames are atomic units, so the reachable
// crash states are the WAL truncated at each frame boundary. The explorer recovers
// from each such prefix and asserts the result is always a gap-free committed
// prefix that passes the structural check. Truncations that fall inside a frame are
// included to prove the chained frame checksum rejects a partial trailing frame
// rather than half-applying it.

// TestALICEPrefixExploration builds a WAL of single-document commits, then recovers
// from the WAL truncated at every frame boundary (and a few mid-frame offsets). It
// asserts every reachable crash state recovers to a valid prefix of the commit log.
func TestALICEPrefixExploration(t *testing.T) {
	const db, coll, path = "shop", "orders", "alice.doc"
	n := crashScale(t, 60)

	// Build the WAL with n commits and no checkpoint, so every commit lives as a
	// frame in the WAL and the explorer can truncate frame by frame.
	mem := vfs.NewMemFS()
	e, err := Open(mem, path, crashOptions())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	c, err := e.CreateCollection(db, coll)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 1; i <= n; i++ {
		if _, err := c.InsertOne(crashDoc(i)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	mainBytes := mem.Snapshot(path)
	walBytes := mem.Snapshot(path + "-wal")
	if err := e.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	frameSize := wal.FrameSize(uint32(8192))
	// Crash offsets: every whole-frame boundary plus a mid-frame point to confirm a
	// partial frame is rejected, not partially applied.
	var offsets []int64
	for off := int64(wal.WALHeaderSize); off <= int64(len(walBytes)); off += frameSize {
		offsets = append(offsets, off)
		if mid := off + frameSize/2; mid < int64(len(walBytes)) {
			offsets = append(offsets, mid)
		}
	}

	maxPrefix := 0
	for _, off := range offsets {
		m := aliceRecover(t, mainBytes, walBytes, off, path, db, coll, n)
		if m > maxPrefix {
			maxPrefix = m
		}
	}
	if maxPrefix < n {
		t.Fatalf("the full-WAL crash state recovered only %d of %d commits", maxPrefix, n)
	}
	t.Logf("explored %d crash states; every one recovered a gap-free prefix, up to all %d commits", len(offsets), n)
}

// aliceRecover truncates the WAL at off, runs recovery, and asserts the recovered
// documents form a gap-free prefix {1..m} with each value intact and a clean check.
// It returns m.
func aliceRecover(t *testing.T, mainBytes, walBytes []byte, off int64, path, db, coll string, total int) int {
	t.Helper()
	truncated := make([]byte, off)
	copy(truncated, walBytes[:off])

	fs := loadCrashFS(crashImage{main: mainBytes, wal: truncated}, path)
	e, err := Open(fs, path, crashOptions())
	if err != nil {
		t.Fatalf("off %d: reopen: %v", off, err)
	}
	defer e.Close()

	present := map[int]string{}
	if c := e.GetCollection(db, coll); c != nil {
		docs, err := c.Find(bson.NewBuilder().Build())
		if err != nil {
			t.Fatalf("off %d: find: %v", off, err)
		}
		for _, d := range docs {
			id, _ := d.Lookup("_id")
			v, _ := d.Lookup("v")
			present[int(id.Int32())] = v.StringValue()
		}
	}

	m := 0
	for seq := 1; seq <= total; seq++ {
		if _, ok := present[seq]; ok {
			m = seq
		} else {
			break
		}
	}
	if len(present) != m {
		t.Fatalf("off %d: recovered %d docs but the gap-free prefix is %d; a frame applied out of order", off, len(present), m)
	}
	for seq := 1; seq <= m; seq++ {
		if present[seq] != crashValue(seq) {
			t.Fatalf("off %d: doc %d reads %q, want %q", off, seq, present[seq], crashValue(seq))
		}
	}
	if rep := e.Check(true); !rep.Valid {
		t.Fatalf("off %d: doc check failed: %+v", off, rep)
	}
	return m
}
