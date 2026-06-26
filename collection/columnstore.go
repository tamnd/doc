package collection

import (
	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/colstore"
)

// This file wires the columnar projection store (spec 2061 doc 04 §10) into the
// collection. The store is an optional derived structure: enabling it builds it from
// the current heap, the commit path keeps it current from the same change records the
// change feed sees, and a covered aggregation reads its compressed columns instead of
// the heap.
//
// The store accelerates the read but never changes the answer. A routed aggregation
// reconstructs only the covered fields it needs and runs the unchanged aggregation
// pipeline over them, so the result is identical to the heap path by construction
// (spec 2061 doc 19 testing matrix: "column-store path produces identical results as
// heap path"). The zone map prunes whole segments a range predicate cannot match,
// which is a pure skip: the pipeline's own $match still runs and is the source of
// truth for correctness.

// EnableColumnStore turns on the columnar projection store for the given fields and
// maintenance mode, building it from the collection's current committed state. An
// empty field list projects every top-level field observed in the current documents.
// Calling it with ModeOff tears the store down.
func (c *Collection) EnableColumnStore(mode colstore.Mode, fields []string) error {
	if mode == colstore.ModeOff {
		c.cstore = nil
		return nil
	}
	t := c.BeginReadOnly()
	defer func() { _ = t.Rollback() }()
	docs, err := t.Find(matchAll())
	if err != nil {
		return err
	}
	if len(fields) == 0 {
		fields = observedFields(docs)
	}
	store := colstore.New(mode, fields)
	store.Rebuild(docs, t.startVer)
	c.cstore = store
	return nil
}

// ColumnStoreEnabled reports whether a column store is attached.
func (c *Collection) ColumnStoreEnabled() bool { return c.cstore != nil }

// ColumnStoreFields returns the projected fields, or nil when no store is attached.
func (c *Collection) ColumnStoreFields() []string {
	if c.cstore == nil {
		return nil
	}
	return c.cstore.Fields()
}

// observedFields collects the distinct top-level field names across a set of
// documents, skipping _id (the group store never projects the identity column).
func observedFields(docs []bson.Raw) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range docs {
		els, err := d.Elements()
		if err != nil {
			continue
		}
		for _, e := range els {
			if e.Key == idFieldName || seen[e.Key] {
				continue
			}
			seen[e.Key] = true
			out = append(out, e.Key)
		}
	}
	return out
}

// maintainColumn applies one committed transaction's change records to the column
// store in transactional mode (spec 2061 doc 04 §10.4). Lazy mode does not maintain
// here; it is refreshed out of band through Rebuild.
func (c *Collection) maintainColumn(recs []ChangeRecord, cv uint64) {
	if c.cstore == nil || c.cstore.Mode() != colstore.ModeTransactional {
		return
	}
	for _, r := range recs {
		switch r.Op {
		case "insert":
			c.cstore.Insert(r.Doc, cv)
		case "update", "replace":
			c.cstore.Update(r.Before, r.Doc, cv)
		case "delete":
			c.cstore.Delete(r.Before, cv)
		}
	}
}

// columnSource returns reconstructed documents for a covered aggregation when the
// column store can serve it, and ok=false to fall back to the heap. It reconstructs
// only the covered fields each visible row carries, which is what makes the column
// path cheaper than a heap scan: non-projected fields are never touched.
func (t *Txn) columnSource(pipeline []bson.Raw) (docs []bson.Raw, ok bool) {
	c := t.c
	if c.cstore == nil {
		return nil, false
	}
	plan, ok := parseColumnEligible(pipeline)
	if !ok {
		return nil, false
	}
	if !c.cstore.Covers(plan.fields) {
		return nil, false
	}
	if !c.cstore.PreferOverHeap(t.startVer, plan.pred) {
		return nil, false
	}
	rows := c.cstore.Reconstruct(t.startVer, plan.fields, plan.pred)
	return rows, true
}

// colPlan is the covered shape of an eligible aggregation: the fields it reads and an
// optional range predicate the scan can push into the zone map.
type colPlan struct {
	fields []string
	pred   *colstore.RangePred
}

