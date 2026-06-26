package heap

import (
	"strings"
	"testing"

	"github.com/tamnd/doc/pager"
	"github.com/tamnd/doc/storage"
	"github.com/tamnd/doc/vfs"
)

// TestHeapCheckClean inserts, updates, and deletes records, then confirms the
// integrity walk finds no problems and its observed counts match the directory.
func TestHeapCheckClean(t *testing.T) {
	fs := vfs.NewMemFS()
	_, h := openHeap(t, fs, pager.Options{Sync: pager.SyncFull})

	var rids []storage.RID
	for i := range 20 {
		rids = append(rids, insertCommit(t, h, makeDoc(64, byte(i+1))))
	}
	// Grow one document so it forwards off-page, exercising the forwarding path.
	tx := h.Begin()
	if _, err := h.Update(tx, rids[0], makeDoc(9000, 0x7)); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// Delete a few so dead slots exist.
	tx = h.Begin()
	for _, r := range rids[10:14] {
		if err := h.Delete(tx, r); err != nil {
			t.Fatalf("delete: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	res := h.Check()
	if len(res.Problems) != 0 {
		t.Fatalf("clean heap reported problems: %v", res.Problems)
	}
	if res.LiveRecords != h.liveCount {
		t.Fatalf("walk live %d, directory %d", res.LiveRecords, h.liveCount)
	}
}

// TestHeapCheckCountMismatch corrupts the cached live counter and confirms Check
// reports the disagreement between the directory and the pages.
func TestHeapCheckCountMismatch(t *testing.T) {
	fs := vfs.NewMemFS()
	_, h := openHeap(t, fs, pager.Options{Sync: pager.SyncFull})
	for i := range 5 {
		insertCommit(t, h, makeDoc(48, byte(i+1)))
	}
	h.liveCount += 3 // pretend the directory lost track of three records

	res := h.Check()
	if len(res.Problems) == 0 {
		t.Fatal("a count mismatch should be reported")
	}
	if !strings.Contains(strings.Join(res.Problems, ";"), "live-count mismatch") {
		t.Fatalf("expected a live-count mismatch, got %v", res.Problems)
	}
}
