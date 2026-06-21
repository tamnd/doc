package agg

import (
	"io"

	"github.com/tamnd/doc/bson"
)

// ---- $facet --------------------------------------------------------------

// facetField pairs an output field name with its compiled sub-pipeline.
type facetField struct {
	name string
	pipe *Pipeline
}

// compileFacet compiles a multi-pipeline stage that runs each sub-pipeline over the
// same buffered input (spec 2061 doc 12 §13). $facet, $out, and $merge are not
// allowed inside a sub-pipeline.
func compileFacet(arg bson.RawValue) (stageSpec, error) {
	if arg.Type != bson.TypeDocument {
		return nil, ErrBadStage
	}
	elems, err := arg.Document().Elements()
	if err != nil {
		return nil, err
	}
	st := &facetStage{}
	for _, e := range elems {
		stages, derr := arrayDocs(e.Value)
		if derr != nil {
			return nil, derr
		}
		if err := rejectInFacet(stages); err != nil {
			return nil, err
		}
		pipe, cerr := Compile(stages)
		if cerr != nil {
			return nil, cerr
		}
		st.fields = append(st.fields, facetField{name: e.Key, pipe: pipe})
	}
	return st, nil
}

// rejectInFacet enforces the sub-pipeline stage constraints of $facet.
func rejectInFacet(stages []bson.Raw) error {
	for _, s := range stages {
		els, err := s.Elements()
		if err != nil {
			return err
		}
		if len(els) != 1 {
			return ErrBadStage
		}
		switch els[0].Key {
		case "$facet", "$out", "$merge":
			return ErrBadStage
		}
	}
	return nil
}

type facetStage struct {
	fields []facetField
}

func (s *facetStage) open(in src, ec *execCtx) src {
	return &facetSrc{in: in, stage: s, ec: ec}
}

type facetSrc struct {
	in    src
	stage *facetStage
	ec    *execCtx
	done  bool
}

func (s *facetSrc) next() (bson.Raw, error) {
	if s.done {
		return nil, io.EOF
	}
	s.done = true
	buffered, err := drain(s.in)
	if err != nil {
		return nil, err
	}
	b := bson.NewBuilder()
	for _, f := range s.stage.fields {
		res, rerr := f.pipe.runInner(buffered, s.ec)
		if rerr != nil {
			return nil, rerr
		}
		b.AppendArray(f.name, bson.BuildArray(rawsToValues(res)...))
	}
	return b.Build(), nil
}
