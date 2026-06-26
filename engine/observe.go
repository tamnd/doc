package engine

import "github.com/tamnd/doc/pager"

// PagerStats returns the storage layer's I/O and cache accounting, the source of
// the storage half of the metric catalogue (spec 2061 doc 18 §2.3).
func (e *Engine) PagerStats() pager.Stats { return e.pgr.Stats() }

// GaugeSnapshot is the catalog-derived state the metric gauges report: how many
// collections and indexes exist, their estimated document counts and on-disk
// sizes, and the file's space accounting (spec 2061 doc 18 §2.3). It is read on
// demand rather than tracked incrementally, so it is always consistent with the
// catalog even after a crash recovery.
type GaugeSnapshot struct {
	Collections   int64
	Indexes       int64
	FileSizeBytes int64
	FreelistPages int64
	PerCollection []CollectionGauge
}

// CollectionGauge carries one collection's gauge values, labelled by namespace.
type CollectionGauge struct {
	Namespace     string
	DocumentCount int64
	Indexes       []IndexGauge
}

// IndexGauge carries one index's on-disk size, labelled by collection and index
// name.
type IndexGauge struct {
	Namespace string
	Name      string
	SizeBytes int64
}

// Gauges walks the catalog and every open collection to produce the current
// gauge values. It takes the engine lock only to snapshot the namespace list, then
// gathers per-collection stats through the same path collStats uses.
func (e *Engine) Gauges() GaugeSnapshot {
	e.mu.Lock()
	recs := e.mcat.ListCollections("")
	e.mu.Unlock()

	ps := e.pgr.Stats()
	out := GaugeSnapshot{
		FileSizeBytes: int64(ps.FileSizePages * ps.PageSize),
		FreelistPages: int64(ps.FreelistPages),
	}
	for _, rec := range recs {
		cs, err := e.CollectionStats(rec.DBName, rec.Name)
		if err != nil {
			continue
		}
		out.Collections++
		cg := CollectionGauge{Namespace: cs.Namespace, DocumentCount: cs.DocumentCount}
		for name, sz := range cs.IndexSizes {
			out.Indexes++
			cg.Indexes = append(cg.Indexes, IndexGauge{Namespace: cs.Namespace, Name: name, SizeBytes: sz})
		}
		out.PerCollection = append(out.PerCollection, cg)
	}
	return out
}
