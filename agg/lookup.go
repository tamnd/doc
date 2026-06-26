package agg

import (
	"errors"
	"io"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/query"
)

// ErrNoEnv reports a cross-collection stage used without an environment. The
// collection package supplies the environment through Pipeline.RunWith.
var ErrNoEnv = errors.New("agg: cross-collection stage requires an environment")

// ---- $lookup -------------------------------------------------------------

// letVar binds a sub-pipeline variable name to an expression evaluated against the
// outer (local) document.
type letVar struct {
	name string
	expr Expr
}

// compileLookup compiles the equality, sub-pipeline, and combined forms of $lookup
// (spec 2061 doc 12 §11).
func compileLookup(arg bson.RawValue) (stageSpec, error) {
	if arg.Type != bson.TypeDocument {
		return nil, ErrBadStage
	}
	d := arg.Document()
	fromv, ok := d.Lookup("from")
	if !ok {
		return nil, ErrBadStage
	}
	from, ok := strOf(fromv)
	if !ok {
		return nil, ErrBadStage
	}
	asv, ok := d.Lookup("as")
	if !ok {
		return nil, ErrBadStage
	}
	as, ok := strOf(asv)
	if !ok {
		return nil, ErrBadStage
	}
	st := &lookupStage{from: from, as: splitPath(as)}
	if lv, ok := d.Lookup("localField"); ok {
		s, sok := strOf(lv)
		if !sok {
			return nil, ErrBadStage
		}
		fv, fok := d.Lookup("foreignField")
		if !fok {
			return nil, ErrBadStage
		}
		fs, fsok := strOf(fv)
		if !fsok {
			return nil, ErrBadStage
		}
		st.localField = splitPath(s)
		st.foreignField = splitPath(fs)
		st.hasEquality = true
	}
	if lv, ok := d.Lookup("let"); ok {
		lets, err := compileLet2(lv)
		if err != nil {
			return nil, err
		}
		st.lets = lets
	}
	if pv, ok := d.Lookup("pipeline"); ok {
		stages, err := arrayDocs(pv)
		if err != nil {
			return nil, err
		}
		sub, cerr := Compile(stages)
		if cerr != nil {
			return nil, cerr
		}
		st.pipeline = sub
		st.hasPipeline = true
	}
	if !st.hasEquality && !st.hasPipeline {
		return nil, ErrBadStage
	}
	return st, nil
}

// compileLet2 compiles a let document into ordered variable bindings.
func compileLet2(v bson.RawValue) ([]letVar, error) {
	if v.Type != bson.TypeDocument {
		return nil, ErrBadStage
	}
	elems, err := v.Document().Elements()
	if err != nil {
		return nil, err
	}
	out := make([]letVar, 0, len(elems))
	for _, e := range elems {
		ex, cerr := compileExpr(e.Value)
		if cerr != nil {
			return nil, cerr
		}
		out = append(out, letVar{name: e.Key, expr: ex})
	}
	return out, nil
}

// arrayDocs reads an array of documents (a sub-pipeline) into a slice.
func arrayDocs(v bson.RawValue) ([]bson.Raw, error) {
	if v.Type != bson.TypeArray {
		return nil, ErrBadStage
	}
	elems, err := arrayElements(v)
	if err != nil {
		return nil, err
	}
	out := make([]bson.Raw, 0, len(elems))
	for _, e := range elems {
		if e.Type != bson.TypeDocument {
			return nil, ErrBadStage
		}
		out = append(out, e.Document())
	}
	return out, nil
}

type lookupStage struct {
	from         string
	as           []string
	localField   []string
	foreignField []string
	hasEquality  bool
	lets         []letVar
	pipeline     *Pipeline
	hasPipeline  bool
}

func (s *lookupStage) open(in src, ec *execCtx) src {
	return &lookupSrc{in: in, stage: s, ec: ec}
}

type lookupSrc struct {
	in      src
	stage   *lookupStage
	ec      *execCtx
	foreign []bson.Raw
	loaded  bool
	// fidx maps a foreign join-key to the indices of the foreign documents that
	// carry it, built once for the equality form so the join is O(local + foreign)
	// rather than a scan per input document (spec 2061 doc 12 §11.4, hash join).
	fidx map[string][]int
	// cached holds an uncorrelated sub-pipeline result, computed once.
	cached    []bson.RawValue
	cachedSet bool
}

