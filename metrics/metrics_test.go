package metrics

import (
	"strings"
	"testing"
)

func TestCounterAndGauge(t *testing.T) {
	var c Counter
	c.Inc()
	c.Add(4)
	if c.Value() != 5 {
		t.Fatalf("counter = %d, want 5", c.Value())
	}
	var g Gauge
	g.Set(10)
	g.Add(-3)
	if g.Value() != 7 {
		t.Fatalf("gauge = %d, want 7", g.Value())
	}
}

func TestHistogramBucketing(t *testing.T) {
	h := newHistogram([]float64{0.01, 0.1, 1.0})
	for _, v := range []float64{0.005, 0.05, 0.5, 5.0} {
		h.Observe(v)
	}
	snap := h.Snapshot()
	// cumulative: <=0.01 has 1, <=0.1 has 2, <=1.0 has 3, +Inf has 4
	want := []uint64{1, 2, 3, 4}
	for i, w := range want {
		if snap.CumulativeCounts[i] != w {
			t.Fatalf("bucket %d = %d, want %d", i, snap.CumulativeCounts[i], w)
		}
	}
	if snap.Count != 4 {
		t.Fatalf("count = %d, want 4", snap.Count)
	}
	if snap.Sum < 5.555-1e-9 || snap.Sum > 5.555+1e-9 {
		t.Fatalf("sum = %v, want 5.555", snap.Sum)
	}
}

func TestVecSeriesAreStable(t *testing.T) {
	r := New()
	v := r.Counter("doc_ops_total", "ops", "op", "collection")
	v.With("find", "db.a").Inc()
	v.With("find", "db.a").Inc()
	v.With("insert", "db.b").Add(3)
	if got := v.With("find", "db.a").Value(); got != 2 {
		t.Fatalf("find/db.a = %d, want 2", got)
	}
	if got := v.With("insert", "db.b").Value(); got != 3 {
		t.Fatalf("insert/db.b = %d, want 3", got)
	}
}

func TestPrometheusExposition(t *testing.T) {
	r := New()
	c := r.Counter("doc_ops_total", "Operations completed.", "op")
	c.With("find").Add(2)
	c.With("insert").Inc()
	g := r.Gauge("doc_collection_count", "Collections.")
	g.With().Set(4)
	h := r.Histogram("doc_op_duration_seconds", "Duration.", []float64{0.1, 1.0}, "op")
	h.With("find").Observe(0.05)

	var b strings.Builder
	if err := r.WritePrometheus(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()

	for _, want := range []string{
		"# TYPE doc_ops_total counter",
		`doc_ops_total{op="find"} 2`,
		`doc_ops_total{op="insert"} 1`,
		"# TYPE doc_collection_count gauge",
		"doc_collection_count 4",
		"# TYPE doc_op_duration_seconds histogram",
		`doc_op_duration_seconds_bucket{op="find",le="0.1"} 1`,
		`doc_op_duration_seconds_bucket{op="find",le="+Inf"} 1`,
		`doc_op_duration_seconds_count{op="find"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("exposition missing %q\nfull output:\n%s", want, out)
		}
	}
}

func TestExpositionIsDeterministic(t *testing.T) {
	r := New()
	c := r.Counter("doc_ops_total", "ops", "op")
	c.With("zeta").Inc()
	c.With("alpha").Inc()
	c.With("mu").Inc()

	var a, b strings.Builder
	_ = r.WritePrometheus(&a)
	_ = r.WritePrometheus(&b)
	if a.String() != b.String() {
		t.Fatal("two scrapes differ")
	}
	// alpha sorts before mu before zeta.
	out := a.String()
	ia := strings.Index(out, `op="alpha"`)
	im := strings.Index(out, `op="mu"`)
	iz := strings.Index(out, `op="zeta"`)
	if ia >= im || im >= iz {
		t.Fatalf("series not sorted: alpha=%d mu=%d zeta=%d", ia, im, iz)
	}
}

func TestLabelEscaping(t *testing.T) {
	r := New()
	c := r.Counter("doc_ops_total", "ops", "ns")
	c.With(`weird"\ns`).Inc()
	var b strings.Builder
	_ = r.WritePrometheus(&b)
	if !strings.Contains(b.String(), `ns="weird\"\\ns"`) {
		t.Fatalf("label not escaped: %s", b.String())
	}
}
