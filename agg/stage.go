package agg

import (
	"errors"
	"io"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/query"
)

// ErrBadStage reports a malformed pipeline stage: an unknown stage name, a stage
// argument of the wrong shape, or an empty pipeline element.
var ErrBadStage = errors.New("agg: malformed pipeline stage")

// src is a pull-based document stream. next returns io.EOF when the stream is
// exhausted. Every stage both consumes and implements src, so a pipeline is a
// chain of sources (spec 2061 doc 12 §2).
type src interface {
	next() (bson.Raw, error)
}

// stageSpec is one compiled pipeline stage. open wraps an upstream source into the
// stage's own source, binding the runtime now for $$NOW.
type stageSpec interface {
	open(in src, now int64) src
}

// Pipeline is a compiled aggregation pipeline.
type Pipeline struct {
	stages []stageSpec
}

// Compile compiles a pipeline (an array of single-key stage documents) into a
// Pipeline (spec 2061 doc 12 §2).
func Compile(stages []bson.Raw) (*Pipeline, error) {
	p := &Pipeline{}
	for _, s := range stages {
		elems, err := s.Elements()
		if err != nil {
			return nil, err
		}
		if len(elems) != 1 {
			return nil, ErrBadStage
		}
		spec, cerr := compileStage(elems[0].Key, elems[0].Value)
		if cerr != nil {
			return nil, cerr
		}
		p.stages = append(p.stages, spec)
	}
	return p, nil
}

// Run executes the pipeline over input documents, returning the result documents.
// now is the epoch-millisecond value bound to $$NOW.
func (p *Pipeline) Run(input []bson.Raw, now int64) ([]bson.Raw, error) {
	var s src = &sliceSrc{docs: input}
	for _, st := range p.stages {
		s = st.open(s, now)
	}
	var out []bson.Raw
	for {
		doc, err := s.next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, doc)
	}
}

// sliceSrc serves documents from a slice.
type sliceSrc struct {
	docs []bson.Raw
	i    int
}

func (s *sliceSrc) next() (bson.Raw, error) {
	if s.i >= len(s.docs) {
		return nil, io.EOF
	}
	d := s.docs[s.i]
	s.i++
	return d, nil
}

// docCtx builds the evaluation context for a document: $$ROOT and $$CURRENT both
// start at the whole document.
func docCtx(doc bson.Raw, now int64) *evalCtx {
	return &evalCtx{root: doc, cur: mkDoc(doc), now: now, vars: map[string]bson.RawValue{}}
}

// compileStage dispatches one stage by name.
func compileStage(name string, arg bson.RawValue) (stageSpec, error) {
	switch name {
	case "$match":
		return compileMatch(arg)
	case "$project":
		return compileProject(arg)
	case "$addFields", "$set":
		return compileAddFields(arg)
	case "$unset":
		return compileUnset(arg)
	case "$replaceRoot":
		return compileReplaceRoot(arg, false)
	case "$replaceWith":
		return compileReplaceRoot(arg, true)
	case "$limit":
		return compileLimit(arg)
	case "$skip":
		return compileSkip(arg)
	case "$count":
		return compileCount(arg)
	case "$unwind":
		return compileUnwind(arg)
	default:
		return nil, ErrBadStage
	}
}

// ---- $match --------------------------------------------------------------

func compileMatch(arg bson.RawValue) (stageSpec, error) {
	if arg.Type != bson.TypeDocument {
		return nil, ErrBadStage
	}
	d := arg.Document()
	elems, err := d.Elements()
	if err != nil {
		return nil, err
	}
	spec := &matchStage{}
	rest := bson.NewBuilder()
	hasRest := false
	for _, e := range elems {
		if e.Key == "$expr" {
			ex, cerr := compileExpr(e.Value)
			if cerr != nil {
				return nil, cerr
			}
			spec.expr = ex
			continue
		}
		rest.AppendValue(e.Key, e.Value)
		hasRest = true
	}
	if hasRest {
		m, merr := query.Compile(rest.Build())
		if merr != nil {
			return nil, merr
		}
		spec.matcher = m
	}
	return spec, nil
}

