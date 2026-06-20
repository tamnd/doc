package agg

import (
	"math/rand"

	"github.com/tamnd/doc/bson"
)

// compileLiteral compiles $literal, which returns its argument verbatim with no
// further evaluation (spec 2061 doc 12 §3.4).
func compileLiteral(arg bson.RawValue) (Expr, error) {
	return constExpr{v: arg}, nil
}

// opMergeObjects merges document operands left to right; later fields win and
// nullish operands are skipped.
func opMergeObjects(vals []bson.RawValue) bson.RawValue {
	merged := map[string]bson.RawValue{}
	var order []string
	for _, v := range vals {
		if isNullish(v) {
			continue
		}
		if v.Type != bson.TypeDocument {
			return mkNull()
		}
		elems, err := v.Document().Elements()
		if err != nil {
			return mkNull()
		}
		for _, e := range elems {
			if _, seen := merged[e.Key]; !seen {
				order = append(order, e.Key)
			}
			merged[e.Key] = e.Value
		}
	}
	b := bson.NewBuilder()
	for _, k := range order {
		b.AppendValue(k, merged[k])
	}
	return mkDoc(b.Build())
}

// compileGetField compiles $getField: either {field, input} or a bare field name
// resolved against $$CURRENT.
func compileGetField(arg bson.RawValue) (Expr, error) {
	if arg.Type == bson.TypeDocument {
		if _, ok := arg.Document().Lookup("field"); ok {
			d := arg.Document()
			fv, _ := d.Lookup("field")
			fe, err := compileExpr(fv)
			if err != nil {
				return nil, err
			}
			var ine Expr
			if iv, ok := d.Lookup("input"); ok {
				ine, err = compileExpr(iv)
				if err != nil {
					return nil, err
				}
			}
			return getFieldExpr{field: fe, input: ine}, nil
		}
	}
	fe, err := compileExpr(arg)
	if err != nil {
		return nil, err
	}
	return getFieldExpr{field: fe}, nil
}

type getFieldExpr struct {
	field Expr
	input Expr // nil means $$CURRENT
}

func (e getFieldExpr) eval(c *evalCtx) bson.RawValue {
	name, ok := strOf(e.field.eval(c))
	if !ok {
		return missing
	}
	doc := c.cur
	if e.input != nil {
		doc = e.input.eval(c)
	}
	if doc.Type != bson.TypeDocument {
		return missing
	}
	v, found := doc.Document().Lookup(name)
	if !found {
		return missing
	}
	return v
}

// compileSetField compiles $setField {field, input, value}; a value of $$REMOVE
// deletes the field.
func compileSetField(arg bson.RawValue) (Expr, error) {
	if arg.Type != bson.TypeDocument {
		return nil, ErrBadExpr
	}
	d := arg.Document()
	fv, ok1 := d.Lookup("field")
	iv, ok2 := d.Lookup("input")
	vv, ok3 := d.Lookup("value")
	if !ok1 || !ok2 || !ok3 {
		return nil, ErrBadExpr
	}
	fe, err := compileExpr(fv)
	if err != nil {
		return nil, err
	}
	ine, err := compileExpr(iv)
	if err != nil {
		return nil, err
	}
	ve, err := compileExpr(vv)
	if err != nil {
		return nil, err
	}
	return setFieldExpr{field: fe, input: ine, value: ve}, nil
}

type setFieldExpr struct {
	field, input, value Expr
}

func (e setFieldExpr) eval(c *evalCtx) bson.RawValue {
	name, ok := strOf(e.field.eval(c))
	if !ok {
		return missing
	}
	in := e.input.eval(c)
	if isNullish(in) {
		in = mkDoc(bson.NewBuilder().Build())
	}
	if in.Type != bson.TypeDocument {
		return missing
	}
	newVal := e.value.eval(c)
	remove := isMissing(newVal)
	elems, err := in.Document().Elements()
	if err != nil {
		return missing
	}
	b := bson.NewBuilder()
	replaced := false
	for _, el := range elems {
		if el.Key == name {
			if remove {
				continue
			}
			b.AppendValue(name, newVal)
			replaced = true
			continue
		}
		b.AppendValue(el.Key, el.Value)
	}
	if !replaced && !remove {
		b.AppendValue(name, newVal)
	}
	return mkDoc(b.Build())
}

// opRand returns a uniform random double in [0, 1).
func opRand(vals []bson.RawValue) bson.RawValue {
	return mkDouble(rand.Float64())
}
