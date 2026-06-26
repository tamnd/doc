package collection

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tamnd/doc/bson"
)

// This file holds the concurrency stress test and the linearizability checker it feeds
// (spec 2061 doc 19 §19.2). N = 2 x GOMAXPROCS goroutines run a random read-then-write
// mix of multi-document transactions against one collection, each recording the history
// of the attempt that committed. The checker then verifies the combined history against
// the declared isolation level: under snapshot isolation every read returned the value
// visible at the transaction's snapshot version, and under serializable isolation the
// committed transactions additionally form no read-write antidependency cycle.
//
// The workload reads in a phase before it writes and never reads a key it is about to
// write, so every recorded read reflects the snapshot rather than read-your-writes. That
// keeps the checker a pure model of versioned visibility. Writes set a globally unique
// tag so each version is distinguishable across the whole history.

// readEvent is one observed read: the key and the tag it saw, or found=false for absent.
type readEvent struct {
	key   int32
	tag   int64
	found bool
}

// writeEvent is one committed write: the key and the tag it installed.
type writeEvent struct {
	key int32
	tag int64
}

// txnHistory is the record of one committed transaction.
type txnHistory struct {
	startVer     uint64
	commitVer    uint64
	reads        []readEvent
	writes       []writeEvent
	serializable bool
	txnRef       *Txn // live handle of the committed attempt; nil once commitVer is captured
}

// docTag builds {_id: id, tag: tag}.
func docTag(id int32, tag int64) bson.Raw {
	return bson.NewBuilder().AppendInt32("_id", id).AppendInt64("tag", tag).Build()
}

// tagSet builds {$set: {tag: tag}}.
func tagSet(tag int64) bson.Raw {
	return bson.NewBuilder().AppendDocument("$set",
		bson.NewBuilder().AppendInt64("tag", tag).Build()).Build()
}

// version is one entry in a key's reconstructed version timeline.
type version struct {
	commitVer uint64
	tag       int64
	deleted   bool
}

// readTag reads key id and returns its tag and whether a document was present.
func readTag(t *testing.T, tx *Txn, id int32) (int64, bool) {
	t.Helper()
	doc, err := tx.FindOne(filterID(id))
	if err != nil {
		t.Fatalf("FindOne(%d): %v", id, err)
	}
	if doc == nil {
		return 0, false
	}
	v, ok := doc.Lookup("tag")
	if !ok {
		t.Fatalf("document _id=%d has no tag", id)
	}
	return v.Int64(), true
}

// writeTag sets key id's tag through an update.
func writeTag(t *testing.T, tx *Txn, id int32, tag int64) {
	t.Helper()
	upd := tagSet(tag)
	if _, err := tx.UpdateOne(filterID(id), upd); err != nil {
		t.Fatalf("UpdateOne(%d): %v", id, err)
	}
}

// buildVersions reconstructs, per key, the ascending-by-commit-version timeline from the
// seed plus every committed write in the history.
func buildVersions(seedCommit uint64, keys []int32, hist []txnHistory) map[int32][]version {
	vs := make(map[int32][]version, len(keys))
	for _, k := range keys {
		vs[k] = []version{{commitVer: seedCommit, tag: 0}}
	}
	for _, h := range hist {
		for _, w := range h.writes {
			vs[w.key] = append(vs[w.key], version{commitVer: h.commitVer, tag: w.tag})
		}
	}
	for k := range vs {
		slice := vs[k]
		// Insertion sort by commitVer; histories are small per key.
		for i := 1; i < len(slice); i++ {
			for j := i; j > 0 && slice[j-1].commitVer > slice[j].commitVer; j-- {
				slice[j-1], slice[j] = slice[j], slice[j-1]
			}
		}
		vs[k] = slice
	}
	return vs
}

// visibleAtVersion returns the version of key k visible at snapshot ver: the one with the
// greatest commit version at or below ver.
func visibleAtVersion(timeline []version, ver uint64) (version, bool) {
	var got version
	found := false
	for _, v := range timeline {
		if v.commitVer <= ver {
			got = v
			found = true
		} else {
			break
		}
	}
	return got, found
}

