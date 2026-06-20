package query

import (
	"fmt"
	"strings"

	"github.com/tamnd/doc/bson"
)

// eqLeaf is equality: the resolved value compares equal to the operand under the
// BSON total order, which makes a numeric comparison cross-type and a NaN match a
// NaN (spec 2061 doc 08 §3.2). Missing matches only when the operand is null.
type eqLeaf struct{ want bson.RawValue }

func (l eqLeaf) matchValue(v bson.RawValue) bool { return bson.Equal(v, l.want) }
func (l eqLeaf) matchMissing() bool              { return isNullOperand(l.want) }
func (l eqLeaf) elementWise() bool               { return true }

type cmpOp int

const (
	opGt cmpOp = iota
	opGte
	opLt
	opLte
)

// cmpLeaf is a range operator. The comparison only applies within the operand's
// canonical type bracket: a value of a different type never satisfies $gt/$lt
// (spec 2061 doc 08 §4.2), and a missing field never does. Comparisons against
// null and against the array/document brackets follow the same bracket rule.
type cmpLeaf struct {
	want bson.RawValue
	op   cmpOp
}

func (l cmpLeaf) matchValue(v bson.RawValue) bool {
	if bson.CanonicalType(v.Type) != bson.CanonicalType(l.want.Type) {
		return false
	}
	c := bson.Compare(v, l.want)
	switch l.op {
	case opGt:
		return c > 0
	case opGte:
		return c >= 0
	case opLt:
		return c < 0
	default:
		return c <= 0
	}
}

func (l cmpLeaf) matchMissing() bool { return false }
func (l cmpLeaf) elementWise() bool  { return true }

// inLeaf is $in: the value equals any operand in the list. A regex operand would
// match by pattern, deferred here; a null operand makes a missing field match.
type inLeaf struct {
	wants   []bson.RawValue
	hasNull bool
}

func newInLeaf(arg bson.RawValue) (inLeaf, error) {
	if arg.Type != bson.TypeArray {
		return inLeaf{}, fmt.Errorf("%w: $in/$nin needs an array", ErrBadQuery)
	}
	elems := arrayElems(arg)
	leaf := inLeaf{wants: make([]bson.RawValue, 0, len(elems))}
	for _, e := range elems {
		if isOperatorDoc(e) {
			return inLeaf{}, fmt.Errorf("%w: $in/$nin does not take operator documents", ErrBadQuery)
		}
		if isNullOperand(e) {
			leaf.hasNull = true
		}
		leaf.wants = append(leaf.wants, e)
	}
	return leaf, nil
}

func (l inLeaf) matchValue(v bson.RawValue) bool {
	for _, w := range l.wants {
		if bson.Equal(v, w) {
			return true
		}
	}
	return false
}

func (l inLeaf) matchMissing() bool { return l.hasNull }
func (l inLeaf) elementWise() bool  { return true }

// existsLeaf is $exists: a present value matches when the argument is truthy, a
// missing field when it is falsy (spec 2061 doc 08 §6.1).
type existsLeaf struct{ want bool }

func (l existsLeaf) matchValue(bson.RawValue) bool { return l.want }
func (l existsLeaf) matchMissing() bool            { return !l.want }
func (l existsLeaf) elementWise() bool             { return false }

// typeLeaf is $type: the value's BSON type is one of the requested types. It is
// element-wise, so {a:{$type:"int"}} matches an array containing an int, and the
// whole-array case matches {$type:"array"} (spec 2061 doc 08 §6.2).
type typeLeaf struct{ want map[bson.Type]bool }

func newTypeLeaf(arg bson.RawValue) (typeLeaf, error) {
	leaf := typeLeaf{want: map[bson.Type]bool{}}
	add := func(v bson.RawValue) error {
		ts, err := resolveTypeArg(v)
		if err != nil {
			return err
		}
		for _, t := range ts {
			leaf.want[t] = true
		}
		return nil
	}
	if arg.Type == bson.TypeArray {
		for _, e := range arrayElems(arg) {
			if err := add(e); err != nil {
				return typeLeaf{}, err
			}
		}
		return leaf, nil
	}
	if err := add(arg); err != nil {
		return typeLeaf{}, err
	}
	return leaf, nil
}

