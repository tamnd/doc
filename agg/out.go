package agg

import (
	"io"

	"github.com/tamnd/doc/bson"
)

// ---- $out ----------------------------------------------------------------

// compileOut compiles the string and {db, coll} forms of $out (spec 2061 doc 12
// §16.1). $out must be the final stage and emits no documents to the caller.
func compileOut(arg bson.RawValue) (stageSpec, error) {
	var coll string
	switch arg.Type {
	case bson.TypeString:
		coll = arg.StringValue()
	case bson.TypeDocument:
		cv, ok := arg.Document().Lookup("coll")
		if !ok {
			return nil, ErrBadStage
		}
		c, cok := strOf(cv)
		if !cok {
			return nil, ErrBadStage
		}
		coll = c
	default:
		return nil, ErrBadStage
	}
	if coll == "" {
		return nil, ErrBadStage
	}
	return &outStage{coll: coll}, nil
}

type outStage struct{ coll string }

func (s *outStage) open(in src, ec *execCtx) src {
	return &writeSrc{in: in, ec: ec, req: WriteRequest{Coll: s.coll, Replace: true}}
}

// ---- $merge --------------------------------------------------------------

// compileMerge compiles the string and document forms of $merge (spec 2061 doc 12
// §16.2). The pipeline form of whenMatched is not yet supported.
func compileMerge(arg bson.RawValue) (stageSpec, error) {
	req := WriteRequest{On: []string{"_id"}, WhenMatched: "merge", WhenNotMatched: "insert"}
	switch arg.Type {
	case bson.TypeString:
		req.Coll = arg.StringValue()
	case bson.TypeDocument:
		d := arg.Document()
		into, ok := d.Lookup("into")
		if !ok {
			return nil, ErrBadStage
		}
		switch into.Type {
		case bson.TypeString:
			req.Coll = into.StringValue()
		case bson.TypeDocument:
			cv, cok := into.Document().Lookup("coll")
			if !cok {
				return nil, ErrBadStage
			}
			c, sok := strOf(cv)
			if !sok {
				return nil, ErrBadStage
			}
			req.Coll = c
		default:
			return nil, ErrBadStage
		}
		if on, ok := d.Lookup("on"); ok {
			fields, err := onFields(on)
			if err != nil {
				return nil, err
			}
			req.On = fields
		}
		if wm, ok := d.Lookup("whenMatched"); ok {
			s, sok := strOf(wm)
			if !sok {
				return nil, ErrBadStage
			}
			req.WhenMatched = s
		}
		if wn, ok := d.Lookup("whenNotMatched"); ok {
			s, sok := strOf(wn)
			if !sok {
				return nil, ErrBadStage
			}
			req.WhenNotMatched = s
		}
	default:
		return nil, ErrBadStage
	}
	if req.Coll == "" {
		return nil, ErrBadStage
	}
	return &mergeStage{req: req}, nil
}

// onFields reads the $merge on argument: a single field name or an array of names.
func onFields(v bson.RawValue) ([]string, error) {
	switch v.Type {
	case bson.TypeString:
		return []string{v.StringValue()}, nil
	case bson.TypeArray:
		elems, err := arrayElements(v)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(elems))
		for _, e := range elems {
			s, ok := strOf(e)
			if !ok {
				return nil, ErrBadStage
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, ErrBadStage
	}
}

type mergeStage struct{ req WriteRequest }

func (s *mergeStage) open(in src, ec *execCtx) src {
	return &writeSrc{in: in, ec: ec, req: s.req}
}

// ---- shared write source -------------------------------------------------

// writeSrc backs $out and $merge: it drains the upstream, hands the collected
// documents to the environment's Write callback, then reports an empty stream.
type writeSrc struct {
	in   src
	ec   *execCtx
	req  WriteRequest
	done bool
}

func (s *writeSrc) next() (bson.Raw, error) {
	if !s.done {
		if s.ec.env == nil || s.ec.env.Write == nil {
			return nil, ErrNoEnv
		}
		docs, err := drain(s.in)
		if err != nil {
			return nil, err
		}
		req := s.req
		req.Docs = docs
		if err := s.ec.env.Write(req); err != nil {
			return nil, err
		}
		s.done = true
	}
	return nil, io.EOF
}