func (s *lookupSrc) next() (bson.Raw, error) {
	if !s.loaded {
		if s.ec.env == nil || s.ec.env.Read == nil {
			return nil, ErrNoEnv
		}
		fdocs, err := s.ec.env.Read(s.stage.from)
		if err != nil {
			return nil, err
		}
		s.foreign = fdocs
		if s.stage.hasEquality {
			s.buildForeignIndex()
		}
		s.loaded = true
	}
	doc, err := s.in.next()
	if err != nil {
		return nil, err
	}
	matches, err := s.matchesFor(doc)
	if err != nil {
		return nil, err
	}
	return docWith(doc, s.stage.as, mkArray(matches)), nil
}

// matchesFor computes the foreign documents joined to one input document.
func (s *lookupSrc) matchesFor(doc bson.Raw) ([]bson.RawValue, error) {
	cand := s.foreign
	if s.stage.hasEquality {
		cand = s.equalityMatches(doc)
	}
	if !s.stage.hasPipeline {
		return rawsToValues(cand), nil
	}
	return s.pipelineMatches(doc, cand)
}

// buildForeignIndex hashes each foreign document by the value(s) of its
// foreignField. An array-valued field (multikey) is indexed under each element, so
// the probe stays array-aware. The key is the canonical group key, which collapses
// the numeric types the same way join equality does (spec 2061 doc 12 §11.4).
func (s *lookupSrc) buildForeignIndex() {
	s.fidx = make(map[string][]int, len(s.foreign))
	for i, f := range s.foreign {
		fv := resolvePath(mkDoc(f), s.stage.foreignField)
		for _, c := range joinCandidates(fv) {
			k := groupKey(c)
			idx := s.fidx[k]
			// Skip a duplicate index when the same multikey value repeats, so a
			// foreign document is never joined to one input more than once.
			if n := len(idx); n > 0 && idx[n-1] == i {
				continue
			}
			s.fidx[k] = append(idx, i)
		}
	}
}

// equalityMatches returns foreign documents whose foreignField equals the input
// document's localField, probing the prebuilt hash index. It collects matched
// indices into a set and then emits the foreign documents in their original order
// with no duplicates, matching the array- and null-aware semantics of the scan it
// replaces.
func (s *lookupSrc) equalityMatches(doc bson.Raw) []bson.Raw {
	local := resolvePath(mkDoc(doc), s.stage.localField)
	matched := map[int]struct{}{}
	for _, w := range joinCandidates(local) {
		for _, idx := range s.fidx[groupKey(w)] {
			matched[idx] = struct{}{}
		}
	}
	if len(matched) == 0 {
		return nil
	}
	out := make([]bson.Raw, 0, len(matched))
	for i, f := range s.foreign {
		if _, ok := matched[i]; ok {
			out = append(out, f)
		}
	}
	return out
}

// pipelineMatches runs the sub-pipeline over the candidate foreign documents with
// the let-variables bound to the input document, caching the uncorrelated case.
func (s *lookupSrc) pipelineMatches(doc bson.Raw, cand []bson.Raw) ([]bson.RawValue, error) {
	if len(s.stage.lets) == 0 && !s.stage.hasEquality {
		if s.cachedSet {
			return s.cached, nil
		}
		res, err := s.stage.pipeline.runInner(cand, s.ec.withVars(nil))
		if err != nil {
			return nil, err
		}
		s.cached, s.cachedSet = rawsToValues(res), true
		return s.cached, nil
	}
	vars := map[string]bson.RawValue{}
	ctx := docCtx(doc, s.ec)
	for _, lv := range s.stage.lets {
		vars[lv.name] = lv.expr.eval(ctx)
	}
	res, err := s.stage.pipeline.runInner(cand, s.ec.withVars(vars))
	if err != nil {
		return nil, err
	}
	return rawsToValues(res), nil
}

