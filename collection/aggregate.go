package collection

import (
	"github.com/tamnd/doc/agg"
	"github.com/tamnd/doc/bson"
)

// Aggregate runs an aggregation pipeline over the collection and returns the
// result documents. The pipeline is an array of single-key stage documents (spec
// 2061 doc 12). The source is the full collection in natural order; stage-level
// access-path pushdown is a later optimization.
func (t *Txn) Aggregate(pipeline []bson.Raw) ([]bson.Raw, error) {
	if t.done {
		return nil, ErrTxnDone
	}
	p, err := agg.Compile(pipeline)
	if err != nil {
		return nil, err
	}
	docs, err := t.Find(bson.NewBuilder().Build())
	if err != nil {
		return nil, err
	}
	return p.Run(docs, t.c.clk.Now().UnixMilli())
}

// Aggregate runs an aggregation pipeline in its own read-only transaction.
func (c *Collection) Aggregate(pipeline []bson.Raw) ([]bson.Raw, error) {
	t := c.BeginReadOnly()
	defer func() { _ = t.Rollback() }()
	return t.Aggregate(pipeline)
}
