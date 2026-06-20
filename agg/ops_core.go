package agg

import (
	"math"

	"github.com/tamnd/doc/bson"
)

// opCompiler compiles one operator's argument into an Expr.
type opCompiler func(arg bson.RawValue) (Expr, error)

// eagerExpr evaluates its operands first, then hands the values to fn. It is the
// shape of every operator that does not short-circuit.
type eagerExpr struct {
	args []Expr
	fn   func(vals []bson.RawValue) bson.RawValue
}

func (e eagerExpr) eval(c *evalCtx) bson.RawValue {
	vals := make([]bson.RawValue, len(e.args))
	for i, a := range e.args {
		vals[i] = a.eval(c)
	}
	return e.fn(vals)
}

// eager builds an opCompiler for an eager operator accepting between min and max
// operands (max < 0 means unbounded).
func eager(min, max int, fn func(vals []bson.RawValue) bson.RawValue) opCompiler {
	return func(arg bson.RawValue) (Expr, error) {
		args, err := compileArgs(arg)
		if err != nil {
			return nil, err
		}
		if len(args) < min || (max >= 0 && len(args) > max) {
			return nil, ErrBadExpr
		}
		return eagerExpr{args: args, fn: fn}, nil
	}
}

// ---- arithmetic ----------------------------------------------------------

// opAdd sums numbers; a single date operand shifts by the summed milliseconds.
func opAdd(vals []bson.RawValue) bson.RawValue {
	var i int64
	var f float64
	k := kindInt32
	hasDate := false
	var dateMs int64
	for _, v := range vals {
		if isNullish(v) {
			return mkNull()
		}
		if v.Type == bson.TypeDateTime {
			if hasDate {
				return mkNull()
			}
			hasDate, dateMs = true, v.DateTime()
			continue
		}
		iv, fv, kk := numOf(v)
		if kk == kindNotNum {
			return mkNull()
		}
		k = widen(k, kk)
		i += iv
		f += fv
	}
	if hasDate {
		add := i
		if k == kindDouble {
			add = int64(f)
		}
		return mkDate(dateMs + add)
	}
	return mkNum(i, f, k)
}

// opSubtract handles number-number, date-date (ms), and date-number.
func opSubtract(vals []bson.RawValue) bson.RawValue {
	a, b := vals[0], vals[1]
	if isNullish(a) || isNullish(b) {
		return mkNull()
	}
	if a.Type == bson.TypeDateTime {
		if b.Type == bson.TypeDateTime {
			return mkInt64(a.DateTime() - b.DateTime())
		}
		bi, bf, bk := numOf(b)
		if bk == kindNotNum {
			return mkNull()
		}
		sub := bi
		if bk == kindDouble {
			sub = int64(bf)
		}
		return mkDate(a.DateTime() - sub)
	}
	ai, af, ak := numOf(a)
	bi, bf, bk := numOf(b)
	if ak == kindNotNum || bk == kindNotNum {
		return mkNull()
	}
	return mkNum(ai-bi, af-bf, widen(ak, bk))
}

// opMultiply returns the product of its numeric operands.
func opMultiply(vals []bson.RawValue) bson.RawValue {
	i := int64(1)
	f := 1.0
	k := kindInt32
	for _, v := range vals {
		if isNullish(v) {
			return mkNull()
		}
		iv, fv, kk := numOf(v)
		if kk == kindNotNum {
			return mkNull()
		}
		k = widen(k, kk)
		i *= iv
		f *= fv
	}
	return mkNum(i, f, k)
}

// opDivide returns a/b as a double; division by zero yields null.
func opDivide(vals []bson.RawValue) bson.RawValue {
	a, b := vals[0], vals[1]
	if isNullish(a) || isNullish(b) {
		return mkNull()
	}
	af, aok := a.AsFloat64()
	bf, bok := b.AsFloat64()
	if !aok || !bok || bf == 0 {
		return mkNull()
	}
	return mkDouble(af / bf)
}

// opMod returns the truncated-division remainder.
func opMod(vals []bson.RawValue) bson.RawValue {
	a, b := vals[0], vals[1]
	if isNullish(a) || isNullish(b) {
		return mkNull()
	}
	ai, af, ak := numOf(a)
	bi, bf, bk := numOf(b)
	if ak == kindNotNum || bk == kindNotNum {
		return mkNull()
	}
	if ak != kindDouble && bk != kindDouble {
		if bi == 0 {
			return mkNull()
		}
		return mkNum(ai%bi, 0, widen(ak, bk))
	}
	if bf == 0 {
		return mkNull()
	}
	return mkDouble(math.Mod(af, bf))
}

