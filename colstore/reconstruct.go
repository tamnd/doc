package colstore

import "github.com/tamnd/doc/bson"

// Reconstruct returns one document per visible row at snapshot snap, carrying only
// the requested covered fields, with the range predicate's segments pruned via the
// zone map. A null or missing field is left out of the rebuilt document so it reads
// the same way an absent field does on the heap path, which keeps grouping and
// accumulation identical to the heap (spec 2061 doc 04 §10.5). This is the source a
// covered aggregation reads instead of scanning the heap.
func (s *Store) Reconstruct(snap uint64, fields []string, pred *RangePred) []bson.Raw {
	var out []bson.Raw
	_ = s.scan(snap, fields, pred, func(row []Value) {
		b := bson.NewBuilder()
		for j, f := range fields {
			if row[j].Kind == KindNull {
				continue
			}
			b.AppendValue(f, row[j].ToRawValue())
		}
		out = append(out, b.Build())
	})
	return out
}

// Cost-model constants for the planner's column-versus-heap decision (spec 2061 doc
// 04 §10.5). They are ratios, not absolute timings: a heap scan pays roughly one
// unit per document fetched, while a covered column scan pays one unit per segment
// read after zone-map skipping and nothing per document, because no field outside
// the projection is materialized. The break-even is therefore about one segment's
// worth of rows; below that the per-query column setup is not worth it and the heap
// path wins. These were calibrated against BenchmarkGroupSum: the column path pulls
// ahead once a collection spans more than a couple of segments.
const (
	heapFetchCost   = 1.0
	segmentReadCost = 1.0
	columnSetupCost = float64(SegmentSize) // amortizes the per-query column setup
)

// PreferOverHeap reports whether the planner should run a covered aggregation
// through the column store rather than a heap scan, using the §10.5 cost model. A
// covered query fetches no documents from the heap, so its cost is the number of
// segments it must read after zone-map skipping; the heap alternative costs one unit
// per live document. The decision favors the column store once the collection is
// large enough that reading a handful of segments beats reading every document.
func (s *Store) PreferOverHeap(snap uint64, pred *RangePred) bool {
	rows := s.RowCount(snap)
	if rows == 0 {
		return false
	}
	costHeap := heapFetchCost * float64(rows)

	segs := s.SegmentCount()
	skipRate := s.estimateSkipRate(pred)
	costColumn := columnSetupCost + segmentReadCost*float64(segs)*(1-skipRate)
	return costColumn < costHeap
}

// estimateSkipRate estimates the fraction of segments a range predicate prunes via
// zone maps, by checking each segment's zone against the bound. It is exact for the
// current segment set, which is what the cost model wants.
func (s *Store) estimateSkipRate(pred *RangePred) float64 {
	if pred == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	segs, ok := s.segs[pred.Field]
	if !ok || len(segs) == 0 {
		return 0
	}
	skipped := 0
	for _, seg := range segs {
		if seg.skipForRange(pred.Op, pred.Bound) {
			skipped++
		}
	}
	return float64(skipped) / float64(len(segs))
}