func (l typeLeaf) matchValue(v bson.RawValue) bool { return l.want[v.Type] }
func (l typeLeaf) matchMissing() bool              { return false }
func (l typeLeaf) elementWise() bool               { return true }

// sizeLeaf is $size: an array whose length equals n. It is not element-wise; it
// inspects the array as a whole (spec 2061 doc 08 §7.2).
type sizeLeaf struct{ n int }

func (l sizeLeaf) matchValue(v bson.RawValue) bool {
	return v.Type == bson.TypeArray && len(arrayElems(v)) == l.n
}
func (l sizeLeaf) matchMissing() bool { return false }
func (l sizeLeaf) elementWise() bool  { return false }

// allPred is $all: the value contains every operand. Against an array it requires
// each operand to equal some element; against a scalar it requires every operand
// to equal that scalar (spec 2061 doc 08 §7.3).
type allLeaf struct{ wants []bson.RawValue }

func newAllPred(path []string, arg bson.RawValue) (predicate, error) {
	if arg.Type != bson.TypeArray {
		return nil, fmt.Errorf("%w: $all needs an array", ErrBadQuery)
	}
	elems := arrayElems(arg)
	wants := make([]bson.RawValue, 0, len(elems))
	for _, e := range elems {
		if isOperatorDoc(e) {
			return nil, fmt.Errorf("%w: $all with operator documents is not supported", ErrBadQuery)
		}
		wants = append(wants, e)
	}
	// An empty $all matches nothing, which a leaf that always returns false on a
	// present value (and on missing) expresses directly.
	return fieldPred{path: path, leaf: allLeaf{wants: wants}}, nil
}

func (l allLeaf) matchValue(v bson.RawValue) bool {
	if len(l.wants) == 0 {
		return false
	}
	for _, w := range l.wants {
		if !l.contains(v, w) {
			return false
		}
	}
	return true
}

func (l allLeaf) contains(v, w bson.RawValue) bool {
	if bson.Equal(v, w) {
		return true
	}
	if v.Type == bson.TypeArray {
		for _, e := range arrayElems(v) {
			if bson.Equal(e, w) {
				return true
			}
		}
	}
	return false
}

func (l allLeaf) matchMissing() bool { return false }
func (l allLeaf) elementWise() bool  { return false }

// elemMatchLeaf is $elemMatch: at least one array element satisfies all the
// sub-criteria. The argument is either an operator document applied to each
// element directly ({$gt:5,$lt:10}) or a criteria document applied to each element
// as a sub-document ({b:1,c:{$gt:2}}) (spec 2061 doc 08 §7.4).
type elemMatchLeaf struct {
	inner predicate
	// wrap distinguishes the two argument forms. In the operator form the inner
	// predicate binds to the element value through a synthetic empty-key field, so
	// each element is wrapped before evaluation. In the criteria form the inner
	// predicate is a sub-filter over the element as a document, evaluated directly.
	wrap bool
}

func newElemMatchPred(path []string, arg bson.RawValue) (predicate, error) {
	if arg.Type != bson.TypeDocument {
		return nil, fmt.Errorf("%w: $elemMatch needs a document", ErrBadQuery)
	}
	inner, wrap, err := compileElemMatch(arg.Document())
	if err != nil {
		return nil, err
	}
	return fieldPred{path: path, leaf: elemMatchLeaf{inner: inner, wrap: wrap}}, nil
}