type matchStage struct {
	matcher *query.Matcher
	expr    Expr
}

func (s *matchStage) open(in src, now int64) src {
	return &matchSrc{in: in, now: now, spec: s}
}

type matchSrc struct {
	in   src
	now  int64
	spec *matchStage
}

func (s *matchSrc) next() (bson.Raw, error) {
	for {
		doc, err := s.in.next()
		if err != nil {
			return nil, err
		}
		if s.spec.matcher != nil && !s.spec.matcher.Match(doc) {
			continue
		}
		if s.spec.expr != nil && !truthy(s.spec.expr.eval(docCtx(doc, s.now))) {
			continue
		}
		return doc, nil
	}
}

// ---- $addFields / $set ---------------------------------------------------

func compileAddFields(arg bson.RawValue) (stageSpec, error) {
	if arg.Type != bson.TypeDocument {
		return nil, ErrBadStage
	}
	fields, err := compileFieldExprs(arg.Document())
	if err != nil {
		return nil, err
	}
	return &addFieldsStage{fields: fields}, nil
}

// fieldExpr binds a dotted output path to a compiled expression.
type fieldExpr struct {
	path []string
	expr Expr
}

// compileFieldExprs compiles each top-level field of a $addFields/$set/$group-like
// document into a path-and-expression pair.
func compileFieldExprs(d bson.Raw) ([]fieldExpr, error) {
	elems, err := d.Elements()
	if err != nil {
		return nil, err
	}
	out := make([]fieldExpr, 0, len(elems))
	for _, e := range elems {
		ex, cerr := compileExpr(e.Value)
		if cerr != nil {
			return nil, cerr
		}
		out = append(out, fieldExpr{path: splitPath(e.Key), expr: ex})
	}
	return out, nil
}

type addFieldsStage struct {
	fields []fieldExpr
}

func (s *addFieldsStage) open(in src, now int64) src {
	return &addFieldsSrc{in: in, now: now, stage: s}
}

type addFieldsSrc struct {
	in    src
	now   int64
	stage *addFieldsStage
}

func (s *addFieldsSrc) next() (bson.Raw, error) {
	doc, err := s.in.next()
	if err != nil {
		return nil, err
	}
	ctx := docCtx(doc, s.now)
	for _, f := range s.stage.fields {
		doc = docWith(doc, f.path, f.expr.eval(ctx))
	}
	return doc, nil
}

// ---- $unset --------------------------------------------------------------

func compileUnset(arg bson.RawValue) (stageSpec, error) {
	var paths [][]string
	switch arg.Type {
	case bson.TypeString:
		paths = append(paths, splitPath(arg.StringValue()))
	case bson.TypeArray:
		elems, err := arrayElements(arg)
		if err != nil {
			return nil, err
		}
		for _, e := range elems {
			s, ok := strOf(e)
			if !ok {
				return nil, ErrBadStage
			}
			paths = append(paths, splitPath(s))
		}
	default:
		return nil, ErrBadStage
	}
	return &unsetStage{paths: paths}, nil
}

type unsetStage struct {
	paths [][]string
}

func (s *unsetStage) open(in src, now int64) src {
	return &unsetSrc{in: in, stage: s}
}

type unsetSrc struct {
	in    src
	stage *unsetStage
}

func (s *unsetSrc) next() (bson.Raw, error) {
	doc, err := s.in.next()
	if err != nil {
		return nil, err
	}
	for _, p := range s.stage.paths {
		doc = docWith(doc, p, missing)
	}
	return doc, nil
}

// ---- $replaceRoot / $replaceWith ----------------------------------------

