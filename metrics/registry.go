package metrics

import "sync"

// metricKind tags a registered vector so the exposition writer knows how to
// render it (TYPE line) and the snapshot knows which family it belongs to.
type metricKind int

const (
	kindCounter metricKind = iota
	kindGauge
	kindHistogram
)

// CounterVec is a family of counters sharing one metric name and label set.
type CounterVec struct {
	name, help string
	labels     []string
	mu         sync.Mutex
	series     map[string]*series[*Counter]
}

// GaugeVec is a family of gauges sharing one metric name and label set.
type GaugeVec struct {
	name, help string
	labels     []string
	mu         sync.Mutex
	series     map[string]*series[*Gauge]
}

// HistogramVec is a family of histograms sharing one metric name, label set, and
// bucket bounds.
type HistogramVec struct {
	name, help string
	labels     []string
	bounds     []float64
	mu         sync.Mutex
	series     map[string]*series[*Histogram]
}

// series binds one metric instance to the label values that select it.
type series[T any] struct {
	values []string
	metric T
}

// With returns the counter for the given label values, creating it on first use.
// The number of values must match the label names the vector was registered with.
func (v *CounterVec) With(values ...string) *Counter {
	v.mu.Lock()
	defer v.mu.Unlock()
	k := labelKey(values)
	if s := v.series[k]; s != nil {
		return s.metric
	}
	c := &Counter{}
	v.series[k] = &series[*Counter]{values: append([]string(nil), values...), metric: c}
	return c
}

// With returns the gauge for the given label values, creating it on first use.
func (v *GaugeVec) With(values ...string) *Gauge {
	v.mu.Lock()
	defer v.mu.Unlock()
	k := labelKey(values)
	if s := v.series[k]; s != nil {
		return s.metric
	}
	g := &Gauge{}
	v.series[k] = &series[*Gauge]{values: append([]string(nil), values...), metric: g}
	return g
}

// With returns the histogram for the given label values, creating it on first use.
func (v *HistogramVec) With(values ...string) *Histogram {
	v.mu.Lock()
	defer v.mu.Unlock()
	k := labelKey(values)
	if s := v.series[k]; s != nil {
		return s.metric
	}
	h := newHistogram(v.bounds)
	v.series[k] = &series[*Histogram]{values: append([]string(nil), values...), metric: h}
	return h
}

// Registry holds every metric vector by name in registration order, so the
// exposition output is stable across scrapes.
type Registry struct {
	mu       sync.RWMutex
	order    []string
	kinds    map[string]metricKind
	counters map[string]*CounterVec
	gauges   map[string]*GaugeVec
	histos   map[string]*HistogramVec
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{
		kinds:    map[string]metricKind{},
		counters: map[string]*CounterVec{},
		gauges:   map[string]*GaugeVec{},
		histos:   map[string]*HistogramVec{},
	}
}

// Counter registers (or returns) a counter vector. Re-registering the same name
// returns the existing vector so callers can fetch a handle without threading it.
func (r *Registry) Counter(name, help string, labels ...string) *CounterVec {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v := r.counters[name]; v != nil {
		return v
	}
	v := &CounterVec{name: name, help: help, labels: labels, series: map[string]*series[*Counter]{}}
	r.counters[name] = v
	r.kinds[name] = kindCounter
	r.order = append(r.order, name)
	return v
}

// Gauge registers (or returns) a gauge vector.
func (r *Registry) Gauge(name, help string, labels ...string) *GaugeVec {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v := r.gauges[name]; v != nil {
		return v
	}
	v := &GaugeVec{name: name, help: help, labels: labels, series: map[string]*series[*Gauge]{}}
	r.gauges[name] = v
	r.kinds[name] = kindGauge
	r.order = append(r.order, name)
	return v
}

// Histogram registers (or returns) a histogram vector. Passing nil bounds uses
// DefaultLatencyBuckets.
func (r *Registry) Histogram(name, help string, bounds []float64, labels ...string) *HistogramVec {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v := r.histos[name]; v != nil {
		return v
	}
	if bounds == nil {
		bounds = DefaultLatencyBuckets
	}
	v := &HistogramVec{name: name, help: help, labels: labels, bounds: bounds, series: map[string]*series[*Histogram]{}}
	r.histos[name] = v
	r.kinds[name] = kindHistogram
	r.order = append(r.order, name)
	return v
}