// unaryNum builds an eager unary operator over one numeric operand, returning null
// for a nullish or non-numeric input.
func unaryNum(fn func(i int64, f float64, k numKind) bson.RawValue) opCompiler {
	return eager(1, 1, func(vals []bson.RawValue) bson.RawValue {
		v := vals[0]
		if isNullish(v) {
			return mkNull()
		}
		i, f, k := numOf(v)
		if k == kindNotNum {
			return mkNull()
		}
		return fn(i, f, k)
	})
}

// opAbs, opCeil, opFloor preserve integer types and act on doubles directly.
func opAbs(i int64, f float64, k numKind) bson.RawValue {
	if k == kindDouble {
		return mkDouble(math.Abs(f))
	}
	if i < 0 {
		i = -i
	}
	return mkNum(i, 0, k)
}

func opCeil(i int64, f float64, k numKind) bson.RawValue {
	if k == kindDouble {
		return mkDouble(math.Ceil(f))
	}
	return mkNum(i, 0, k)
}

func opFloor(i int64, f float64, k numKind) bson.RawValue {
	if k == kindDouble {
		return mkDouble(math.Floor(f))
	}
	return mkNum(i, 0, k)
}

// roundTrunc builds $round (banker's rounding) or $trunc (toward zero), each
// accepting an optional decimal-place operand.
func roundTrunc(banker bool) opCompiler {
	return eager(1, 2, func(vals []bson.RawValue) bson.RawValue {
		v := vals[0]
		if isNullish(v) {
			return mkNull()
		}
		i, f, k := numOf(v)
		if k == kindNotNum {
			return mkNull()
		}
		place := 0
		if len(vals) == 2 {
			pi, _, pk := numOf(vals[1])
			if pk == kindNotNum {
				return mkNull()
			}
			place = int(pi)
		}
		if k != kindDouble {
			return mkNum(i, 0, k)
		}
		scale := math.Pow(10, float64(place))
		x := f * scale
		if banker {
			x = math.RoundToEven(x)
		} else {
			x = math.Trunc(x)
		}
		return mkDouble(x / scale)
	})
}

// opPow, opSqrt, opLn, opLog, opLog10, opExp return doubles.
func opPow(vals []bson.RawValue) bson.RawValue {
	if isNullish(vals[0]) || isNullish(vals[1]) {
		return mkNull()
	}
	a, aok := vals[0].AsFloat64()
	b, bok := vals[1].AsFloat64()
	if !aok || !bok {
		return mkNull()
	}
	return mkDouble(math.Pow(a, b))
}

// unaryFloat builds an eager unary operator producing a double from a numeric
// input through fn.
func unaryFloat(fn func(float64) float64) opCompiler {
	return eager(1, 1, func(vals []bson.RawValue) bson.RawValue {
		if isNullish(vals[0]) {
			return mkNull()
		}
		x, ok := vals[0].AsFloat64()
		if !ok {
			return mkNull()
		}
		return mkDouble(fn(x))
	})
}

func opLog(vals []bson.RawValue) bson.RawValue {
	if isNullish(vals[0]) || isNullish(vals[1]) {
		return mkNull()
	}
	x, xok := vals[0].AsFloat64()
	base, bok := vals[1].AsFloat64()
	if !xok || !bok {
		return mkNull()
	}
	return mkDouble(math.Log(x) / math.Log(base))
}

// ---- comparison ----------------------------------------------------------

// cmpVal normalizes a missing value to null so comparison operators treat an
// absent field like null in BSON order, matching MongoDB.
func cmpVal(v bson.RawValue) bson.RawValue {
	if isMissing(v) {
		return mkNull()
	}
	return v
}

// compareOp builds a comparison operator whose boolean result is keep(compare).
func compareOp(keep func(c int) bool) opCompiler {
	return eager(2, 2, func(vals []bson.RawValue) bson.RawValue {
		return mkBool(keep(bson.Compare(cmpVal(vals[0]), cmpVal(vals[1]))))
	})
}

// opCmp returns -1, 0, or 1 from the BSON comparison.
func opCmp(vals []bson.RawValue) bson.RawValue {
	c := bson.Compare(cmpVal(vals[0]), cmpVal(vals[1]))
	switch {
	case c < 0:
		return mkInt32(-1)
	case c > 0:
		return mkInt32(1)
	default:
		return mkInt32(0)
	}
}

// ---- boolean (short-circuit) ---------------------------------------------

type andExpr struct{ args []Expr }

func (e andExpr) eval(c *evalCtx) bson.RawValue {
	for _, a := range e.args {
		if !truthy(a.eval(c)) {
			return mkBool(false)
		}
	}
	return mkBool(true)
}

