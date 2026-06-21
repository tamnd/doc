package agg

import (
	"github.com/tamnd/doc/bson"
)

// The $redact system variables resolve to sentinel values that only $redact
// interprets. They are encoded as strings with a NUL-delimited private prefix so a
// user string literal cannot collide with them (spec 2061 doc 12 §17.1).
var (
	redactKeep    = mkString("\x00redact\x00KEEP")
	redactPrune   = mkString("\x00redact\x00PRUNE")
	redactDescend = mkString("\x00redact\x00DESCEND")
)

// redact decisions.
const (
	decisionError = iota
	decisionKeep
	decisionPrune
	decisionDescend
)

// redactDecision maps a $redact expression result to a decision.
func redactDecision(v bson.RawValue) int {
	switch {
	case bson.Equal(v, redactKeep):
		return decisionKeep
	case bson.Equal(v, redactPrune):
		return decisionPrune
	case bson.Equal(v, redactDescend):
		return decisionDescend
	default:
		return decisionError
	}
}

// ---- $redact -------------------------------------------------------------

// compileRedact compiles a field-level access-control stage (spec 2061 doc 12
// §17.1).
func compileRedact(arg bson.RawValue) (stageSpec, error) {
	ex, err := compileExpr(arg)
	if err != nil {
		return nil, err
	}
	return &redactStage{expr: ex}, nil
}

type redactStage struct{ expr Expr }

func (s *redactStage) open(in src, ec *execCtx) src {
	return &redactSrc{in: in, stage: s, ec: ec}
}

type redactSrc struct {
	in    src
	stage *redactStage
	ec    *execCtx
}

func (s *redactSrc) next() (bson.Raw, error) {
	for {
		doc, err := s.in.next()
		if err != nil {
			return nil, err
		}
		out, keep, rerr := s.redactDoc(doc, doc)
		if rerr != nil {
			return nil, rerr
		}
		if keep {
			return out, nil
		}
		// A pruned top-level document is dropped; pull the next one.
	}
}

// redactDoc applies the redact expression to one document. root stays the whole
// input document so the expression can reference outer fields; cur is the
// subdocument currently under evaluation via $$CURRENT.
func (s *redactSrc) redactDoc(root, cur bson.Raw) (bson.Raw, bool, error) {
	ctx := docCtx(root, s.ec)
	ctx.cur = mkDoc(cur)
	v := s.stage.expr.eval(ctx)
	switch redactDecision(v) {
	case decisionKeep:
		return cur, true, nil
	case decisionPrune:
		return nil, false, nil
	case decisionDescend:
		out, err := s.descend(root, cur)
		return out, true, err
	default:
		return nil, false, ErrBadExpr
	}
}

// descend recurses into each subdocument and array element, re-evaluating the
// redact expression and rebuilding the document from the surviving fields.
func (s *redactSrc) descend(root, cur bson.Raw) (bson.Raw, error) {
	elems, err := cur.Elements()
	if err != nil {
		return nil, err
	}
	b := bson.NewBuilder()
	for _, e := range elems {
		val, keep, rerr := s.redactValue(root, e.Value)
		if rerr != nil {
			return nil, rerr
		}
		if keep {
			b.AppendValue(e.Key, val)
		}
	}
	return b.Build(), nil
}

// redactValue applies redaction to a single value: documents recurse through the
// decision logic, arrays redact each document element, scalars pass through.
func (s *redactSrc) redactValue(root bson.Raw, v bson.RawValue) (bson.RawValue, bool, error) {
	switch v.Type {
	case bson.TypeDocument:
		out, keep, err := s.redactDoc(root, v.Document())
		if err != nil || !keep {
			return missing, false, err
		}
		return mkDoc(out), true, nil
	case bson.TypeArray:
		els, err := arrayElements(v)
		if err != nil {
			return missing, false, err
		}
		var kept []bson.RawValue
		for _, el := range els {
			ev, keep, rerr := s.redactValue(root, el)
			if rerr != nil {
				return missing, false, rerr
			}
			if keep {
				kept = append(kept, ev)
			}
		}
		return mkArray(kept), true, nil
	default:
		return v, true, nil
	}
}
