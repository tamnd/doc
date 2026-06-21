package agg

import (
	"strings"

	"github.com/tamnd/doc/bson"
)

// optimizeRaw rewrites the logical pipeline (the array of stage documents) into an
// equivalent, cheaper pipeline before compilation (spec 2061 doc 12 §18). It
// applies the rewrite rules to a fixpoint so a rule can expose work for another.
func optimizeRaw(stages []bson.Raw) []bson.Raw {
	for {
		next, changed := optimizePass(stages)
		stages = next
		if !changed {
			return stages
		}
	}
}

// optimizePass applies each rewrite rule once across the pipeline.
func optimizePass(stages []bson.Raw) ([]bson.Raw, bool) {
	rules := []func([]bson.Raw) ([]bson.Raw, bool){
		coalesceLimits,
		coalesceSorts,
		mergeReshapes,
		pushMatchBeforeReshape,
	}
	changed := false
	for _, rule := range rules {
		next, c := rule(stages)
		if c {
			changed = true
			stages = next
		}
	}
	return stages, changed
}

// stageName returns the single stage key, or "" for a malformed stage.
func stageName(s bson.Raw) string {
	elems, err := s.Elements()
	if err != nil || len(elems) != 1 {
		return ""
	}
	return elems[0].Key
}

// stageArg returns a stage's argument value.
func stageArg(s bson.Raw) bson.RawValue {
	elems, err := s.Elements()
	if err != nil || len(elems) != 1 {
		return bson.RawValue{}
	}
	return elems[0].Value
}

// coalesceLimits merges two consecutive $limit stages into the smaller bound
// (spec 2061 doc 12 §18.5).
func coalesceLimits(stages []bson.Raw) ([]bson.Raw, bool) {
	for i := 0; i+1 < len(stages); i++ {
		if stageName(stages[i]) == "$limit" && stageName(stages[i+1]) == "$limit" {
			a, ok1 := intArg(stageArg(stages[i]))
			b, ok2 := intArg(stageArg(stages[i+1]))
			if !ok1 || !ok2 {
				continue
			}
			n := a
			if b < n {
				n = b
			}
			merged := bson.NewBuilder().AppendInt64("$limit", int64(n)).Build()
			return spliceStages(stages, i, 2, merged), true
		}
	}
	return stages, false
}

// coalesceSorts drops the earlier of two consecutive $sort stages: the later sort
// determines the final order (spec 2061 doc 12 §18.11).
func coalesceSorts(stages []bson.Raw) ([]bson.Raw, bool) {
	for i := 0; i+1 < len(stages); i++ {
		if stageName(stages[i]) == "$sort" && stageName(stages[i+1]) == "$sort" {
			return spliceStages(stages, i, 1), true
		}
	}
	return stages, false
}

// mergeReshapes merges two consecutive $addFields (or $set) stages into one,
// composing their field specs (spec 2061 doc 12 §18.9).
func mergeReshapes(stages []bson.Raw) ([]bson.Raw, bool) {
	for i := 0; i+1 < len(stages); i++ {
		a, b := stageName(stages[i]), stageName(stages[i+1])
		if !isAddFields(a) || !isAddFields(b) {
			continue
		}
		av, bv := stageArg(stages[i]), stageArg(stages[i+1])
		if av.Type != bson.TypeDocument || bv.Type != bson.TypeDocument {
			continue
		}
		body := bson.NewBuilder()
		appendAll(body, av.Document())
		appendAll(body, bv.Document())
		merged := bson.NewBuilder().AppendDocument(a, body.Build()).Build()
		return spliceStages(stages, i, 2, merged), true
	}
	return stages, false
}

func isAddFields(name string) bool { return name == "$addFields" || name == "$set" }

// appendAll copies every element of d into b.
func appendAll(b *bson.Builder, d bson.Raw) {
	elems, err := d.Elements()
	if err != nil {
		return
	}
	for _, e := range elems {
		b.AppendValue(e.Key, e.Value)
	}
}

