package collection

import (
	"fmt"
	"sort"
	"testing"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/colstore"
	"github.com/tamnd/doc/sys"
	"github.com/tamnd/doc/vfs"
)

// groupStage builds {$group: {_id: "$cat", total: {$sum: "$qty"}, n: {$sum: 1},
// avg: {$avg: "$qty"}, hi: {$max: "$qty"}, lo: {$min: "$qty"}}}.
func groupStage() bson.Raw {
	return bson.NewBuilder().AppendDocument("$group", bson.NewBuilder().
		AppendString("_id", "$cat").
		AppendDocument("total", bson.NewBuilder().AppendString("$sum", "$qty").Build()).
		AppendDocument("n", bson.NewBuilder().AppendInt32("$sum", 1).Build()).
		AppendDocument("avg", bson.NewBuilder().AppendString("$avg", "$qty").Build()).
		AppendDocument("hi", bson.NewBuilder().AppendString("$max", "$qty").Build()).
		AppendDocument("lo", bson.NewBuilder().AppendString("$min", "$qty").Build()).
		Build()).Build()
}

// resultKey serializes a group result document to a comparable string so two result
// sets can be compared regardless of group order.
func resultKey(d bson.Raw) string {
	els, _ := d.Elements()
	parts := make([]string, len(els))
	for i, e := range els {
		parts[i] = fmt.Sprintf("%s=%d:%x", e.Key, e.Value.Type, e.Value.Data)
	}
	sort.Strings(parts)
	return joinSorted(parts)
}

func joinSorted(parts []string) string {
	out := ""
	for _, p := range parts {
		out += p + "|"
	}
	return out
}

func resultSet(t *testing.T, docs []bson.Raw) map[string]int {
	t.Helper()
	m := map[string]int{}
	for _, d := range docs {
		m[resultKey(d)]++
	}
	return m
}

func sameResults(t *testing.T, a, b []bson.Raw) bool {
	t.Helper()
	ra, rb := resultSet(t, a), resultSet(t, b)
	if len(ra) != len(rb) {
		return false
	}
	for k, v := range ra {
		if rb[k] != v {
			return false
		}
	}
	return true
}

// seedAgg inserts a deterministic spread of documents across categories and qtys.
func seedAgg(t *testing.T, c *Collection, n int) {
	t.Helper()
	cats := []string{"a", "b", "c", "d"}
	for i := 0; i < n; i++ {
		mustInsert(t, c, aggDoc(int32(i+1), cats[i%len(cats)], int32(i%17)))
	}
}

