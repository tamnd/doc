package metrics

import (
	"io"
	"sort"
	"strconv"
	"strings"
)

// WritePrometheus writes the whole registry in the Prometheus text exposition
// format, version 0.0.4 (spec 2061 doc 18 §2.4). Metrics come out in registration
// order; series within a metric come out sorted by their label values so two
// scrapes of an unchanged registry are byte-identical.
func (r *Registry) WritePrometheus(w io.Writer) error {
	r.mu.RLock()
	order := append([]string(nil), r.order...)
	r.mu.RUnlock()

	bw := &errWriter{w: w}
	for _, name := range order {
		r.mu.RLock()
		kind := r.kinds[name]
		r.mu.RUnlock()
		switch kind {
		case kindCounter:
			writeCounterVec(bw, r.counters[name])
		case kindGauge:
			writeGaugeVec(bw, r.gauges[name])
		case kindHistogram:
			writeHistogramVec(bw, r.histos[name])
		}
	}
	return bw.err
}

func writeCounterVec(w *errWriter, v *CounterVec) {
	if v == nil {
		return
	}
	writeHeader(w, v.name, v.help, "counter")
	v.mu.Lock()
	rows := sortedRows(v.series)
	for _, s := range rows {
		w.line(v.name + labelString(v.labels, s.values) + " " + itoa(s.metric.Value()))
	}
	v.mu.Unlock()
}

func writeGaugeVec(w *errWriter, v *GaugeVec) {
	if v == nil {
		return
	}
	writeHeader(w, v.name, v.help, "gauge")
	v.mu.Lock()
	rows := sortedRows(v.series)
	for _, s := range rows {
		w.line(v.name + labelString(v.labels, s.values) + " " + itoa(s.metric.Value()))
	}
	v.mu.Unlock()
}

func writeHistogramVec(w *errWriter, v *HistogramVec) {
	if v == nil {
		return
	}
	writeHeader(w, v.name, v.help, "histogram")
	v.mu.Lock()
	rows := sortedRows(v.series)
	for _, s := range rows {
		snap := s.metric.Snapshot()
		for i, ub := range snap.Bounds {
			le := labelWith(v.labels, s.values, "le", formatFloat(ub))
			w.line(v.name + "_bucket" + le + " " + utoa(snap.CumulativeCounts[i]))
		}
		leInf := labelWith(v.labels, s.values, "le", "+Inf")
		w.line(v.name + "_bucket" + leInf + " " + utoa(snap.CumulativeCounts[len(snap.Bounds)]))
		base := labelString(v.labels, s.values)
		w.line(v.name + "_sum" + base + " " + formatFloat(snap.Sum))
		w.line(v.name + "_count" + base + " " + utoa(snap.Count))
	}
	v.mu.Unlock()
}

func writeHeader(w *errWriter, name, help, typ string) {
	if help != "" {
		w.line("# HELP " + name + " " + help)
	}
	w.line("# TYPE " + name + " " + typ)
}

// sortedRows returns a vector's series sorted by joined label values for stable
// output.
func sortedRows[T any](m map[string]*series[T]) []*series[T] {
	rows := make([]*series[T], 0, len(m))
	for _, s := range m {
		rows = append(rows, s)
	}
	sort.Slice(rows, func(i, j int) bool {
		return strings.Join(rows[i].values, "\x00") < strings.Join(rows[j].values, "\x00")
	})
	return rows
}

// labelString renders {name="value",...} for the given label names and values,
// or the empty string when there are no labels.
func labelString(names, values []string) string {
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n)
		b.WriteString(`="`)
		if i < len(values) {
			b.WriteString(escapeLabel(values[i]))
		}
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// labelWith renders the base labels plus one extra label appended last, used for
// the histogram "le" bucket label.
func labelWith(names, values []string, extraName, extraValue string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, n := range names {
		b.WriteString(n)
		b.WriteString(`="`)
		if i < len(values) {
			b.WriteString(escapeLabel(values[i]))
		}
		b.WriteString(`",`)
	}
	b.WriteString(extraName)
	b.WriteString(`="`)
	b.WriteString(extraValue)
	b.WriteString(`"}`)
	return b.String()
}

func escapeLabel(s string) string {
	if !strings.ContainsAny(s, `\"`+"\n") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

func itoa(n int64) string  { return strconv.FormatInt(n, 10) }
func utoa(n uint64) string { return strconv.FormatUint(n, 10) }

func formatFloat(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

// errWriter accumulates the first write error so the exposition loop stays flat.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) line(s string) {
	if e.err != nil {
		return
	}
	if _, err := io.WriteString(e.w, s); err != nil {
		e.err = err
		return
	}
	_, e.err = io.WriteString(e.w, "\n")
}
