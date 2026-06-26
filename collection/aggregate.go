package collection

import (
	"fmt"

	"github.com/tamnd/doc/agg"
	"github.com/tamnd/doc/bson"
)

// Aggregate runs an aggregation pipeline over the collection and returns the
// result documents. The pipeline is an array of single-key stage documents (spec
// 2061 doc 12). The source is the full collection in natural order; stage-level
// access-path pushdown is a later optimization.
//
// Cross-collection stages ($lookup, $graphLookup, $unionWith, $out, $merge)
// resolve through an environment backed by this transaction. M4 is one collection
// per file; the catalog that maps a foreign collection name to its own storage
// arrives in M6 (doc 09). Until then every collection name resolves to this
// collection, which is what makes a self-referencing $lookup or $graphLookup work.
func (t *Txn) Aggregate(pipeline []bson.Raw) ([]bson.Raw, error) {
	if t.done {
		return nil, ErrTxnDone
	}
	p, err := agg.Compile(pipeline)
	if err != nil {
		return nil, err
	}
	// A covered terminal $group runs through the column store's vectorized executor,
	// which folds the decoded columns straight into accumulators and returns the group
	// documents directly, byte-identical to the pipeline (spec 2061 doc 19 §6.3). The
	// pipeline is still compiled above so an unsupported $group is rejected the same
	// way before the fast path is even considered.
	if docs, ok := t.columnGroup(pipeline); ok {
		return docs, nil
	}
	// A covered analytical pipeline reads its fields from the columnar projection
	// store instead of the heap (spec 2061 doc 04 §10.5). The store reconstructs only
	// the covered fields, so the same compiled pipeline produces an identical result.
	docs, ok := t.columnSource(pipeline)
	if !ok {
		if docs, err = t.Find(matchAll()); err != nil {
			return nil, err
		}
	}
	return p.RunWith(docs, t.c.clk.Now().UnixMilli(), t.aggEnv())
}

// Aggregate runs an aggregation pipeline in its own read-only transaction. A
// pipeline that ends in $out or $merge needs a writable transaction; open one with
// Begin and call Txn.Aggregate directly.
func (c *Collection) Aggregate(pipeline []bson.Raw) ([]bson.Raw, error) {
	t := c.BeginReadOnly()
	defer func() { _ = t.Rollback() }()
	return t.Aggregate(pipeline)
}

// aggEnv builds the environment the aggregation engine uses to reach other
// collections. Read serves the current collection for any name (one collection per
// file at M4); Write applies a $out or $merge result back through this transaction.
func (t *Txn) aggEnv() *agg.Env {
	return &agg.Env{
		Read: func(string) ([]bson.Raw, error) {
			return t.Find(matchAll())
		},
		Write: t.aggWrite,
	}
}

// aggWrite applies a pipeline write request: $out replaces the whole collection,
// $merge upserts each output document by its on-fields (spec 2061 doc 12 §15, §16).
func (t *Txn) aggWrite(req agg.WriteRequest) error {
	if req.Replace {
		return t.aggOut(req.Docs)
	}
	return t.aggMerge(req)
}

// aggOut implements $out: discard every existing document and replace the
// collection with the pipeline output. The output is already materialized in
// memory before this runs, so dropping the source is safe.
func (t *Txn) aggOut(docs []bson.Raw) error {
	if _, err := t.DeleteMany(matchAll()); err != nil {
		return err
	}
	_, err := t.InsertMany(docs, true)
	return err
}

// aggMerge implements $merge: for each output document, match an existing target by
// the on-fields and apply whenMatched, or apply whenNotMatched when none matches.
func (t *Txn) aggMerge(req agg.WriteRequest) error {
	on := req.On
	if len(on) == 0 {
		on = []string{"_id"}
	}
	for _, d := range req.Docs {
		filter, err := mergeFilter(d, on)
		if err != nil {
			return err
		}
		existing, err := t.FindOne(filter)
		if err != nil {
			return err
		}
		if existing == nil {
			if err := t.mergeUnmatched(d, req.WhenNotMatched); err != nil {
				return err
			}
			continue
		}
		if err := t.mergeMatched(filter, existing, d, req.WhenMatched); err != nil {
			return err
		}
	}
	return nil
}

// mergeUnmatched applies the whenNotMatched action for a $merge document with no
// existing target.
func (t *Txn) mergeUnmatched(d bson.Raw, when string) error {
	switch when {
	case "", "insert":
		_, err := t.InsertOne(d)
		return err
	case "discard":
		return nil
	case "fail":
		return fmt.Errorf("collection: $merge found no matching document and whenNotMatched is fail")
	default:
		return fmt.Errorf("collection: $merge unknown whenNotMatched %q", when)
	}
}

// mergeMatched applies the whenMatched action for a $merge document that found an
// existing target.
func (t *Txn) mergeMatched(filter, existing, d bson.Raw, when string) error {
	switch when {
	case "", "merge":
		merged, err := mergeDocs(existing, d)
		if err != nil {
			return err
		}
		_, err = t.ReplaceOne(filter, merged)
		return err
	case "replace":
		_, err := t.ReplaceOne(filter, d)
		return err
	case "keepExisting":
		return nil
	case "fail":
		return fmt.Errorf("collection: $merge matched an existing document and whenMatched is fail")
	default:
		return fmt.Errorf("collection: $merge unknown whenMatched %q", when)
	}
}

// matchAll is the match-all filter.
func matchAll() bson.Raw { return bson.NewBuilder().Build() }

// mergeFilter builds an equality filter that selects the target document for a
// $merge output document by its on-fields. A document missing an on-field is an
// error, matching MongoDB.
func mergeFilter(d bson.Raw, on []string) (bson.Raw, error) {
	b := bson.NewBuilder()
	for _, field := range on {
		v, ok := d.Lookup(field)
		if !ok {
			return nil, fmt.Errorf("collection: $merge document is missing on-field %q", field)
		}
		b.AppendValue(field, v)
	}
	return b.Build(), nil
}

// mergeDocs overlays the fields of the pipeline document over the existing target,
// keeping fields the new document does not mention, overriding the rest in their
// existing position, and appending genuinely new fields at the end.
func mergeDocs(existing, incoming bson.Raw) (bson.Raw, error) {
	exElems, err := existing.Elements()
	if err != nil {
		return nil, err
	}
	inElems, err := incoming.Elements()
	if err != nil {
		return nil, err
	}
	override := make(map[string]bson.RawValue, len(inElems))
	seen := make(map[string]bool, len(inElems))
	order := make([]string, 0, len(inElems))
	for _, e := range inElems {
		if !seen[e.Key] {
			seen[e.Key] = true
			order = append(order, e.Key)
		}
		override[e.Key] = e.Value
	}
	b := bson.NewBuilder()
	emitted := make(map[string]bool, len(exElems))
	for _, e := range exElems {
		if v, ok := override[e.Key]; ok {
			b.AppendValue(e.Key, v)
		} else {
			b.AppendValue(e.Key, e.Value)
		}
		emitted[e.Key] = true
	}
	for _, k := range order {
		if !emitted[k] {
			b.AppendValue(k, override[k])
		}
	}
	return b.Build(), nil
}
