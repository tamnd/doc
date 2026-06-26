package index

import (
	"testing"

	"github.com/tamnd/doc/pager"
	"github.com/tamnd/doc/vfs"
)

// TestBTreeCheckEmpty reports a clean result for a tree that was opened but never
// written: there is no root, so there is nothing to violate.
func TestBTreeCheckEmpty(t *testing.T) {
	fs := vfs.NewMemFS()
	_, bt := openTree(t, fs, pager.Options{Sync: pager.SyncFull})
	res := bt.Check()
	if len(res.Problems) != 0 {
		t.Fatalf("empty tree reported problems: %v", res.Problems)
	}
	if res.LeafKeys != 0 {
		t.Fatalf("empty tree reported %d keys", res.LeafKeys)
	}
}

// TestBTreeCheckClean inserts enough keys to force the tree to split into several
// leaves under at least one interior node, then confirms the two independent key
// counts agree and no structural problem is found.
func TestBTreeCheckClean(t *testing.T) {
	fs := vfs.NewMemFS()
	_, bt := openTree(t, fs, pager.Options{Sync: pager.SyncFull})

	const n = 2000
	for i := range n {
		putCommit(t, bt, EncodeObjectID(oid(uint32(i+1))), rid(uint32(i/64)+1, uint16(i%64)))
	}

	res := bt.Check()
	if len(res.Problems) != 0 {
		t.Fatalf("clean tree reported problems: %v", res.Problems)
	}
	if res.LeafKeys != n {
		t.Fatalf("leaf-chain key total = %d, want %d", res.LeafKeys, n)
	}
	if res.NodePages < 2 {
		t.Fatalf("expected the tree to have split into multiple nodes, got %d", res.NodePages)
	}
}

// TestBTreeCheckDetectsBadChecksum corrupts a node page on disk and confirms the
// structural walk surfaces the broken checksum after reopen.
func TestBTreeCheckDetectsBadChecksum(t *testing.T) {
	fs := vfs.NewMemFS()
	p, bt := openTree(t, fs, pager.Options{Sync: pager.SyncFull})
	const n = 500
	for i := range n {
		putCommit(t, bt, EncodeObjectID(oid(uint32(i+1))), rid(1, uint16(i%64)))
	}
	root := bt.root
	if err := p.Checkpoint(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	p.Close()

	raw := fs.Snapshot(dbPath)
	off := int(root) * 8192
	raw[off+80] ^= 0xFF // damage the root node body
	fs2 := loadFS(raw, fs.Snapshot(dbPath+"-wal"))

	p2, err := pager.Open(fs2, dbPath, pager.Options{Sync: pager.SyncFull})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer p2.Close()
	bt2, err := OpenWithRoot(p2, collID, true, root, func(uint32) {})
	if err != nil {
		t.Fatalf("btree reopen: %v", err)
	}
	res := bt2.Check()
	if len(res.Problems) == 0 {
		t.Fatal("a corrupted node should be reported")
	}
}