// joinCandidates expands a local join value into the set of values to match: the
// elements of an array, or the value itself, with missing normalized to null.
func joinCandidates(v bson.RawValue) []bson.RawValue {
	if v.Type == bson.TypeArray {
		els, err := arrayElements(v)
		if err == nil {
			return els
		}
	}
	return []bson.RawValue{cmpVal(v)}
}

// joinOverlaps reports whether a foreign value matches any candidate, treating an
// array foreign value as the set of its elements.
func joinOverlaps(want []bson.RawValue, fv bson.RawValue) bool {
	fvals := joinCandidates(fv)
	for _, w := range want {
		for _, f := range fvals {
			if bson.Equal(w, f) {
				return true
			}
		}
	}
	return false
}

// rawsToValues wraps documents as RawValues for array construction.
func rawsToValues(docs []bson.Raw) []bson.RawValue {
	out := make([]bson.RawValue, len(docs))
	for i, d := range docs {
		out[i] = mkDoc(d)
	}
	return out
}

// ---- $unionWith ----------------------------------------------------------

// compileUnionWith compiles the string and {coll, pipeline} forms (spec 2061 doc
// 12 §15).
func compileUnionWith(arg bson.RawValue) (stageSpec, error) {
	st := &unionWithStage{}
	switch arg.Type {
	case bson.TypeString:
		st.coll = arg.StringValue()
	case bson.TypeDocument:
		d := arg.Document()
		cv, ok := d.Lookup("coll")
		if !ok {
			return nil, ErrBadStage
		}
		c, cok := strOf(cv)
		if !cok {
			return nil, ErrBadStage
		}
		st.coll = c
		if pv, ok := d.Lookup("pipeline"); ok {
			stages, err := arrayDocs(pv)
			if err != nil {
				return nil, err
			}
			sub, cerr := Compile(stages)
			if cerr != nil {
				return nil, cerr
			}
			st.pipeline = sub
		}
	default:
		return nil, ErrBadStage
	}
	return st, nil
}

type unionWithStage struct {
	coll     string
	pipeline *Pipeline
}

func (s *unionWithStage) open(in src, ec *execCtx) src {
	return &unionWithSrc{in: in, stage: s, ec: ec}
}

type unionWithSrc struct {
	in        src
	stage     *unionWithStage
	ec        *execCtx
	drainedIn bool
	foreign   []bson.Raw
	fi        int
}

func (s *unionWithSrc) next() (bson.Raw, error) {
	if !s.drainedIn {
		doc, err := s.in.next()
		if err == io.EOF {
			if lerr := s.loadForeign(); lerr != nil {
				return nil, lerr
			}
			s.drainedIn = true
		} else {
			return doc, err
		}
	}
	if s.fi >= len(s.foreign) {
		return nil, io.EOF
	}
	d := s.foreign[s.fi]
	s.fi++
	return d, nil
}

// loadForeign reads the foreign collection, applying the sub-pipeline if present.
func (s *unionWithSrc) loadForeign() error {
	if s.ec.env == nil || s.ec.env.Read == nil {
		return ErrNoEnv
	}
	fdocs, err := s.ec.env.Read(s.stage.coll)
	if err != nil {
		return err
	}
	if s.stage.pipeline != nil {
		fdocs, err = s.stage.pipeline.runInner(fdocs, s.ec.withVars(nil))
		if err != nil {
			return err
		}
	}
	s.foreign = fdocs
	return nil
}

// ---- $graphLookup --------------------------------------------------------

