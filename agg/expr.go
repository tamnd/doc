package agg

import (
	"errors"
	"strconv"
	"strings"

	"github.com/tamnd/doc/bson"
)

// ErrBadExpr reports a malformed aggregation expression: an unknown operator, an
// operator argument of the wrong shape, or a document that mixes operator and
// plain keys.
var ErrBadExpr = errors.New("agg: malformed expression")

// Expr is a compiled aggregation expression. eval computes its value against the
// evaluation context (the current document and the variable environment); a result
// of the missing value (Type 0) means the expression produced no value (spec 2061
// doc 12 §3.6).
type Expr interface {
	eval(c *evalCtx) bson.RawValue
}

// evalCtx is the environment an expression evaluates against: root is $$ROOT, cur
// is $$CURRENT, now is $$NOW in epoch milliseconds, and vars holds user variables
// ($let, $map, $filter, $reduce) plus $$this and $$value.
type evalCtx struct {
	root bson.Raw
	cur  bson.RawValue
	now  int64
	vars map[string]bson.RawValue
}

// withVar returns a child context with name bound to v, leaving the parent's
// bindings intact (lexical scoping for $let/$map/$filter/$reduce).
func (c *evalCtx) withVar(name string, v bson.RawValue) *evalCtx {
	nv := make(map[string]bson.RawValue, len(c.vars)+1)
	for k, val := range c.vars {
		nv[k] = val
	}
	nv[name] = v
	return &evalCtx{root: c.root, cur: c.cur, now: c.now, vars: nv}
}

// constExpr is a literal BSON value.
type constExpr struct{ v bson.RawValue }

func (e constExpr) eval(*evalCtx) bson.RawValue { return e.v }

// fieldPathExpr resolves a "$a.b.c" field path against $$CURRENT.
type fieldPathExpr struct{ path []string }

func (e fieldPathExpr) eval(c *evalCtx) bson.RawValue {
	return resolvePath(c.cur, e.path)
}

// varExpr resolves a "$$VAR.a.b" system or user variable, then walks the path.
type varExpr struct {
	name string
	path []string
}

func (e varExpr) eval(c *evalCtx) bson.RawValue {
	var base bson.RawValue
	switch e.name {
	case "ROOT":
		base = mkDoc(c.root)
	case "CURRENT":
		base = c.cur
	case "NOW":
		base = mkDate(c.now)
	case "REMOVE":
		return missing
	default:
		v, ok := c.vars[e.name]
		if !ok {
			return missing
		}
		base = v
	}
	return resolvePath(base, e.path)
}

// arrayExpr evaluates each element expression into an array value.
type arrayExpr struct{ elems []Expr }

func (e arrayExpr) eval(c *evalCtx) bson.RawValue {
	vals := make([]bson.RawValue, len(e.elems))
	for i, el := range e.elems {
		v := el.eval(c)
		if isMissing(v) {
			v = mkNull() // array elements cannot be absent; missing becomes null
		}
		vals[i] = v
	}
	return mkArray(vals)
}

// objectExpr evaluates a plain (non-operator) document, computing each field's
// value; a field whose value evaluates to missing is omitted (spec 2061 doc 12
// §3.5, §6.1).
type objectExpr struct {
	keys  []string
	exprs []Expr
}

func (e objectExpr) eval(c *evalCtx) bson.RawValue {
	b := bson.NewBuilder()
	for i, k := range e.keys {
		v := e.exprs[i].eval(c)
		if isMissing(v) {
			continue
		}
		b.AppendValue(k, v)
	}
	return mkDoc(b.Build())
}

// compileExpr compiles one aggregation expression value into an Expr.
func compileExpr(v bson.RawValue) (Expr, error) {
	switch v.Type {
	case bson.TypeString:
		s := v.StringValue()
		if strings.HasPrefix(s, "$$") {
			return compileVar(s[2:])
		}
		if strings.HasPrefix(s, "$") {
			return fieldPathExpr{path: splitPath(s[1:])}, nil
		}
		return constExpr{v: v}, nil
	case bson.TypeArray:
		elems, err := arrayElements(v)
		if err != nil {
			return nil, err
		}
		out := make([]Expr, len(elems))
		for i, el := range elems {
			ce, cerr := compileExpr(el)
			if cerr != nil {
				return nil, cerr
			}
			out[i] = ce
		}
		return arrayExpr{elems: out}, nil
	case bson.TypeDocument:
		return compileDocExpr(v.Document())
	default:
		return constExpr{v: v}, nil
	}
}