func compileReplaceRoot(arg bson.RawValue, with bool) (stageSpec, error) {
	var newRoot bson.RawValue
	if with {
		newRoot = arg
	} else {
		if arg.Type != bson.TypeDocument {
			return nil, ErrBadStage
		}
		v, ok := arg.Document().Lookup("newRoot")
		if !ok {
			return nil, ErrBadStage
		}
		newRoot = v
	}
	ex, err := compileExpr(newRoot)
	if err != nil {
		return nil, err
	}
	return &replaceRootStage{expr: ex}, nil
}

type replaceRootStage struct {
	expr Expr
}

func (s *replaceRootStage) open(in src, now int64) src {
	return &replaceRootSrc{in: in, now: now, stage: s}
}

type replaceRootSrc struct {
	in    src
	now   int64
	stage *replaceRootStage
}

func (s *replaceRootSrc) next() (bson.Raw, error) {
	doc, err := s.in.next()
	if err != nil {
		return nil, err
	}
	v := s.stage.expr.eval(docCtx(doc, s.now))
	if v.Type != bson.TypeDocument {
		return nil, ErrBadStage
	}
	return v.Document().Clone(), nil
}

// ---- $limit / $skip ------------------------------------------------------

func compileLimit(arg bson.RawValue) (stageSpec, error) {
	n, ok := intArg(arg)
	if !ok || n < 0 {
		return nil, ErrBadStage
	}
	return &limitStage{n: n}, nil
}

type limitStage struct{ n int }

func (s *limitStage) open(in src, now int64) src {
	return &limitSrc{in: in, remaining: s.n}
}

type limitSrc struct {
	in        src
	remaining int
}

func (s *limitSrc) next() (bson.Raw, error) {
	if s.remaining <= 0 {
		return nil, io.EOF
	}
	doc, err := s.in.next()
	if err != nil {
		return nil, err
	}
	s.remaining--
	return doc, nil
}

func compileSkip(arg bson.RawValue) (stageSpec, error) {
	n, ok := intArg(arg)
	if !ok || n < 0 {
		return nil, ErrBadStage
	}
	return &skipStage{n: n}, nil
}

type skipStage struct{ n int }

func (s *skipStage) open(in src, now int64) src {
	return &skipSrc{in: in, remaining: s.n}
}

type skipSrc struct {
	in        src
	remaining int
}

func (s *skipSrc) next() (bson.Raw, error) {
	for s.remaining > 0 {
		if _, err := s.in.next(); err != nil {
			return nil, err
		}
		s.remaining--
	}
	return s.in.next()
}

// ---- $count --------------------------------------------------------------

func compileCount(arg bson.RawValue) (stageSpec, error) {
	name, ok := strOf(arg)
	if !ok || name == "" {
		return nil, ErrBadStage
	}
	return &countStage{field: name}, nil
}

type countStage struct{ field string }

func (s *countStage) open(in src, now int64) src {
	return &countSrc{in: in, field: s.field}
}

type countSrc struct {
	in    src
	field string
	done  bool
}

func (s *countSrc) next() (bson.Raw, error) {
	if s.done {
		return nil, io.EOF
	}
	s.done = true
	var n int64
	for {
		_, err := s.in.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		n++
	}
	return bson.NewBuilder().AppendInt64(s.field, n).Build(), nil
}

// ---- $unwind -------------------------------------------------------------

func compileUnwind(arg bson.RawValue) (stageSpec, error) {
	st := &unwindStage{}
	switch arg.Type {
	case bson.TypeString:
		p, ok := unwindPath(arg.StringValue())
		if !ok {
			return nil, ErrBadStage
		}
		st.path = p
	case bson.TypeDocument:
		d := arg.Document()
		pv, ok := d.Lookup("path")
		if !ok {
			return nil, ErrBadStage
		}
		ps, sok := strOf(pv)
		if !sok {
			return nil, ErrBadStage
		}
		p, pok := unwindPath(ps)
		if !pok {
			return nil, ErrBadStage
		}
		st.path = p
		if iv, ok := d.Lookup("includeArrayIndex"); ok {
			if s, sok := strOf(iv); sok {
				st.indexField = splitPath(s)
			}
		}
		if pv, ok := d.Lookup("preserveNullAndEmptyArrays"); ok {
			st.preserve = truthy(pv)
		}
	default:
		return nil, ErrBadStage
	}
	return st, nil
}