// checkSnapshotIsolation verifies every recorded read returned the value visible at the
// reading transaction's snapshot version. This is the snapshot-isolation contract from
// spec 2061 doc 19 §19.2; it holds for both SI and SSI histories.
func checkSnapshotIsolation(t *testing.T, seedCommit uint64, keys []int32, hist []txnHistory) {
	t.Helper()
	vs := buildVersions(seedCommit, keys, hist)
	for _, h := range hist {
		for _, r := range h.reads {
			want, ok := visibleAtVersion(vs[r.key], h.startVer)
			if !ok || want.deleted {
				if r.found {
					t.Fatalf("read of key %d at snapshot %d saw tag %d, want absent", r.key, h.startVer, r.tag)
				}
				continue
			}
			if !r.found {
				t.Fatalf("read of key %d at snapshot %d saw absent, want tag %d", r.key, h.startVer, want.tag)
			}
			if r.tag != want.tag {
				t.Fatalf("read of key %d at snapshot %d saw tag %d, want tag %d (the version visible at that snapshot)", r.key, h.startVer, r.tag, want.tag)
			}
		}
	}
}

// checkSerializable verifies the committed serializable transactions form no read-write
// antidependency cycle, which is equivalent to the history being serializable (spec 2061
// doc 19 §19.2). An edge T1 -> T2 exists when T1 read a key that T2 wrote at a commit
// version above T1's snapshot, so T2's write was invisible to T1 and T1 must precede T2
// in any serial order.
func checkSerializable(t *testing.T, hist []txnHistory) {
	t.Helper()
	n := len(hist)
	adj := make([][]int, n)
	for i := range hist {
		ti := hist[i]
		for j := range hist {
			if i == j {
				continue
			}
			tj := hist[j]
			if antidependency(ti, tj) {
				adj[i] = append(adj[i], j)
			}
		}
	}
	if cyc := findCycle(adj); cyc != nil {
		t.Fatalf("serializable history has a read-write antidependency cycle among transactions %v", cyc)
	}
}

// antidependency reports whether t1 -> t2 holds: t1 read some key that t2 wrote at a
// commit version above t1's snapshot.
func antidependency(t1, t2 txnHistory) bool {
	for _, r := range t1.reads {
		for _, w := range t2.writes {
			if r.key == w.key && t2.commitVer > t1.startVer {
				return true
			}
		}
	}
	return false
}