// compileVar compiles a "$$VAR" reference, splitting the variable name from any
// trailing field path.
func compileVar(s string) (Expr, error) {
	if s == "" {
		return nil, ErrBadExpr
	}
	parts := strings.SplitN(s, ".", 2)
	e := varExpr{name: parts[0]}
	if len(parts) == 2 {
		e.path = splitPath(parts[1])
	}
	return e, nil
}

// compileDocExpr compiles a document in expression position: a single $-prefixed
// key is an operator expression; otherwise every key must be plain and the
// document is an object expression. Mixing the two is an error.
func compileDocExpr(d bson.Raw) (Expr, error) {
	elems, err := d.Elements()
	if err != nil {
		return nil, err
	}
	if len(elems) == 0 {
		return constExpr{v: mkDoc(d)}, nil
	}
	hasOp := strings.HasPrefix(elems[0].Key, "$")
	if hasOp {
		if len(elems) != 1 {
			return nil, ErrBadExpr // an operator document has exactly one key
		}
		return compileOperator(elems[0].Key, elems[0].Value)
	}
	keys := make([]string, len(elems))
	exprs := make([]Expr, len(elems))
	for i, e := range elems {
		if strings.HasPrefix(e.Key, "$") {
			return nil, ErrBadExpr
		}
		ce, cerr := compileExpr(e.Value)
		if cerr != nil {
			return nil, cerr
		}
		keys[i] = e.Key
		exprs[i] = ce
	}
	return objectExpr{keys: keys, exprs: exprs}, nil
}

// compileOperator dispatches one operator expression by name through opTable.
func compileOperator(op string, arg bson.RawValue) (Expr, error) {
	fn, ok := opTable[op]
	if !ok {
		return nil, ErrBadExpr
	}
	return fn(arg)
}

// compileArgs compiles an operator's argument list. An operator that takes an
// array of operands receives them directly; a single non-array argument is
// wrapped as a one-element list so unary operators accept both forms.
func compileArgs(arg bson.RawValue) ([]Expr, error) {
	if arg.Type == bson.TypeArray {
		elems, err := arrayElements(arg)
		if err != nil {
			return nil, err
		}
		out := make([]Expr, len(elems))
		for i, e := range elems {
			ce, cerr := compileExpr(e)
			if cerr != nil {
				return nil, cerr
			}
			out[i] = ce
		}
		return out, nil
	}
	ce, err := compileExpr(arg)
	if err != nil {
		return nil, err
	}
	return []Expr{ce}, nil
}

// resolvePath walks a dotted field path from a value: into documents by field
// name, into arrays by numeric index, and over arrays by field name (mapping the
// remaining path across the document elements and collecting an array). A miss at
// any step yields the missing value (spec 2061 doc 12 §3.2).
func resolvePath(v bson.RawValue, path []string) bson.RawValue {
	if len(path) == 0 {
		return v
	}
	seg, rest := path[0], path[1:]
	switch v.Type {
	case bson.TypeDocument:
		child, ok := v.Document().Lookup(seg)
		if !ok {
			return missing
		}
		return resolvePath(child, rest)
	case bson.TypeArray:
		elems, err := arrayElements(v)
		if err != nil {
			return missing
		}
		if idx, ierr := strconv.Atoi(seg); ierr == nil && idx >= 0 {
			if idx < len(elems) {
				return resolvePath(elems[idx], rest)
			}
			return missing
		}
		var out []bson.RawValue
		for _, el := range elems {
			if el.Type == bson.TypeDocument {
				r := resolvePath(el, path)
				if !isMissing(r) {
					out = append(out, r)
				}
			}
		}
		return mkArray(out)
	default:
		return missing
	}
}

// arrayElements returns the element values of an array RawValue in order.
func arrayElements(v bson.RawValue) ([]bson.RawValue, error) {
	if v.Type != bson.TypeArray {
		return nil, ErrBadExpr
	}
	elems, err := v.Document().Elements()
	if err != nil {
		return nil, err
	}
	out := make([]bson.RawValue, len(elems))
	for i, e := range elems {
		out[i] = e.Value
	}
	return out, nil
}

// splitPath splits a dotted path into components.
func splitPath(p string) []string {
	if p == "" {
		return nil
	}
	return strings.Split(p, ".")
}
