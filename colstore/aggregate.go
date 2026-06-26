package colstore

// This file is the column-at-a-time execution path the engine runs aggregation
// through (spec 2061 doc 04 §10.5, doc 11, doc 19 §6.3). A scan decodes one segment
// per field at a time, skips whole segments a range predicate cannot match via the
// zone map, and applies MVCC visibility per entry. The accumulators are tight range
// loops over the decoded values so the compiler can vectorize the numeric reductions.

// RangePred is an optional pushdown predicate on one field: rows where Field Op
// Bound holds survive, and a segment whose zone map rules the predicate out is
// skipped without decoding. Op is one of $gt, $gte, $lt, $lte, $eq.
type RangePred struct {
	Field string
	Op    string
	Bound Value
}

// matches reports whether v satisfies the predicate.
func (p *RangePred) matches(v Value) bool {
	c, ok := compareNumericOrString(v, p.Bound)
	if !ok {
		return false
	}
	switch p.Op {
	case "$gt":
		return c > 0
	case "$gte":
		return c >= 0
	case "$lt":
		return c < 0
	case "$lte":
		return c <= 0
	case "$eq":
		return c == 0
	default:
		return false
	}
}

// GroupAgg holds the running accumulators for one group. Avg is Sum/NumCount; the
// caller selects whichever accumulator the pipeline asked for.
type GroupAgg struct {
	Key      Value
	Sum      float64
	NumCount int64 // numeric values seen, the $avg denominator
	Count    int64 // all rows in the group, the $count / $sum:1 value
	Min      Value
	Max      Value
	hasExt   bool // Min/Max have been set
}

// Avg returns the mean of the numeric values, and whether any were seen.
func (g *GroupAgg) Avg() (float64, bool) {
	if g.NumCount == 0 {
		return 0, false
	}
	return g.Sum / float64(g.NumCount), true
}

// scan walks every visible row at snapshot snap, calling fn with the decoded values
// of the requested fields in order. A range predicate prunes segments via the zone
// map and filters surviving rows.
func (s *Store) scan(snap uint64, need []string, pred *RangePred, fn func(row []Value)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(need) == 0 {
		return nil
	}
	// The visibility field is any projected field, since write and delete stamps are
	// row-aligned across fields.
	visField := s.fields[0]
	row := make([]Value, len(need))

	for k := range s.bounds {
		if pred != nil {
			if segs, ok := s.segs[pred.Field]; ok && segs[k].skipForRange(pred.Op, pred.Bound) {
				continue
			}
		}
		cols := make([][]Value, len(need))
		for j, f := range need {
			c, err := s.segs[f][k].Values()
			if err != nil {
				return err
			}
			cols[j] = c
		}
		var predCol []Value
		if pred != nil {
			if c, err := s.predColumn(pred.Field, k, need, cols); err != nil {
				return err
			} else {
				predCol = c
			}
		}
		vis := s.segs[visField][k]
		for i := 0; i < vis.n; i++ {
			if !vis.visible(i, snap) {
				continue
			}
			if pred != nil && !pred.matches(predCol[i]) {
				continue
			}
			for j := range need {
				row[j] = cols[j][i]
			}
			fn(row)
		}
	}

	// The in-progress buffer holds decoded values directly; no zone skip applies.
	for i := range s.bufRIDs {
		if s.bufWrite[i] > snap || (s.bufDel[i] != 0 && s.bufDel[i] <= snap) {
			continue
		}
		if pred != nil && !pred.matches(s.bufVals[pred.Field][i]) {
			continue
		}
		for j, f := range need {
			row[j] = s.bufVals[f][i]
		}
		fn(row)
	}
	return nil
}

// predColumn returns the predicate field's decoded column for block k, reusing an
// already-decoded column from need when possible.
func (s *Store) predColumn(field string, k int, need []string, cols [][]Value) ([]Value, error) {
	for j, f := range need {
		if f == field {
			return cols[j], nil
		}
	}
	return s.segs[field][k].Values()
}

