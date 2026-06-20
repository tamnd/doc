package query

import (
	"fmt"
	"strings"

	"github.com/tamnd/doc/bson"
)

// Projection reshapes a result document by including or excluding a fixed set of
// fields (spec 2061 doc 11 §5). MongoDB allows two shapes: an inclusion projection
// that keeps only the named fields, and an exclusion projection that drops the
// named fields, with _id available in either. The two cannot be mixed except for
// excluding _id from an inclusion projection.
type Projection struct {
	// fields maps a dotted path to keep (inclusion) or drop (exclusion).
	fields  map[string]bool
	include bool // true for an inclusion projection, false for exclusion
	keepID  bool // whether _id survives
	empty   bool // no projection: return the document unchanged
}

// CompileProjection parses a projection document. A nil or empty projection is a
// pass-through. A field value that is truthy means include, falsy means exclude;
// the first non-_id field fixes the projection's polarity and every other non-_id
// field must agree (spec 2061 doc 11 §5.2).
func CompileProjection(proj bson.Raw) (*Projection, error) {
	if len(proj) == 0 {
		return &Projection{empty: true}, nil
	}
	elems, err := proj.Elements()
	if err != nil {
		return nil, err
	}
	if len(elems) == 0 {
		return &Projection{empty: true}, nil
	}
	p := &Projection{fields: map[string]bool{}, keepID: true}
	polaritySet := false
	idExplicit := false
	for _, e := range elems {
		keep := projectionTruthy(e.Value)
		if e.Key == "_id" {
			p.keepID = keep
			idExplicit = true
			continue
		}
		if !polaritySet {
			p.include = keep
			polaritySet = true
		} else if keep != p.include {
			return nil, fmt.Errorf("%w: cannot mix inclusion and exclusion in a projection", ErrBadQuery)
		}
		p.fields[e.Key] = true
	}
	if !polaritySet {
		// Only _id was named. {_id:0} is an exclusion; {_id:1} an inclusion.
		p.include = p.keepID
	}
	// In both an inclusion and an exclusion projection _id is kept unless it was
	// explicitly excluded (spec 2061 doc 11 §5.2).
	if !idExplicit {
		p.keepID = true
	}
	return p, nil
}

// Apply returns the projected document. The original is never mutated.
func (p *Projection) Apply(d bson.Raw) (bson.Raw, error) {
	if p.empty {
		return d, nil
	}
	elems, err := d.Elements()
	if err != nil {
		return nil, err
	}
	b := bson.NewBuilder()
	for _, e := range elems {
		if e.Key == "_id" {
			if p.keepID {
				b.AppendValue(e.Key, e.Value)
			}
			continue
		}
		if p.keep(e.Key) {
			b.AppendValue(e.Key, e.Value)
		}
	}
	return b.Build(), nil
}

// keep reports whether a non-_id top-level field survives the projection. Dotted
// projection paths keep a field when the path's first segment names it; the value
// is carried whole (sub-document pruning is a documented later refinement).
func (p *Projection) keep(field string) bool {
	if p.include {
		return p.named(field)
	}
	return !p.named(field)
}

func (p *Projection) named(field string) bool {
	if p.fields[field] {
		return true
	}
	for path := range p.fields {
		if seg := strings.SplitN(path, ".", 2)[0]; seg == field {
			return true
		}
	}
	return false
}

// projectionTruthy interprets a projection field value: false, 0, and null mean
// exclude; anything else means include.
func projectionTruthy(v bson.RawValue) bool { return truthy(v) }
