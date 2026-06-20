// Package plan is doc's query planner and execution engine: it turns a compiled
// filter, projection, and sort into a tree of pull-based plan stages, picks an
// access path (a collection scan or a secondary-index scan) by a simple cost
// estimate, and renders the MongoDB-shaped explain output for the chosen tree
// (spec 2061 doc 10, doc 11).
//
// The engine follows the Volcano iterator model: every stage answers GetNext by
// pulling from its child, so the tree streams documents without materializing
// intermediate results, except for the blocking stages (sort) that must consume
// their whole input first. Stage names mirror MongoDB (COLLSCAN, IXSCAN, FETCH,
// FILTER, SORT, SKIP, LIMIT, PROJECTION) so an explain plan compares directly
// against the reference server (spec 2061 doc 10 §16, doc 11 §1.3).
//
// The planner never owns the durable data: it reads through a Source the
// collection layer supplies, which resolves index RIDs and collection documents
// at the caller's MVCC snapshot. That keeps the engine independent of the storage
// and overlay details and lets a read see its own snapshot consistently.
package plan

import (
	"io"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/storage"
)

// EOF signals an exhausted stage, returned by GetNext when no more members are
// available. It is io.EOF so callers can compare with errors.Is.
var EOF = io.EOF

// WorkingSetMember is the unit that flows up the plan tree. An index scan emits a
// member with its RID and raw index Key and no document; a fetch fills in Doc; a
// covered projection builds Doc straight from Key without a fetch. A collection
// scan emits a member that already carries Doc.
type WorkingSetMember struct {
	RID storage.RID
	Key storage.IndexKey // the index field key (no trailing RID), for a covered plan
	Doc bson.Raw         // the document, once a fetch or covered projection produces it
}

// StageStats records the execution counters MongoDB reports per stage in
// executionStats verbosity (spec 2061 doc 10 §16.2). Works counts GetNext calls,
// Advanced counts members returned, and the examined counters report index keys
// and documents touched.
type StageStats struct {
	Stage        string
	Works        uint64
	Advanced     uint64
	KeysExamined uint64
	DocsExamined uint64
}

// PlanStage is one node of the execution tree. GetNext pulls the next member from
// the stage, returning EOF when the stage is exhausted. Stats exposes the live
// counters, and explainNode (unexported, via the helpers in explain.go) renders
// the stage into the plan tree.
type PlanStage interface {
	// GetNext returns the next working-set member or EOF. Any other error aborts
	// the query.
	GetNext() (*WorkingSetMember, error)
	// Stats returns the stage's live execution counters.
	Stats() *StageStats
	// Child returns the single input stage, or nil for a leaf (a scan).
	Child() PlanStage
	// explain renders the stage-specific plan fields into b, then recurses into the
	// child under "inputStage"; verbose adds the execution counters.
	explain(b *bson.Builder, verbose bool)
}
