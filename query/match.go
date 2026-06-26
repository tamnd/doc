package query

import (
	"errors"
	"fmt"
	"strings"

	"github.com/tamnd/doc/bson"
)

// ErrBadQuery reports a filter document that is not a valid MQL match expression:
// an unknown operator, a malformed argument, or a structurally invalid filter.
var ErrBadQuery = errors.New("query: invalid filter")

// Matcher is a compiled filter: a predicate tree evaluated against a document
// with MongoDB's three-valued match semantics flattened to a boolean (a document
// matches only when the top-level filter evaluates to true).
type Matcher struct {
	pred predicate
}

// Compile parses a filter document into a Matcher. An empty or nil filter matches
// every document.
func Compile(filter bson.Raw) (*Matcher, error) {
	p, err := compileDoc(filter)
	if err != nil {
		return nil, err
	}
	return &Matcher{pred: p}, nil
}

// Match reports whether doc satisfies the filter.
func (m *Matcher) Match(doc bson.Raw) bool { return m.pred.eval(doc) }

// predicate is one node of the compiled match tree.
type predicate interface {
	eval(doc bson.Raw) bool
}

type truePred struct{}

func (truePred) eval(bson.Raw) bool { return true }

type andPred struct{ parts []predicate }

func (p andPred) eval(doc bson.Raw) bool {
	for _, c := range p.parts {
		if !c.eval(doc) {
			return false
		}
	}
	return true
}

type orPred struct{ parts []predicate }

func (p orPred) eval(doc bson.Raw) bool {
	for _, c := range p.parts {
		if c.eval(doc) {
			return true
		}
	}
	return false
}

type notPred struct{ inner predicate }

func (p notPred) eval(doc bson.Raw) bool { return !p.inner.eval(doc) }

// fieldPred applies a leaf operator to a dotted path within a document, handling
// the dotted-path traversal, the array fan-out, and the missing-field rule.
type fieldPred struct {
	path []string
	leaf fieldLeaf
}

func (p fieldPred) eval(doc bson.Raw) bool {
	// Fast path: a path that resolves through plain documents (no array fan-out
	// at any step) reaches at most one value, so it needs no slice. This is the
	// common case (a top-level field or a nested document field such as an _id or
	// a comparison), and keeping it allocation-free is the doc 19 §24.1 predicate
	// invariant. The walk falls back to the slice traversal the moment it meets an
	// array, which is where MongoDB's fan-out semantics actually need every value.
	if v, found, simple := traverseSingle(doc, p.path); simple {
		if !found {
			return p.leaf.matchMissing()
		}
		if p.leaf.matchValue(v) {
			return true
		}
		if p.leaf.elementWise() && v.Type == bson.TypeArray {
			for _, e := range arrayElems(v) {
				if p.leaf.matchValue(e) {
					return true
				}
			}
		}
		return false
	}
	values, present := traverse(doc, p.path)
	if !present {
		return p.leaf.matchMissing()
	}
	for _, v := range values {
		if p.leaf.matchValue(v) {
			return true
		}
		if p.leaf.elementWise() && v.Type == bson.TypeArray {
			for _, e := range arrayElems(v) {
				if p.leaf.matchValue(e) {
					return true
				}
			}
		}
	}
	return false
}

// fieldLeaf is one operator applied to the value(s) a path resolves to.
type fieldLeaf interface {
	// matchValue reports whether a concrete present value satisfies the operator.
	matchValue(v bson.RawValue) bool
	// matchMissing reports whether absence of the field satisfies the operator
	// (the spec's null/missing conflation, doc 08 §17).
	matchMissing() bool
	// elementWise reports whether, when a resolved value is an array, the operator
	// should also be tried against each element (true for equality, comparison,
	// $in, $type; false for $size, $all, $elemMatch, which consume the array
	// themselves).
	elementWise() bool
}

// ---- compilation ---------------------------------------------------------

