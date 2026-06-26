package collection

import (
	"testing"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/query"
)

// This file is the doc 19 §24.3 allocation profile test. It uses testing.AllocsPerRun
// to pin the hot-path allocation budget so a regression that puts a warm read back on
// the allocator fails the gate. Two budgets are enforced: the §24.1 zero-allocation
// operations, held at ≤0.1 allocs/op (the spec's tolerance for a rare sync.Pool miss),
// and the §24.2 bounded operations, held under a measured ceiling that guards against
// drift without claiming an idealized zero.
//
// An honest note on scope: the §24.1 table describes the slotted-page buffer-pool
// engine, where a point read returns a slice straight out of a pinned frame. The v1
// store is the in-memory MVCC overlay (a version-chain map keyed by the order-preserving
// _id encoding) inherited from M1, which returns an independent clone for caller safety
// and encodes the lookup key into a Go string. So the three primitives the overlay can
// make genuinely allocation-free are pinned at ≤0.1 here (predicate evaluation, BSON
// field lookup, the read-only snapshot), while FindOne-by-_id and InsertOne carry a
// small bounded count from the clone and the string key. The buffer-pool zero-copy path
// is the deferred storage-engine work, not a behavior the overlay claims to meet.

const (
	// zeroAllocTol is the §24.3 tolerance: 0 would flake when the GC evicts a pool
	// entry between runs, so the gate allows up to a tenth of an allocation per op.
	zeroAllocTol = 0.1
	allocRuns    = 2000
)

// warmAllocColl loads n documents over {_id, v} and returns the warm collection.
func warmAllocColl(t *testing.T, n int) *Collection {
	t.Helper()
	c := newTestColl(t)
	for i := 0; i < n; i++ {
		if _, err := c.InsertOne(docInt(int32(i+1), "v", int32(i))); err != nil {
			t.Fatalf("seed insert: %v", err)
		}
	}
	return c
}

// TestPredicateEvalAllocsPerOp pins the §24.1 invariant that evaluating a compiled
// equality predicate against a warm document allocates nothing. The matcher is
// compiled once outside the measured closure, mirroring the cached-plan hot path.
func TestPredicateEvalAllocsPerOp(t *testing.T) {
	c := warmAllocColl(t, 2000)
	filter := filterID(1000)
	m, err := query.Compile(filter)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	doc, err := c.FindOne(filter)
	if err != nil || doc == nil {
		t.Fatalf("seed read: doc=%v err=%v", doc, err)
	}
	allocs := testing.AllocsPerRun(allocRuns, func() { _ = m.Match(doc) })
	if allocs > zeroAllocTol {
		t.Errorf("predicate eval: want ≤%.1f allocs/op, got %.2f", zeroAllocTol, allocs)
	}
}

// TestBSONLookupAllocsPerOp pins the skip-scan field lookup (doc 19 §6.1) at zero
// allocations: resolving a top-level field returns a slice into the document bytes.
func TestBSONLookupAllocsPerOp(t *testing.T) {
	doc := bson.NewBuilder().
		AppendInt32("_id", 7).
		AppendString("name", "carol").
		AppendInt32("v", 42).
		Build()
	allocs := testing.AllocsPerRun(allocRuns, func() {
		if _, ok := doc.Lookup("v"); !ok {
			t.Fatal("field v missing")
		}
	})
	if allocs > zeroAllocTol {
		t.Errorf("bson lookup: want ≤%.1f allocs/op, got %.2f", zeroAllocTol, allocs)
	}
}

// TestReadOnlyTxnBeginAllocsPerOp pins the §24.1 invariant that opening and closing a
// read-only snapshot allocates nothing: the snapshot version is read from an atomic and
// the Txn value does not escape, so it stays on the stack.
func TestReadOnlyTxnBeginAllocsPerOp(t *testing.T) {
	c := warmAllocColl(t, 100)
	allocs := testing.AllocsPerRun(allocRuns, func() {
		tx := c.BeginReadOnly()
		_ = tx.Rollback()
	})
	if allocs > zeroAllocTol {
		t.Errorf("read-only txn begin: want ≤%.1f allocs/op, got %.2f", zeroAllocTol, allocs)
	}
}

// TestFindOneByIDAllocsPerOp guards the warm point read against allocation drift. The
// overlay engine returns an independent clone and encodes the lookup key into a string,
// so the count is bounded rather than zero; the budget catches a regression that adds
// to it (for instance, re-compiling a residual matcher on the exact _id path).
func TestFindOneByIDAllocsPerOp(t *testing.T) {
	const budget = 8 // measured 6 on arm64; headroom for codec changes, not a license to grow.
	c := warmAllocColl(t, 2000)
	filter := filterID(1000)
	allocs := testing.AllocsPerRun(allocRuns, func() {
		doc, err := c.FindOne(filter)
		if err != nil || doc == nil {
			t.Fatalf("find: doc=%v err=%v", doc, err)
		}
	})
	t.Logf("FindOne by _id: %.2f allocs/op (budget %d)", allocs, budget)
	if allocs > budget {
		t.Errorf("FindOne by _id: want ≤%d allocs/op, got %.2f", budget, allocs)
	}
}

// TestInsertOneAllocsPerOp guards single-document insert-and-commit against gross
// regression. This is deliberately not a §24.1 zero-alloc claim: the §24.1 invariant is
// over insert *staging* (buffering before the WAL serializer), whereas this measures the
// full durable commit. The overlay engine runs a complete commit per call (buffer the
// document, encode the _id key, hash the conflict key, append a WAL frame, publish the
// version), none of it pooled yet, so the count is in the hundreds. The pooled
// single-allocation commit the spec describes is the deferred buffer-pool and WAL-pool
// work. The budget here exists only to catch a doubling, for example a stray per-insert
// clone or a map rebuilt from scratch on every write.
func TestInsertOneAllocsPerOp(t *testing.T) {
	const budget = 800 // measured ~639 on arm64; a guard against a step change, not a target.
	c := newTestColl(t)
	next := int32(1)
	allocs := testing.AllocsPerRun(allocRuns, func() {
		if _, err := c.InsertOne(docInt(next, "v", next)); err != nil {
			t.Fatalf("insert: %v", err)
		}
		next++
	})
	t.Logf("InsertOne commit: %.2f allocs/op (budget %d, full durable commit, not pooled)", allocs, budget)
	if allocs > budget {
		t.Errorf("InsertOne commit: want ≤%d allocs/op, got %.2f", budget, allocs)
	}
}