// pushMatchBeforeReshape moves a $match before a preceding $addFields or $set when
// the predicate references no field the reshaping stage introduces, so the match
// can later reach the cursor and an index (spec 2061 doc 12 §18.2).
func pushMatchBeforeReshape(stages []bson.Raw) ([]bson.Raw, bool) {
	for i := 1; i < len(stages); i++ {
		if stageName(stages[i]) != "$match" {
			continue
		}
		prev := stages[i-1]
		if !isAddFields(stageName(prev)) {
			continue
		}
		added := reshapeRoots(stageArg(prev))
		fields, ok := matchFieldRoots(stageArg(stages[i]))
		if !ok || intersects(added, fields) {
			continue
		}
		swapped := make([]bson.Raw, len(stages))
		copy(swapped, stages)
		swapped[i-1], swapped[i] = stages[i], stages[i-1]
		return swapped, true
	}
	return stages, false
}

// reshapeRoots returns the set of top-level field roots a reshape stage writes.
func reshapeRoots(arg bson.RawValue) map[string]bool {
	roots := map[string]bool{}
	if arg.Type != bson.TypeDocument {
		return roots
	}
	elems, err := arg.Document().Elements()
	if err != nil {
		return roots
	}
	for _, e := range elems {
		roots[fieldRoot(e.Key)] = true
	}
	return roots
}

// matchFieldRoots returns the field roots a match predicate reads. The bool is
// false when the predicate cannot be analyzed safely (it uses $expr or an
// unrecognized top-level operator), in which case the match must not move.
func matchFieldRoots(arg bson.RawValue) (map[string]bool, bool) {
	roots := map[string]bool{}
	if arg.Type != bson.TypeDocument {
		return nil, false
	}
	elems, err := arg.Document().Elements()
	if err != nil {
		return nil, false
	}
	for _, e := range elems {
		if e.Key == "$and" || e.Key == "$or" || e.Key == "$nor" {
			sub, ok := matchListRoots(e.Value)
			if !ok {
				return nil, false
			}
			for r := range sub {
				roots[r] = true
			}
			continue
		}
		if strings.HasPrefix(e.Key, "$") {
			return nil, false
		}
		roots[fieldRoot(e.Key)] = true
	}
	return roots, true
}

// matchListRoots gathers roots from a $and/$or/$nor array of sub-predicates.
func matchListRoots(v bson.RawValue) (map[string]bool, bool) {
	if v.Type != bson.TypeArray {
		return nil, false
	}
	elems, err := arrayElements(v)
	if err != nil {
		return nil, false
	}
	roots := map[string]bool{}
	for _, e := range elems {
		sub, ok := matchFieldRoots(e)
		if !ok {
			return nil, false
		}
		for r := range sub {
			roots[r] = true
		}
	}
	return roots, true
}

// fieldRoot returns the first dotted segment of a field path.
func fieldRoot(key string) string {
	if i := strings.IndexByte(key, '.'); i >= 0 {
		return key[:i]
	}
	return key
}

func intersects(a, b map[string]bool) bool {
	for k := range a {
		if b[k] {
			return true
		}
	}
	return false
}

// spliceStages replaces count stages at index i with the given replacements.
func spliceStages(stages []bson.Raw, i, count int, repl ...bson.Raw) []bson.Raw {
	out := make([]bson.Raw, 0, len(stages)-count+len(repl))
	out = append(out, stages[:i]...)
	out = append(out, repl...)
	out = append(out, stages[i+count:]...)
	return out
}

// fuseTopK fuses a $sort immediately followed by a $limit into a bounded top-K
// sort, setting the sort's limit and dropping the now-redundant $limit stage
// (spec 2061 doc 12 §18.4).
func fuseTopK(p *Pipeline) {
	out := make([]stageSpec, 0, len(p.stages))
	for i := 0; i < len(p.stages); i++ {
		if ss, ok := p.stages[i].(*sortStage); ok && i+1 < len(p.stages) {
			if ls, ok := p.stages[i+1].(*limitStage); ok {
				ss.limit = ls.n
				out = append(out, ss)
				i++
				continue
			}
		}
		out = append(out, p.stages[i])
	}
	p.stages = out
}
