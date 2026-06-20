package plan

import "github.com/tamnd/doc/bson"

// Explain renders the chosen plan as MongoDB's explain document at the given
// verbosity (spec 2061 doc 10 §16). At "queryPlanner" it returns just the winning
// plan tree; at "executionStats" or "allPlansExecution" it runs the plan and adds
// the per-stage execution counters and the query-wide totals.
func (p *Plan) Explain(verbosity string) (bson.Raw, error) {
	root, err := p.buildTree()
	if err != nil {
		return nil, err
	}
	verbose := verbosity == "executionStats" || verbosity == "allPlansExecution"

	var nReturned int64
	if verbose {
		for {
			_, gerr := root.GetNext()
			if gerr == EOF {
				break
			}
			if gerr != nil {
				return nil, gerr
			}
			nReturned++
		}
	}

	qp := bson.NewBuilder()
	qp.AppendDocument("winningPlan", planNode(root, false))

	out := bson.NewBuilder()
	out.AppendDocument("queryPlanner", qp.Build())
	if verbose {
		keys, docs := totals(root)
		es := bson.NewBuilder()
		es.AppendInt64("nReturned", nReturned)
		es.AppendInt64("totalKeysExamined", int64(keys))
		es.AppendInt64("totalDocsExamined", int64(docs))
		es.AppendDocument("executionStages", planNode(root, true))
		out.AppendDocument("executionStats", es.Build())
	}
	return out.Build(), nil
}

// planNode renders one stage into a plan-tree document: the stage name, its
// stage-specific fields, the execution counters when verbose, and the child under
// "inputStage". A covered fetch is transparent: it renders as its child, so the
// tree shows a projection directly over the index scan with no FETCH stage.
func planNode(s PlanStage, verbose bool) bson.Raw {
	if cf, ok := s.(*coveredFetchStage); ok {
		return planNode(cf.Child(), verbose)
	}
	b := bson.NewBuilder()
	st := s.Stats()
	b.AppendString("stage", st.Stage)
	s.explain(b, verbose)
	if verbose {
		b.AppendInt64("nReturned", int64(st.Advanced))
		b.AppendInt64("works", int64(st.Works))
		b.AppendInt64("keysExamined", int64(st.KeysExamined))
		b.AppendInt64("docsExamined", int64(st.DocsExamined))
	}
	if child := s.Child(); child != nil {
		b.AppendDocument("inputStage", planNode(child, verbose))
	}
	return b.Build()
}

// totals sums the index keys and documents examined across the whole stage tree,
// for the executionStats query-wide counters.
func totals(s PlanStage) (keys, docs uint64) {
	if s == nil {
		return 0, 0
	}
	st := s.Stats()
	keys, docs = st.KeysExamined, st.DocsExamined
	if child := s.Child(); child != nil {
		k, d := totals(child)
		keys += k
		docs += d
	}
	return keys, docs
}

// Summary returns a short label for the winning access path, "COLLSCAN" or the
// index name, for logs and quick assertions.
func (p *Plan) Summary() string {
	if p.cand == nil {
		return "COLLSCAN"
	}
	return "IXSCAN " + p.cand.desc.Name
}

// UsesIndex reports whether the plan chose an index access path.
func (p *Plan) UsesIndex() bool { return p.cand != nil }