type orExpr struct{ args []Expr }

func (e orExpr) eval(c *evalCtx) bson.RawValue {
	for _, a := range e.args {
		if truthy(a.eval(c)) {
			return mkBool(true)
		}
	}
	return mkBool(false)
}

// boolCompiler compiles $and/$or, which accept an operand list.
func boolCompiler(and bool) opCompiler {
	return func(arg bson.RawValue) (Expr, error) {
		args, err := compileArgs(arg)
		if err != nil {
			return nil, err
		}
		if and {
			return andExpr{args: args}, nil
		}
		return orExpr{args: args}, nil
	}
}

// opNot inverts truthiness; a single operand, in array or scalar form.
func opNot(vals []bson.RawValue) bson.RawValue {
	return mkBool(!truthy(vals[0]))
}

// ---- conditional (short-circuit) -----------------------------------------

type condExpr struct{ cond, then, els Expr }

func (e condExpr) eval(c *evalCtx) bson.RawValue {
	if truthy(e.cond.eval(c)) {
		return e.then.eval(c)
	}
	return e.els.eval(c)
}

// compileCond compiles $cond in both the object and the three-element array form.
func compileCond(arg bson.RawValue) (Expr, error) {
	if arg.Type == bson.TypeArray {
		elems, err := arrayElements(arg)
		if err != nil {
			return nil, err
		}
		if len(elems) != 3 {
			return nil, ErrBadExpr
		}
		return compileCondParts(elems[0], elems[1], elems[2])
	}
	if arg.Type != bson.TypeDocument {
		return nil, ErrBadExpr
	}
	d := arg.Document()
	ifv, ok1 := d.Lookup("if")
	thenv, ok2 := d.Lookup("then")
	elsev, ok3 := d.Lookup("else")
	if !ok1 || !ok2 || !ok3 {
		return nil, ErrBadExpr
	}
	return compileCondParts(ifv, thenv, elsev)
}

func compileCondParts(ifv, thenv, elsev bson.RawValue) (Expr, error) {
	ce, err := compileExpr(ifv)
	if err != nil {
		return nil, err
	}
	te, err := compileExpr(thenv)
	if err != nil {
		return nil, err
	}
	ee, err := compileExpr(elsev)
	if err != nil {
		return nil, err
	}
	return condExpr{cond: ce, then: te, els: ee}, nil
}

type ifNullExpr struct{ args []Expr }

func (e ifNullExpr) eval(c *evalCtx) bson.RawValue {
	for i, a := range e.args {
		v := a.eval(c)
		if i == len(e.args)-1 {
			return v // the final operand is the default, returned as-is
		}
		if !isNullish(v) {
			return v
		}
	}
	return mkNull()
}

func compileIfNull(arg bson.RawValue) (Expr, error) {
	args, err := compileArgs(arg)
	if err != nil {
		return nil, err
	}
	if len(args) < 2 {
		return nil, ErrBadExpr
	}
	return ifNullExpr{args: args}, nil
}

type switchBranch struct{ when, then Expr }

type switchExpr struct {
	branches []switchBranch
	def      Expr // may be nil
}

func (e switchExpr) eval(c *evalCtx) bson.RawValue {
	for _, br := range e.branches {
		if truthy(br.when.eval(c)) {
			return br.then.eval(c)
		}
	}
	if e.def != nil {
		return e.def.eval(c)
	}
	return missing
}

func compileSwitch(arg bson.RawValue) (Expr, error) {
	if arg.Type != bson.TypeDocument {
		return nil, ErrBadExpr
	}
	d := arg.Document()
	brv, ok := d.Lookup("branches")
	if !ok || brv.Type != bson.TypeArray {
		return nil, ErrBadExpr
	}
	elems, err := arrayElements(brv)
	if err != nil {
		return nil, err
	}
	var branches []switchBranch
	for _, el := range elems {
		if el.Type != bson.TypeDocument {
			return nil, ErrBadExpr
		}
		bd := el.Document()
		cv, ok1 := bd.Lookup("case")
		tv, ok2 := bd.Lookup("then")
		if !ok1 || !ok2 {
			return nil, ErrBadExpr
		}
		ce, cerr := compileExpr(cv)
		if cerr != nil {
			return nil, cerr
		}
		te, terr := compileExpr(tv)
		if terr != nil {
			return nil, terr
		}
		branches = append(branches, switchBranch{when: ce, then: te})
	}
	var def Expr
	if dv, ok := d.Lookup("default"); ok {
		def, err = compileExpr(dv)
		if err != nil {
			return nil, err
		}
	}
	return switchExpr{branches: branches, def: def}, nil
}
