package collection

import (
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/colstore"
)

// This file is the doc 19 §12.3 latency-budget verification harness. It measures the
// p50/p99/p999/max of the operations the latency table names and reports each against
// its normative target. The targets in §12.3 are defined on the primary reference
// machine (Linux amd64, 32-core, NVMe). This harness runs wherever the suite runs,
// which in development is the §12.2 secondary reference (macOS arm64, Apple M-series),
// so it does not gate the milestone on the absolute numbers: a missed target on the
// secondary reference is reported, not failed. What it does enforce is a generous
// sanity ceiling per operation, set far above the target, so a pathological regression
// (a quadratic scan, a lock held across the read, an accidental fsync per op) still
// fails the gate. Treat the logged percentiles as the signal; treat the assertions as
// a smoke alarm.
//
// The latency targets reproduced here, from §12.3:
//   FindOne by _id (warm)      p50 < 50 µs   p99 < 200 µs   p999 < 1 ms
//   $group over 1M, columnar   p50 < 50 ms   p99 < 200 ms   p999 < 1 s
//
// The $group row is measured at a smaller document count than 1M so the suite stays
// fast; the per-document cost is what matters for extrapolation, and the harness logs
// the document count it actually used.

// percentile returns the p-quantile (0..1) of an already-sorted duration sample.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// latencyReport sorts the samples, logs the percentile row against the target, and
// returns the measured p50/p99/p999 for the caller's sanity assertions.
func latencyReport(t *testing.T, op string, samples []time.Duration, tP50, tP99, tP999 time.Duration) (p50, p99, p999 time.Duration) {
	t.Helper()
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	p50 = percentile(samples, 0.50)
	p99 = percentile(samples, 0.99)
	p999 = percentile(samples, 0.999)
	max := samples[len(samples)-1]
	mark := func(got, target time.Duration) string {
		if got <= target {
			return "ok"
		}
		return "over"
	}
	t.Logf("%s (n=%d, secondary reference):", op, len(samples))
	t.Logf("    p50  %12v  target %12v  %s", p50, tP50, mark(p50, tP50))
	t.Logf("    p99  %12v  target %12v  %s", p99, tP99, mark(p99, tP99))
	t.Logf("    p999 %12v  target %12v  %s", p999, tP999, mark(p999, tP999))
	t.Logf("    max  %12v", max)
	return p50, p99, p999
}

// TestLatencyBudgetFindOneByID measures the warm point-read latency distribution. The
// dataset is fully in memory (the overlay engine holds the version-chain map), so this
// is the warm-cache regime the §12.3 row specifies.
func TestLatencyBudgetFindOneByID(t *testing.T) {
	if testing.Short() {
		t.Skip("latency harness skipped under -short")
	}
	if raceEnabled {
		t.Skip("latency budgets measure wall-clock latency and mean nothing under the race detector")
	}
	const n = 10000
	c := warmAllocColl(t, n)

	// Warm up: touch a spread of ids so any lazy structure is built before measuring.
	for i := 0; i < n; i += 100 {
		if _, err := c.FindOne(filterID(int32(i + 1))); err != nil {
			t.Fatalf("warmup: %v", err)
		}
	}

	rng := rand.New(rand.NewSource(1))
	const samples = 20000
	lat := make([]time.Duration, samples)
	for i := 0; i < samples; i++ {
		id := int32(rng.Intn(n) + 1)
		f := filterID(id)
		start := time.Now()
		doc, err := c.FindOne(f)
		lat[i] = time.Since(start)
		if err != nil || doc == nil {
			t.Fatalf("find id %d: doc=%v err=%v", id, doc, err)
		}
	}

	p50, p99, p999 := latencyReport(t, "FindOne by _id",
		lat, 50*time.Microsecond, 200*time.Microsecond, time.Millisecond)

	// Sanity ceilings: 100x the targets. A warm point read this far off target is a
	// structural regression, not hardware variance.
	assertUnder(t, "FindOne p50", p50, 5*time.Millisecond)
	assertUnder(t, "FindOne p99", p99, 20*time.Millisecond)
	assertUnder(t, "FindOne p999", p999, 100*time.Millisecond)
}

// TestLatencyBudgetGroupColumnar measures the $group latency with the columnar store
// on, the row the M9-e accumulator vectorization targets. It runs many iterations of
// the aggregate over a fixed dataset and reports the distribution.
func TestLatencyBudgetGroupColumnar(t *testing.T) {
	if testing.Short() {
		t.Skip("latency harness skipped under -short")
	}
	if raceEnabled {
		t.Skip("latency budgets measure wall-clock latency and mean nothing under the race detector")
	}
	const n = 50000
	c := newTestColl(t)
	cats := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	for i := 0; i < n; i++ {
		c.mustSeedAgg(t, int32(i+1), cats[i%len(cats)], int32(i%17))
	}
	if err := c.EnableColumnStore(colstore.ModeTransactional, []string{"cat", "qty"}); err != nil {
		t.Fatalf("enable column store: %v", err)
	}
	pipeline := []bson.Raw{groupStage()}

	// Confirm the vectorized path is the one under test, not a silent heap fallback.
	tx := c.BeginReadOnly()
	_, ok := tx.columnGroup(pipeline)
	_ = tx.Rollback()
	if !ok {
		t.Fatal("columnar $group path did not engage; latency row would not measure M9-e")
	}

	// Warmup.
	for i := 0; i < 3; i++ {
		if _, err := c.Aggregate(pipeline); err != nil {
			t.Fatalf("warmup aggregate: %v", err)
		}
	}

	const iters = 200
	lat := make([]time.Duration, iters)
	for i := 0; i < iters; i++ {
		start := time.Now()
		out, err := c.Aggregate(pipeline)
		lat[i] = time.Since(start)
		if err != nil {
			t.Fatalf("aggregate: %v", err)
		}
		if len(out) == 0 {
			t.Fatal("aggregate returned no groups")
		}
	}

	t.Logf("$group columnar measured at n=%d docs (target row is 1M; extrapolate per-doc)", n)
	p50, p99, p999 := latencyReport(t, "$group columnar",
		lat, 50*time.Millisecond, 200*time.Millisecond, time.Second)

	// Sanity ceilings at the 1M-target levels even though we measure 50k: a 50k-doc
	// group that takes longer than the 1M-doc p50 target is a clear regression.
	assertUnder(t, "$group p50", p50, 50*time.Millisecond)
	assertUnder(t, "$group p99", p99, 200*time.Millisecond)
	assertUnder(t, "$group p999", p999, time.Second)
}

func assertUnder(t *testing.T, label string, got, ceiling time.Duration) {
	t.Helper()
	if got > ceiling {
		t.Errorf("%s: %v exceeds sanity ceiling %v", label, got, ceiling)
	}
}

// mustSeedAgg inserts one {_id, cat, qty} document for the latency harness, failing the
// test on error. It mirrors aggDoc/mustInsert without importing the benchmark helpers.
func (c *Collection) mustSeedAgg(t *testing.T, id int32, cat string, qty int32) {
	t.Helper()
	d := bson.NewBuilder().
		AppendInt32("_id", id).
		AppendString("cat", cat).
		AppendInt32("qty", qty).
		Build()
	if _, err := c.InsertOne(d); err != nil {
		t.Fatalf("seed agg insert: %v", err)
	}
}