func compileDoc(d bson.Raw) (predicate, error) {
	if len(d) == 0 {
		return truePred{}, nil
	}
	if err := d.WellFormed(); err != nil {
		return nil, err
	}
	elems, err := d.Elements()
	if err != nil {
		return nil, err
	}
	parts := make([]predicate, 0, len(elems))
	for _, e := range elems {
		var (
			p   predicate
			cer error
		)
		if strings.HasPrefix(e.Key, "$") {
			p, cer = compileLogical(e.Key, e.Value)
		} else {
			p, cer = compileField(e.Key, e.Value)
		}
		if cer != nil {
			return nil, cer
		}
		parts = append(parts, p)
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	return andPred{parts: parts}, nil
}

// compileLogical compiles a top-level $and / $or / $nor expression, each taking an
// array of sub-filters.
func compileLogical(op string, v bson.RawValue) (predicate, error) {
	switch op {
	case "$and", "$or", "$nor":
		subs, err := compileFilterArray(v)
		if err != nil {
			return nil, err
		}
		switch op {
		case "$and":
			return andPred{parts: subs}, nil
		case "$or":
			return orPred{parts: subs}, nil
		default:
			return notPred{inner: orPred{parts: subs}}, nil
		}
	default:
		return nil, fmt.Errorf("%w: unsupported top-level operator %q", ErrBadQuery, op)
	}
}

func compileFilterArray(v bson.RawValue) ([]predicate, error) {
	if v.Type != bson.TypeArray {
		return nil, fmt.Errorf("%w: logical operator needs an array", ErrBadQuery)
	}
	elems := arrayElems(v)
	if len(elems) == 0 {
		return nil, fmt.Errorf("%w: logical operator needs a non-empty array", ErrBadQuery)
	}
	out := make([]predicate, 0, len(elems))
	for _, e := range elems {
		if e.Type != bson.TypeDocument {
			return nil, fmt.Errorf("%w: logical operand must be a document", ErrBadQuery)
		}
		p, err := compileDoc(e.Document())
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// compileField compiles a single field predicate: an operator document or, for a
// plain value, an equality.
func compileField(key string, v bson.RawValue) (predicate, error) {
	path := strings.Split(key, ".")
	if isOperatorDoc(v) {
		return compileOperators(path, v.Document())
	}
	return fieldPred{path: path, leaf: eqLeaf{want: v}}, nil
}

// isOperatorDoc reports whether a predicate value is an operator document: a
// document whose first field name begins with "$". A document whose first field
// is a plain name is an equality comparand (a nested-document match).
func isOperatorDoc(v bson.RawValue) bool {
	if v.Type != bson.TypeDocument {
		return false
	}
	elems, err := v.Document().Elements()
	if err != nil || len(elems) == 0 {
		return false
	}
	return strings.HasPrefix(elems[0].Key, "$")
}

// compileOperators compiles a {$op: arg, ...} document into the AND of its
// operator predicates on one path.
func compileOperators(path []string, d bson.Raw) (predicate, error) {
	elems, err := d.Elements()
	if err != nil {
		return nil, err
	}
	parts := make([]predicate, 0, len(elems))
	for _, e := range elems {
		p, err := compileOperator(path, e.Key, e.Value)
		if err != nil {
			return nil, err
		}
		if p != nil {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return truePred{}, nil
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	return andPred{parts: parts}, nil
}

func compileOperator(path []string, op string, arg bson.RawValue) (predicate, error) {
	switch op {
	case "$eq":
		return fieldPred{path: path, leaf: eqLeaf{want: arg}}, nil
	case "$ne":
		return notPred{inner: fieldPred{path: path, leaf: eqLeaf{want: arg}}}, nil
	case "$gt":
		return fieldPred{path: path, leaf: cmpLeaf{want: arg, op: opGt}}, nil
	case "$gte":
		return fieldPred{path: path, leaf: cmpLeaf{want: arg, op: opGte}}, nil
	case "$lt":
		return fieldPred{path: path, leaf: cmpLeaf{want: arg, op: opLt}}, nil
	case "$lte":
		return fieldPred{path: path, leaf: cmpLeaf{want: arg, op: opLte}}, nil
	case "$in":
		leaf, err := newInLeaf(arg)
		if err != nil {
			return nil, err
		}
		return fieldPred{path: path, leaf: leaf}, nil
	case "$nin":
		leaf, err := newInLeaf(arg)
		if err != nil {
			return nil, err
		}
		return notPred{inner: fieldPred{path: path, leaf: leaf}}, nil
	case "$exists":
		return fieldPred{path: path, leaf: existsLeaf{want: truthy(arg)}}, nil
	case "$type":
		leaf, err := newTypeLeaf(arg)
		if err != nil {
			return nil, err
		}
		return fieldPred{path: path, leaf: leaf}, nil
	case "$size":
		n, ok := arg.AsFloat64()
		if !ok || n < 0 || n != float64(int64(n)) {
			return nil, fmt.Errorf("%w: $size needs a non-negative integer", ErrBadQuery)
		}
		return fieldPred{path: path, leaf: sizeLeaf{n: int(n)}}, nil
	case "$all":
		return newAllPred(path, arg)
	case "$elemMatch":
		return newElemMatchPred(path, arg)
	case "$not":
		if !isOperatorDoc(arg) {
			return nil, fmt.Errorf("%w: $not needs an operator document", ErrBadQuery)
		}
		inner, err := compileOperators(path, arg.Document())
		if err != nil {
			return nil, err
		}
		return notPred{inner: inner}, nil
	default:
		return nil, fmt.Errorf("%w: unsupported operator %q", ErrBadQuery, op)
	}
}