// unwindPath strips the required leading $ from an $unwind path.
func unwindPath(s string) ([]string, bool) {
	if len(s) < 2 || s[0] != '$' {
		return nil, false
	}
	return splitPath(s[1:]), true
}

type unwindStage struct {
	path       []string
	indexField []string
	preserve   bool
}

func (s *unwindStage) open(in src, now int64) src {
	return &unwindSrc{in: in, stage: s}
}

type unwindSrc struct {
	in    src
	stage *unwindStage
	// pending holds the expansion of the current input document.
	pending []bson.Raw
	pi      int
}

func (s *unwindSrc) next() (bson.Raw, error) {
	for {
		if s.pi < len(s.pending) {
			d := s.pending[s.pi]
			s.pi++
			return d, nil
		}
		doc, err := s.in.next()
		if err != nil {
			return nil, err
		}
		s.pending = s.expand(doc)
		s.pi = 0
	}
}

// expand produces the unwound documents for one input document.
func (s *unwindSrc) expand(doc bson.Raw) []bson.Raw {
	v := resolvePath(mkDoc(doc), s.stage.path)
	if v.Type != bson.TypeArray {
		if s.stage.preserve || (!isMissing(v) && !isNull(v)) {
			if isMissing(v) && !s.stage.preserve {
				return nil
			}
			return []bson.Raw{s.withIndex(doc, missing)}
		}
		return nil
	}
	elems, err := arrayElements(v)
	if err != nil {
		return nil
	}
	if len(elems) == 0 {
		if s.stage.preserve {
			return []bson.Raw{s.withIndex(docWith(doc, s.stage.path, missing), missing)}
		}
		return nil
	}
	out := make([]bson.Raw, 0, len(elems))
	for i, el := range elems {
		nd := docWith(doc, s.stage.path, el)
		nd = s.withIndex(nd, mkInt64(int64(i)))
		out = append(out, nd)
	}
	return out
}

// withIndex sets the includeArrayIndex field when configured.
func (s *unwindSrc) withIndex(doc bson.Raw, idx bson.RawValue) bson.Raw {
	if s.stage.indexField == nil {
		return doc
	}
	if isMissing(idx) {
		idx = mkNull()
	}
	return docWith(doc, s.stage.indexField, idx)
}

// ---- shared document mutation -------------------------------------------

// docWith returns a copy of doc with the value at the dotted path set to val; a
// missing val removes the leaf. Intermediate documents are created as needed.
func docWith(doc bson.Raw, path []string, val bson.RawValue) bson.Raw {
	if len(path) == 0 {
		return doc
	}
	elems, err := doc.Elements()
	if err != nil {
		elems = nil
	}
	key := path[0]
	b := bson.NewBuilder()
	found := false
	for _, e := range elems {
		if e.Key != key {
			b.AppendValue(e.Key, e.Value)
			continue
		}
		found = true
		if len(path) == 1 {
			if !isMissing(val) {
				b.AppendValue(key, val)
			}
			continue
		}
		var child bson.Raw
		if e.Value.Type == bson.TypeDocument {
			child = e.Value.Document()
		} else {
			child = bson.NewBuilder().Build()
		}
		b.AppendDocument(key, docWith(child, path[1:], val))
	}
	if !found && !isMissing(val) {
		if len(path) == 1 {
			b.AppendValue(key, val)
		} else {
			b.AppendDocument(key, docWith(bson.NewBuilder().Build(), path[1:], val))
		}
	}
	return b.Build()
}