// GroupBy runs a single-field group-by with the standard accumulators over a value
// field, at snapshot snap, with an optional pushdown predicate. groupField "" puts
// every row in one null-keyed group (a whole-collection aggregate); valueField ""
// accumulates only counts. Groups come back in first-seen order, matching the
// natural-order group output of the heap path.
func (s *Store) GroupBy(snap uint64, groupField, valueField string, pred *RangePred) []*GroupAgg {
	need := dedupFields(groupField, valueField, predField(pred))
	groupIdx, valueIdx := indexOf(need, groupField), indexOf(need, valueField)

	byKey := make(map[string]*GroupAgg)
	var order []*GroupAgg
	_ = s.scan(snap, need, pred, func(row []Value) {
		key := NullValue
		if groupIdx >= 0 {
			key = row[groupIdx]
		}
		hk := key.hashKey()
		g := byKey[hk]
		if g == nil {
			g = &GroupAgg{Key: key}
			byKey[hk] = g
			order = append(order, g)
		}
		g.Count++
		if valueIdx >= 0 {
			accumulate(g, row[valueIdx])
		}
	})
	return order
}

// accumulate folds one value into a group: numeric values feed Sum and NumCount,
// and any zone-comparable value updates Min and Max.
func accumulate(g *GroupAgg, v Value) {
	if f, ok := v.AsFloat(); ok {
		g.Sum += f
		g.NumCount++
	}
	if !v.comparableForZone() {
		return
	}
	if !g.hasExt {
		g.Min, g.Max, g.hasExt = v, v, true
		return
	}
	if c, ok := compareNumericOrString(v, g.Min); ok && c < 0 {
		g.Min = v
	}
	if c, ok := compareNumericOrString(v, g.Max); ok && c > 0 {
		g.Max = v
	}
}

// SumField returns the sum and the numeric count of one field over all visible rows
// at snap, through the vectorized path: it gathers the visible values into a typed
// float slice per segment and reduces with a tight range loop (spec 2061 doc 19
// §6.3). It is the whole-collection $sum / $avg fast path.
func (s *Store) SumField(snap uint64, field string, pred *RangePred) (sum float64, count int64) {
	_ = s.scanFloats(snap, field, pred, func(vals []float64) {
		var local float64
		for _, x := range vals { // tight reduction the compiler can vectorize
			local += x
		}
		sum += local
		count += int64(len(vals))
	})
	return sum, count
}

// scanFloats gathers the visible numeric values of field per segment into a reused
// float slice and hands each batch to fn, the column-at-a-time shape §6.3 calls for.
func (s *Store) scanFloats(snap uint64, field string, pred *RangePred, fn func(batch []float64)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	visField := s.fields[0]
	var batch []float64

	for k := range s.bounds {
		if pred != nil {
			if segs, ok := s.segs[pred.Field]; ok && segs[k].skipForRange(pred.Op, pred.Bound) {
				continue
			}
		}
		col, err := s.segs[field][k].Values()
		if err != nil {
			return err
		}
		var predCol []Value
		if pred != nil {
			if pred.Field == field {
				predCol = col
			} else {
				if predCol, err = s.segs[pred.Field][k].Values(); err != nil {
					return err
				}
			}
		}
		vis := s.segs[visField][k]
		batch = batch[:0]
		for i := 0; i < vis.n; i++ {
			if !vis.visible(i, snap) {
				continue
			}
			if pred != nil && !pred.matches(predCol[i]) {
				continue
			}
			if f, ok := col[i].AsFloat(); ok {
				batch = append(batch, f)
			}
		}
		fn(batch)
	}

	batch = batch[:0]
	for i := range s.bufRIDs {
		if s.bufWrite[i] > snap || (s.bufDel[i] != 0 && s.bufDel[i] <= snap) {
			continue
		}
		if pred != nil && !pred.matches(s.bufVals[pred.Field][i]) {
			continue
		}
		if f, ok := s.bufVals[field][i].AsFloat(); ok {
			batch = append(batch, f)
		}
	}
	fn(batch)
	return nil
}

// dedupFields builds the distinct, non-empty field list a scan needs.
func dedupFields(fs ...string) []string {
	seen := make(map[string]bool, len(fs))
	var out []string
	for _, f := range fs {
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

func predField(p *RangePred) string {
	if p == nil {
		return ""
	}
	return p.Field
}

func indexOf(fs []string, f string) int {
	if f == "" {
		return -1
	}
	for i, x := range fs {
		if x == f {
			return i
		}
	}
	return -1
}