// parseColumnEligible recognizes the analytical shape the column store accelerates:
// an optional single-field range $match followed by a $group, reading only field
// paths (no expressions the column scan cannot reconstruct). It returns the set of
// referenced fields and the pushdown predicate. Anything more complex returns false
// and the heap path runs.
func parseColumnEligible(pipeline []bson.Raw) (colPlan, bool) {
	if len(pipeline) == 0 || len(pipeline) > 2 {
		return colPlan{}, false
	}
	var plan colPlan
	fields := map[string]bool{}
	addField := func(path string) bool {
		if path == "" || path == idFieldName {
			return false // _id is not projected into the group store
		}
		if !fields[path] {
			fields[path] = true
			plan.fields = append(plan.fields, path)
		}
		return true
	}

	idx := 0
	if name, body, ok := singleStage(pipeline[0]); ok && name == "$match" {
		pred, field, ok := parseRangeMatch(body)
		if !ok || !addField(field) {
			return colPlan{}, false
		}
		plan.pred = pred
		idx = 1
	}
	if idx >= len(pipeline) {
		return colPlan{}, false
	}
	name, body, ok := singleStage(pipeline[idx])
	if !ok || name != "$group" {
		return colPlan{}, false
	}
	if idx != len(pipeline)-1 {
		return colPlan{}, false
	}
	if !parseGroupFields(body, addField) {
		return colPlan{}, false
	}
	return plan, true
}

// singleStage returns the stage operator name and its body document for a one-key
// stage like {$group: {...}}.
func singleStage(stage bson.Raw) (string, bson.Raw, bool) {
	els, err := stage.Elements()
	if err != nil || len(els) != 1 {
		return "", nil, false
	}
	e := els[0]
	if e.Value.Type != bson.TypeDocument {
		return "", nil, false
	}
	return e.Key, e.Value.Document(), true
}

// parseRangeMatch recognizes {field: {$gt|$gte|$lt|$lte: bound}} or {field: scalar}
// over a single field and returns the pushdown predicate. Multi-field, $or, $expr,
// and operators the zone map cannot use are rejected so the heap path takes them.
func parseRangeMatch(body bson.Raw) (*colstore.RangePred, string, bool) {
	els, err := body.Elements()
	if err != nil || len(els) != 1 {
		return nil, "", false
	}
	e := els[0]
	if len(e.Key) == 0 || e.Key[0] == '$' {
		return nil, "", false
	}
	if e.Value.Type != bson.TypeDocument {
		// {field: scalar} is an equality.
		return &colstore.RangePred{Field: e.Key, Op: "$eq", Bound: colstore.FromRawValue(e.Value)}, e.Key, true
	}
	inner, err := e.Value.Document().Elements()
	if err != nil || len(inner) != 1 {
		return nil, "", false
	}
	op := inner[0].Key
	switch op {
	case "$gt", "$gte", "$lt", "$lte", "$eq":
		return &colstore.RangePred{Field: e.Key, Op: op, Bound: colstore.FromRawValue(inner[0].Value)}, e.Key, true
	default:
		return nil, "", false
	}
}

// parseGroupFields walks a $group body and registers every field path it reads: the
// _id grouping expression and each accumulator argument. It rejects a $group whose
// _id or accumulators reference anything other than a single field path or the
// constant 1, since the column scan can only reconstruct field values.
func parseGroupFields(body bson.Raw, addField func(string) bool) bool {
	els, err := body.Elements()
	if err != nil {
		return false
	}
	sawID := false
	for _, e := range els {
		if e.Key == idFieldName {
			sawID = true
			if !groupIDFields(e.Value, addField) {
				return false
			}
			continue
		}
		// Accumulator: {$sum|$avg|$min|$max: <arg>} or {$count: {}}.
		if e.Value.Type != bson.TypeDocument {
			return false
		}
		acc, err := e.Value.Document().Elements()
		if err != nil || len(acc) != 1 {
			return false
		}
		switch acc[0].Key {
		case "$sum", "$avg", "$min", "$max":
			if !accumArgField(acc[0].Value, addField) {
				return false
			}
		case "$count":
			// counts no field
		default:
			return false
		}
	}
	return sawID
}

// groupIDField handles the $group _id: a field path, null, or a constant groups the
// whole collection; a document or expression is rejected.
func groupIDFields(v bson.RawValue, addField func(string) bool) bool {
	switch v.Type {
	case bson.TypeNull, 0:
		return true
	case bson.TypeString:
		s := v.StringValue()
		if len(s) > 0 && s[0] == '$' {
			return addField(s[1:])
		}
		return true // constant string _id: whole-collection group
	case bson.TypeDocument:
		return false // composite _id not accelerated
	default:
		return true // constant scalar _id
	}
}

// accumArgField handles an accumulator argument: a "$field" path or a constant. A
// constant (the $sum:1 counting idiom) references no field.
func accumArgField(v bson.RawValue, addField func(string) bool) bool {
	switch v.Type {
	case bson.TypeString:
		s := v.StringValue()
		if len(s) > 0 && s[0] == '$' {
			return addField(s[1:])
		}
		return true
	case bson.TypeDocument, bson.TypeArray:
		return false // nested expression not accelerated
	default:
		return true // constant, e.g. $sum: 1
	}
}
