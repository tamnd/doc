// Package metrics is doc's in-process metric registry (spec 2061 doc 18 §2). It
// follows the Prometheus data model: counters that only go up, gauges that hold a
// current value, and histograms that bucket observations. Every metric is a vector
// with a fixed set of label names; an unlabelled metric is just a vector with no
// labels and a single series.
//
// The registry is always live and cheap: counters and gauges are single atomic
// integers, and a histogram observation is a bucket search plus a couple of atomic
// adds. Nothing here imports the rest of doc, so the pager, the engine, and the
// public layer can all feed one registry without an import cycle. The Prometheus
// text exposition lives in prometheus.go and reads the same registry.
package metrics

import (
	"sort"
	"sync"
	"sync/atomic"
)

// DefaultLatencyBuckets is the upper-bound set the spec fixes for latency
// histograms, in seconds (spec 2061 doc 18 §2.1). It matches the OpenTelemetry
// semantic convention for database client latency.
var DefaultLatencyBuckets = []float64{
	0.0001, 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0,
}

// Counter is a monotonically increasing value backed by one atomic integer.
type Counter struct{ v atomic.Int64 }

// Inc adds one to the counter.
func (c *Counter) Inc() { c.v.Add(1) }

// Add adds n to the counter. n is expected to be non-negative; the type does not
// enforce it so callers can fold in batch counts.
func (c *Counter) Add(n int64) { c.v.Add(n) }

// Value returns the current count.
func (c *Counter) Value() int64 { return c.v.Load() }

// Gauge is a value that can move in either direction.
type Gauge struct{ v atomic.Int64 }

// Set replaces the gauge value.
func (g *Gauge) Set(n int64) { g.v.Store(n) }

// Add adds n (which may be negative) to the gauge.
func (g *Gauge) Add(n int64) { g.v.Add(n) }

// Value returns the current value.
func (g *Gauge) Value() int64 { return g.v.Load() }

// Histogram counts observations into cumulative buckets and tracks the running
// sum and count, the three numbers Prometheus needs to expose a histogram.
type Histogram struct {
	bounds []float64 // upper bounds, ascending; the +Inf bucket is implicit

	counts []atomic.Uint64 // per-bucket counts, len(bounds)+1; index len(bounds) is +Inf
	total  atomic.Uint64

	sumMu sync.Mutex
	sum   float64
}

func newHistogram(bounds []float64) *Histogram {
	b := make([]float64, len(bounds))
	copy(b, bounds)
	sort.Float64s(b)
	return &Histogram{bounds: b, counts: make([]atomic.Uint64, len(b)+1)}
}

// Observe records one value. It finds the lowest bucket whose upper bound is at
// least v (the +Inf bucket catches everything larger) and increments it.
func (h *Histogram) Observe(v float64) {
	i := sort.SearchFloat64s(h.bounds, v)
	h.counts[i].Add(1)
	h.total.Add(1)
	h.sumMu.Lock()
	h.sum += v
	h.sumMu.Unlock()
}

// Snapshot returns the cumulative bucket counts (each bucket including every
// lower one), the total count, and the running sum.
func (h *Histogram) Snapshot() HistogramSnapshot {
	bounds := make([]float64, len(h.bounds))
	copy(bounds, h.bounds)
	cum := make([]uint64, len(h.counts))
	var running uint64
	for i := range h.counts {
		running += h.counts[i].Load()
		cum[i] = running
	}
	h.sumMu.Lock()
	sum := h.sum
	h.sumMu.Unlock()
	return HistogramSnapshot{Bounds: bounds, CumulativeCounts: cum, Count: h.total.Load(), Sum: sum}
}

// HistogramSnapshot is a point-in-time view of a histogram for exposition.
type HistogramSnapshot struct {
	Bounds           []float64
	CumulativeCounts []uint64 // len(Bounds)+1; the last entry is the +Inf bucket
	Count            uint64
	Sum              float64
}

// labelKey joins label values into a single map key. The values never contain a
// NUL, so the separator is unambiguous.
func labelKey(values []string) string {
	if len(values) == 0 {
		return ""
	}
	n := len(values) - 1
	for _, v := range values {
		n += len(v)
	}
	var b []byte
	b = make([]byte, 0, n)
	for i, v := range values {
		if i > 0 {
			b = append(b, 0)
		}
		b = append(b, v...)
	}
	return string(b)
}