// compileElemMatch builds the per-element predicate and reports whether elements
// must be wrapped (the operator form). When the document's first field is an
// operator, the operators apply to the element value itself, reached through the
// empty-key path; otherwise the document is a full sub-filter on the element.
func compileElemMatch(d bson.Raw) (predicate, bool, error) {
	elems, err := d.Elements()
	if err != nil {
		return nil, false, err
	}
	if len(elems) > 0 && strings.HasPrefix(elems[0].Key, "$") {
		emptyPath := []string{""}
		parts := make([]predicate, 0, len(elems))
		for _, e := range elems {
			p, err := compileOperator(emptyPath, e.Key, e.Value)
			if err != nil {
				return nil, false, err
			}
			if p != nil {
				parts = append(parts, p)
			}
		}
		if len(parts) == 1 {
			return parts[0], true, nil
		}
		return andPred{parts: parts}, true, nil
	}
	inner, err := compileDoc(d)
	return inner, false, err
}

func (l elemMatchLeaf) matchValue(v bson.RawValue) bool {
	if v.Type != bson.TypeArray {
		return false
	}
	for _, e := range arrayElems(v) {
		if l.wrap {
			if l.inner.eval(bson.NewBuilder().AppendValue("", e).Build()) {
				return true
			}
			continue
		}
		// Criteria form: only document elements carry the named sub-fields.
		if e.Type == bson.TypeDocument && l.inner.eval(e.Document()) {
			return true
		}
	}
	return false
}

func (l elemMatchLeaf) matchMissing() bool { return false }
func (l elemMatchLeaf) elementWise() bool  { return false }

// isNullOperand reports whether an operand is BSON null (or the deprecated
// undefined), which the equality and $in operators treat as also matching a
// missing field (spec 2061 doc 08 §17).
func isNullOperand(v bson.RawValue) bool {
	return v.Type == bson.TypeNull || v.Type == bson.TypeUndefined
}

// truthy reports the truth value of a $exists argument: any value other than
// false, 0, or null is true (MongoDB treats $exists:1 as $exists:true).
func truthy(v bson.RawValue) bool {
	switch v.Type {
	case bson.TypeBoolean:
		return v.Boolean()
	case bson.TypeNull, bson.TypeUndefined:
		return false
	default:
		if f, ok := v.AsFloat64(); ok {
			return f != 0
		}
		return true
	}
}

// resolveTypeArg maps a $type argument (a type alias string or a numeric type
// code) to the BSON types it names. The "number" alias expands to all four
// numeric types (spec 2061 doc 08 §6.2).
func resolveTypeArg(v bson.RawValue) ([]bson.Type, error) {
	if s, ok := v.StringValueOK(); ok {
		if s == "number" {
			return []bson.Type{bson.TypeDouble, bson.TypeInt32, bson.TypeInt64, bson.TypeDecimal128}, nil
		}
		t, ok := typeAlias[s]
		if !ok {
			return nil, fmt.Errorf("%w: unknown $type alias %q", ErrBadQuery, s)
		}
		return []bson.Type{t}, nil
	}
	if f, ok := v.AsFloat64(); ok && f == float64(int64(f)) {
		return []bson.Type{bson.Type(byte(int64(f)))}, nil
	}
	return nil, fmt.Errorf("%w: $type needs a string alias or numeric code", ErrBadQuery)
}

// typeAlias maps MongoDB's $type string aliases to BSON type bytes.
var typeAlias = map[string]bson.Type{
	"double":              bson.TypeDouble,
	"string":              bson.TypeString,
	"object":              bson.TypeDocument,
	"array":               bson.TypeArray,
	"binData":             bson.TypeBinary,
	"undefined":           bson.TypeUndefined,
	"objectId":            bson.TypeObjectID,
	"bool":                bson.TypeBoolean,
	"date":                bson.TypeDateTime,
	"null":                bson.TypeNull,
	"regex":               bson.TypeRegex,
	"dbPointer":           bson.TypeDBPointer,
	"javascript":          bson.TypeJavaScript,
	"symbol":              bson.TypeSymbol,
	"javascriptWithScope": bson.TypeCodeWithScope,
	"int":                 bson.TypeInt32,
	"timestamp":           bson.TypeTimestamp,
	"long":                bson.TypeInt64,
	"decimal":             bson.TypeDecimal128,
	"minKey":              bson.TypeMinKey,
	"maxKey":              bson.TypeMaxKey,
}