// compileGraphLookup compiles a recursive graph traversal (spec 2061 doc 12 §12).
func compileGraphLookup(arg bson.RawValue) (stageSpec, error) {
	if arg.Type != bson.TypeDocument {
		return nil, ErrBadStage
	}
	d := arg.Document()
	st := &graphLookupStage{maxDepth: -1}
	fromv, ok := d.Lookup("from")
	if !ok {
		return nil, ErrBadStage
	}
	if st.from, ok = strOf(fromv); !ok {
		return nil, ErrBadStage
	}
	sw, ok := d.Lookup("startWith")
	if !ok {
		return nil, ErrBadStage
	}
	start, err := compileExpr(sw)
	if err != nil {
		return nil, err
	}
	st.startWith = start
	cf, ok := d.Lookup("connectFromField")
	if !ok {
		return nil, ErrBadStage
	}
	cfs, ok := strOf(cf)
	if !ok {
		return nil, ErrBadStage
	}
	st.connectFrom = splitPath(cfs)
	ct, ok := d.Lookup("connectToField")
	if !ok {
		return nil, ErrBadStage
	}
	cts, ok := strOf(ct)
	if !ok {
		return nil, ErrBadStage
	}
	st.connectTo = splitPath(cts)
	asv, ok := d.Lookup("as")
	if !ok {
		return nil, ErrBadStage
	}
	as, ok := strOf(asv)
	if !ok {
		return nil, ErrBadStage
	}
	st.as = splitPath(as)
	if mv, ok := d.Lookup("maxDepth"); ok {
		n, nok := intArg(mv)
		if !nok || n < 0 {
			return nil, ErrBadStage
		}
		st.maxDepth = n
	}
	if dv, ok := d.Lookup("depthField"); ok {
		s, sok := strOf(dv)
		if !sok {
			return nil, ErrBadStage
		}
		st.depthField = splitPath(s)
	}
	if rv, ok := d.Lookup("restrictSearchWithMatch"); ok {
		if rv.Type != bson.TypeDocument {
			return nil, ErrBadStage
		}
		m, merr := query.Compile(rv.Document())
		if merr != nil {
			return nil, merr
		}
		st.restrict = m
	}
	return st, nil
}

type graphLookupStage struct {
	from        string
	startWith   Expr
	connectFrom []string
	connectTo   []string
	as          []string
	maxDepth    int
	depthField  []string
	restrict    *query.Matcher
}

func (s *graphLookupStage) open(in src, ec *execCtx) src {
	return &graphLookupSrc{in: in, stage: s, ec: ec}
}

type graphLookupSrc struct {
	in      src
	stage   *graphLookupStage
	ec      *execCtx
	foreign []bson.Raw
	loaded  bool
}

func (s *graphLookupSrc) next() (bson.Raw, error) {
	if !s.loaded {
		if s.ec.env == nil || s.ec.env.Read == nil {
			return nil, ErrNoEnv
		}
		fdocs, err := s.ec.env.Read(s.stage.from)
		if err != nil {
			return nil, err
		}
		s.foreign = fdocs
		s.loaded = true
	}
	doc, err := s.in.next()
	if err != nil {
		return nil, err
	}
	result := s.traverse(doc)
	return docWith(doc, s.stage.as, mkArray(result)), nil
}

// traverse runs a breadth-first search over the foreign collection from the
// startWith seed values, following connectFrom→connectTo edges and tracking
// visited documents to break cycles (spec 2061 doc 12 §12.3).
func (s *graphLookupSrc) traverse(doc bson.Raw) []bson.RawValue {
	frontier := joinCandidates(s.stage.startWith.eval(docCtx(doc, s.ec)))
	visited := map[string]bool{}
	var result []bson.RawValue
	for depth := 0; len(frontier) > 0; depth++ {
		if s.stage.maxDepth >= 0 && depth > s.stage.maxDepth {
			break
		}
		var nextVals []bson.RawValue
		for _, val := range frontier {
			for _, f := range s.foreign {
				fv := resolvePath(mkDoc(f), s.stage.connectTo)
				if !joinOverlaps([]bson.RawValue{cmpVal(val)}, fv) {
					continue
				}
				if s.stage.restrict != nil && !s.stage.restrict.Match(f) {
					continue
				}
				id := docIdentity(f)
				if visited[id] {
					continue
				}
				visited[id] = true
				out := f
				if s.stage.depthField != nil {
					out = docWith(out, s.stage.depthField, mkInt64(int64(depth)))
				}
				result = append(result, mkDoc(out))
				nextVals = append(nextVals, joinCandidates(resolvePath(mkDoc(f), s.stage.connectFrom))...)
			}
		}
		frontier = nextVals
	}
	return result
}

// docIdentity returns a stable identity for a foreign document for cycle
// detection, preferring _id and falling back to the whole document key.
func docIdentity(d bson.Raw) string {
	if v, ok := d.Lookup("_id"); ok {
		return "I" + groupKey(v)
	}
	return "D" + groupKey(mkDoc(d))
}