// findCycle returns a cycle in the directed graph as a list of node indices, or nil if
// the graph is acyclic, by a depth-first three-color walk.
func findCycle(adj [][]int) []int {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make([]int, len(adj))
	var stack []int
	var dfs func(u int) []int
	dfs = func(u int) []int {
		color[u] = gray
		stack = append(stack, u)
		for _, v := range adj[u] {
			if color[v] == gray {
				for i, x := range stack {
					if x == v {
						return append([]int(nil), stack[i:]...)
					}
				}
			}
			if color[v] == white {
				if c := dfs(v); c != nil {
					return c
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[u] = black
		return nil
	}
	for u := range adj {
		if color[u] == white {
			if c := dfs(u); c != nil {
				return c
			}
		}
	}
	return nil
}

// runStress drives the randomized concurrent workload at the given isolation level and
// returns the seed commit version, the key space, and the recorded committed history.
func runStress(t *testing.T, iso IsolationLevel) (uint64, []int32, []txnHistory) {
	t.Helper()
	c := newTestColl(t)

	const numKeys = 8
	keys := make([]int32, numKeys)
	for i := range keys {
		keys[i] = int32(i + 1)
	}
	// Seed every key with tag 0 in one transaction and capture its commit version.
	seed := c.Begin()
	for _, k := range keys {
		if _, err := seed.InsertOne(docTag(k, 0)); err != nil {
			t.Fatalf("seed insert: %v", err)
		}
	}
	if err := seed.Commit(); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	seedCommit := seed.CommitVersion()

	opts := TransactionOptions{Isolation: iso}
	workers := 2 * runtime.GOMAXPROCS(0)
	const txnsPerWorker = 80

	var tagSeq int64
	var mu sync.Mutex
	var hist []txnHistory

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		seedRand := uint64(w)*2862933555777941757 + 3037000493
		wg.Add(1)
		go func(rng uint64) {
			defer wg.Done()
			next := func(n int) int {
				rng ^= rng << 13
				rng ^= rng >> 7
				rng ^= rng << 17
				return int(rng % uint64(n))
			}
			for i := 0; i < txnsPerWorker; i++ {
				numReads := 1 + next(3)
				numWrites := 1 + next(2)
				var committed txnHistory
				err := c.WithTransaction(func(tx *Txn) error {
					rec := txnHistory{serializable: iso == Serializable}
					written := make(map[int32]bool)
					// Read phase: read distinct keys.
					for r := 0; r < numReads; r++ {
						k := keys[next(numKeys)]
						tag, found := readTag(t, tx, k)
						rec.reads = append(rec.reads, readEvent{key: k, tag: tag, found: found})
					}
					// Write phase: write distinct keys not read-back in this txn.
					for ws := 0; ws < numWrites; ws++ {
						k := keys[next(numKeys)]
						if written[k] {
							continue
						}
						written[k] = true
						tag := atomic.AddInt64(&tagSeq, 1)
						writeTag(t, tx, k, tag)
						rec.writes = append(rec.writes, writeEvent{key: k, tag: tag})
					}
					committed = rec
					committed.startVer = tx.SnapshotVersion()
					committed.txnRef = tx
					return nil
				}, opts)
				if err != nil {
					continue
				}
				committed.commitVer = committed.txnRef.CommitVersion()
				committed.txnRef = nil
				if len(committed.writes) == 0 {
					// A read-only outcome commits at no new version; record it for the SI
					// read check but it forms no antidependency target.
					committed.commitVer = committed.startVer
				}
				mu.Lock()
				hist = append(hist, committed)
				mu.Unlock()
			}
		}(seedRand)
	}
	wg.Wait()
	return seedCommit, keys, hist
}

// TestCheckerDetectsAntidependencyCycle proves the serializable checker is not vacuous:
// fed a hand-built write-skew history (two transactions with a mutual antidependency, the
// shape SSI must abort), it reports a cycle. The same two transactions with one write
// removed are acyclic.
func TestCheckerDetectsAntidependencyCycle(t *testing.T) {
	// T0 read key 1, wrote key 2 at version 10; T1 read key 2, wrote key 1 at version
	// 11. Both started at snapshot 5, so each wrote a key the other read at a version
	// above the other's snapshot: T0 -> T1 and T1 -> T0, a cycle.
	cyc := []txnHistory{
		{startVer: 5, commitVer: 10, reads: []readEvent{{key: 1, found: true}}, writes: []writeEvent{{key: 2, tag: 1}}},
		{startVer: 5, commitVer: 11, reads: []readEvent{{key: 2, found: true}}, writes: []writeEvent{{key: 1, tag: 2}}},
	}
	if !antidependency(cyc[0], cyc[1]) || !antidependency(cyc[1], cyc[0]) {
		t.Fatal("expected mutual antidependency between the two write-skew transactions")
	}
	adj := [][]int{{1}, {0}}
	if findCycle(adj) == nil {
		t.Fatal("findCycle missed the two-node cycle")
	}

	// Drop T1's write so it is read-only: it keeps its outgoing edge to T0 (it read a
	// key T0 wrote) but T0 no longer points back, so the graph is acyclic.
	acyclic := []txnHistory{
		cyc[0],
		{startVer: 5, commitVer: 11, reads: []readEvent{{key: 2, found: true}}},
	}
	if antidependency(acyclic[0], acyclic[1]) {
		t.Fatal("no edge should point at a read-only transaction (it wrote nothing)")
	}
	if !antidependency(acyclic[1], acyclic[0]) {
		t.Fatal("the read-only transaction should still have an outgoing edge to the writer")
	}
	if findCycle([][]int{nil, {0}}) != nil {
		t.Fatal("findCycle reported a cycle in an acyclic graph")
	}
}

// TestLinearizabilitySnapshotIsolation runs the concurrent stress at snapshot isolation
// and checks every read against the version visible at its snapshot.
func TestLinearizabilitySnapshotIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test skipped in -short")
	}
	seedCommit, keys, hist := runStress(t, SnapshotIsolation)
	if len(hist) == 0 {
		t.Fatal("no committed transactions recorded")
	}
	checkSnapshotIsolation(t, seedCommit, keys, hist)
}

// TestLinearizabilitySerializable runs the concurrent stress at serializable isolation
// and checks both the snapshot-read contract and the absence of an antidependency cycle.
func TestLinearizabilitySerializable(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test skipped in -short")
	}
	seedCommit, keys, hist := runStress(t, Serializable)
	if len(hist) == 0 {
		t.Fatal("no committed transactions recorded")
	}
	checkSnapshotIsolation(t, seedCommit, keys, hist)
	checkSerializable(t, hist)
}
