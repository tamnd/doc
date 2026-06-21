package metrics

import (
	"io"
	"testing"
)

func BenchmarkCounterInc(b *testing.B) {
	var c Counter
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		c.Inc()
	}
}

func BenchmarkHistogramObserve(b *testing.B) {
	h := newHistogram(DefaultLatencyBuckets)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		h.Observe(0.003)
	}
}

func BenchmarkVecWith(b *testing.B) {
	r := New()
	v := r.Counter("doc_ops_total", "ops", "op", "collection")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		v.With("find", "app.users").Inc()
	}
}

func BenchmarkWritePrometheus(b *testing.B) {
	r := New()
	c := r.Counter("doc_ops_total", "ops", "op", "collection")
	h := r.Histogram("doc_op_duration_seconds", "dur", nil, "op", "collection")
	for _, op := range []string{"find", "insert", "update", "delete", "count"} {
		c.With(op, "app.users").Add(100)
		h.With(op, "app.users").Observe(0.01)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = r.WritePrometheus(io.Discard)
	}
}