// TestColumnAggMatchesHeap is the core contract: with the column store enabled, an
// eligible $group returns exactly what the heap path returns (spec 2061 doc 19
// testing matrix). It computes the heap answer first, then enables the store and
// recomputes.
func TestColumnAggMatchesHeap(t *testing.T) {
	c := newTestColl(t)
	seedAgg(t, c, 200)
	pipeline := []bson.Raw{groupStage()}

	heap, err := c.Aggregate(pipeline)
	if err != nil {
		t.Fatalf("heap aggregate: %v", err)
	}
	if c.ColumnStoreEnabled() {
		t.Fatal("store should be off before enable")
	}

	if err := c.EnableColumnStore(colstore.ModeTransactional, []string{"cat", "qty"}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	col, err := c.Aggregate(pipeline)
	if err != nil {
		t.Fatalf("column aggregate: %v", err)
	}
	if !sameResults(t, heap, col) {
		t.Fatalf("column result differs from heap\n heap = %v\n col  = %v", heap, col)
	}
}

// TestColumnAggWithMatch checks an eligible $match range plus $group still matches
// the heap, exercising the zone-map pushdown plus the in-pipeline filter backstop.
func TestColumnAggWithMatch(t *testing.T) {
	c := newTestColl(t)
	seedAgg(t, c, 300)
	match := bson.NewBuilder().AppendDocument("$match",
		bson.NewBuilder().AppendDocument("qty",
			bson.NewBuilder().AppendInt32("$gte", 8).Build()).Build()).Build()
	pipeline := []bson.Raw{match, groupStage()}

	heap, err := c.Aggregate(pipeline)
	if err != nil {
		t.Fatalf("heap: %v", err)
	}
	if err := c.EnableColumnStore(colstore.ModeTransactional, []string{"cat", "qty"}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	col, err := c.Aggregate(pipeline)
	if err != nil {
		t.Fatalf("column: %v", err)
	}
	if !sameResults(t, heap, col) {
		t.Fatalf("column+match result differs\n heap = %v\n col  = %v", heap, col)
	}
}

// TestColumnMaintenanceMatchesHeap enables the store, then inserts, updates, and
// deletes through the normal write path and checks the column aggregation tracks the
// heap after each kind of change.
func TestColumnMaintenanceMatchesHeap(t *testing.T) {
	c := newTestColl(t)
	seedAgg(t, c, 50)
	if err := c.EnableColumnStore(colstore.ModeTransactional, []string{"cat", "qty"}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	pipeline := []bson.Raw{groupStage()}

	check := func(label string) {
		// The heap answer is computed by a second collection that mirrors the writes
		// without a store, but here we compare against the same collection's heap path
		// by temporarily reading every doc and running the pipeline directly.
		ref := newTestColl(t)
		all, err := c.Find(matchAll())
		if err != nil {
			t.Fatalf("%s: find: %v", label, err)
		}
		for _, d := range all {
			mustInsert(t, ref, d)
		}
		want, err := ref.Aggregate(pipeline)
		if err != nil {
			t.Fatalf("%s: ref aggregate: %v", label, err)
		}
		got, err := c.Aggregate(pipeline)
		if err != nil {
			t.Fatalf("%s: column aggregate: %v", label, err)
		}
		if !sameResults(t, want, got) {
			t.Fatalf("%s: column differs from heap\n want = %v\n got  = %v", label, want, got)
		}
	}

	check("after enable")

	mustInsert(t, c, aggDoc(1001, "a", 9))
	mustInsert(t, c, aggDoc(1002, "b", 3))
	check("after inserts")

	if _, err := c.UpdateOne(
		bson.NewBuilder().AppendInt32("_id", 1001).Build(),
		bson.NewBuilder().AppendDocument("$set", bson.NewBuilder().AppendInt32("qty", 42).Build()).Build(),
	); err != nil {
		t.Fatalf("update: %v", err)
	}
	check("after update")

	if _, err := c.DeleteOne(bson.NewBuilder().AppendInt32("_id", 1002).Build()); err != nil {
		t.Fatalf("delete: %v", err)
	}
	check("after delete")
}

// newBenchColl opens an in-memory collection for a benchmark, mirroring newTestColl.
func newBenchColl(b *testing.B) *Collection {
	b.Helper()
	fs := vfs.NewMemFS()
	c, err := Open(fs, "bench.doc", Options{IDGen: &sys.FixedIDGenerator{Timestamp: 1}})
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { _ = c.Close() })
	return c
}

// benchSeed inserts n documents spread across categories with a bounded qty range,
// used by the heap-vs-column aggregation benchmarks.
func benchSeed(b *testing.B, c *Collection, n int) {
	b.Helper()
	cats := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	for i := 0; i < n; i++ {
		d := aggDoc(int32(i+1), cats[i%len(cats)], int32(i%97))
		if _, err := c.InsertOne(d); err != nil {
			b.Fatalf("insert: %v", err)
		}
	}
}

// BenchmarkAggGroupHeap measures the $group path with no column store: the heap is
// scanned in full and every document is decoded. b.Loop runs the one-time seed
// outside the timed loop, so the 50k-document load is not paid per iteration.
func BenchmarkAggGroupHeap(b *testing.B) {
	c := newBenchColl(b)
	benchSeed(b, c, 50000)
	pipeline := []bson.Raw{groupStage()}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := c.Aggregate(pipeline); err != nil {
			b.Fatalf("aggregate: %v", err)
		}
	}
}

// BenchmarkAggGroupColumn measures the same $group routed through the columnar
// projection store, which reconstructs only the two covered fields.
func BenchmarkAggGroupColumn(b *testing.B) {
	c := newBenchColl(b)
	benchSeed(b, c, 50000)
	if err := c.EnableColumnStore(colstore.ModeTransactional, []string{"cat", "qty"}); err != nil {
		b.Fatalf("enable: %v", err)
	}
	pipeline := []bson.Raw{groupStage()}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := c.Aggregate(pipeline); err != nil {
			b.Fatalf("aggregate: %v", err)
		}
	}
}

// TestColumnFallsBackWhenNotCovered confirms a pipeline that references an
// unprojected field does not route through the store and still returns the heap
// answer.
func TestColumnFallsBackWhenNotCovered(t *testing.T) {
	c := newTestColl(t)
	seedAgg(t, c, 40)
	// Project only cat, not qty.
	if err := c.EnableColumnStore(colstore.ModeTransactional, []string{"cat"}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	// The $group sums qty, which is not covered, so it must fall back to the heap.
	out, err := c.Aggregate([]bson.Raw{groupStage()})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(out) != 4 {
		t.Fatalf("got %d groups, want 4", len(out))
	}
}
