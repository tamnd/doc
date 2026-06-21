package doc

import (
	"context"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/options"
)

// Explain reports how the query planner would run a find: the chosen plan, the
// index it picks (if any), and the bounds it scans (spec 2061 doc 11 §9). The
// verbosity argument follows MongoDB's: "queryPlanner" describes the plan without
// running it, "executionStats" runs it and reports counts. An empty verbosity
// defaults to "queryPlanner". The result is the same plan document a driver gets
// from the explain command, so the shell can print it verbatim.
func (c *Collection) Explain(ctx context.Context, filter any, verbosity string, opts ...*options.FindOptions) (bson.Raw, error) {
	if err := c.db.check(ctx); err != nil {
		return nil, err
	}
	f, err := toFilter(filter)
	if err != nil {
		return nil, err
	}
	col := c.readable()
	if col == nil {
		// No namespace yet: there is nothing to scan, so report an empty-collection
		// plan rather than an error, matching how Find treats a missing namespace.
		col, err = c.writable()
		if err != nil {
			return nil, err
		}
	}
	fo, err := c.findOptionsToEngine(opts)
	if err != nil {
		return nil, err
	}
	plan, err := col.Explain(f, fo, verbosity)
	if err != nil {
		return nil, mapEngineErr(err)
	}
	return plan, nil
}
