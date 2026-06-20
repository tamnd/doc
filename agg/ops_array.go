package agg

import (
	"sort"

	"github.com/tamnd/doc/bson"
)

// opSize returns the length of an array operand.
func opSize(vals []bson.RawValue) bson.RawValue {
	if vals[0].Type != bson.TypeArray {
		return mkNull()
	}
	elems, err := arrayElements(vals[0])
	if err != nil {
		return mkNull()
	}
	return mkInt32(int32(len(elems)))
}

// opArrayElemAt returns the element at an index, counting from the end when the
// index is negative; an out-of-range index yields the missing value.
func opArrayElemAt(vals []bson.RawValue) bson.RawValue {
	if isNullish(vals[0]) || isNullish(vals[1]) {
		return mkNull()
	}
	elems, err := arrayElements(vals[0])
	if err != nil {
		return mkNull()
	}
	idx, ok := intArg(vals[1])
	if !ok {
		return mkNull()
	}
	if idx < 0 {
		idx += len(elems)
	}
	if idx < 0 || idx >= len(elems) {
		return missing
	}
	return elems[idx]
}

// endElem builds $first/$last.
func endElem(last bool) opCompiler {
	return eager(1, 1, func(vals []bson.RawValue) bson.RawValue {
		if isNullish(vals[0]) {
			return mkNull()
		}
		elems, err := arrayElements(vals[0])
		if err != nil {
			return mkNull()
		}
		if len(elems) == 0 {
			return missing
		}
		if last {
			return elems[len(elems)-1]
		}
		return elems[0]
	})
}

// opConcatArrays appends its array operands; a nullish operand yields null.
func opConcatArrays(vals []bson.RawValue) bson.RawValue {
	var out []bson.RawValue
	for _, v := range vals {
		if isNullish(v) {
			return mkNull()
		}
		elems, err := arrayElements(v)
		if err != nil {
			return mkNull()
		}
		out = append(out, elems...)
	}
	return mkArray(out)
}

// opSlice returns a sub-array: [array, n] takes the first or last n; [array,
// position, n] takes n elements from position.
func opSlice(vals []bson.RawValue) bson.RawValue {
	if isNullish(vals[0]) {
		return mkNull()
	}
	elems, err := arrayElements(vals[0])
	if err != nil {
		return mkNull()
	}
	if len(vals) == 2 {
		n, ok := intArg(vals[1])
		if !ok {
			return mkNull()
		}
		return mkArray(sliceFirstLast(elems, n))
	}
	pos, pok := intArg(vals[1])
	n, nok := intArg(vals[2])
	if !pok || !nok || n < 0 {
		return mkNull()
	}
	if pos < 0 {
		pos += len(elems)
		if pos < 0 {
			pos = 0
		}
	}
	if pos >= len(elems) {
		return mkArray(nil)
	}
	end := pos + n
	if end > len(elems) {
		end = len(elems)
	}
	return mkArray(elems[pos:end])
}

func sliceFirstLast(elems []bson.RawValue, n int) []bson.RawValue {
	if n >= 0 {
		if n > len(elems) {
			n = len(elems)
		}
		return elems[:n]
	}
	n = -n
	if n > len(elems) {
		n = len(elems)
	}
	return elems[len(elems)-n:]
}

// opReverseArray returns the elements in reverse order.
func opReverseArray(vals []bson.RawValue) bson.RawValue {
	if isNullish(vals[0]) {
		return mkNull()
	}
	elems, err := arrayElements(vals[0])
	if err != nil {
		return mkNull()
	}
	out := make([]bson.RawValue, len(elems))
	for i, e := range elems {
		out[len(elems)-1-i] = e
	}
	return mkArray(out)
}

// opRange builds an integer array from start to end by step (default 1).
func opRange(vals []bson.RawValue) bson.RawValue {
	start, sok := intArg(vals[0])
	end, eok := intArg(vals[1])
	if !sok || !eok {
		return mkNull()
	}
	step := 1
	if len(vals) == 3 {
		s, ok := intArg(vals[2])
		if !ok || s == 0 {
			return mkNull()
		}
		step = s
	}
	var out []bson.RawValue
	if step > 0 {
		for i := start; i < end; i += step {
			out = append(out, mkInt32(int32(i)))
		}
	} else {
		for i := start; i > end; i += step {
			out = append(out, mkInt32(int32(i)))
		}
	}
	return mkArray(out)
}

// opIn reports whether a value is a member of an array, by BSON equality.
func opIn(vals []bson.RawValue) bson.RawValue {
	if vals[1].Type != bson.TypeArray {
		return mkNull()
	}
	elems, err := arrayElements(vals[1])
	if err != nil {
		return mkNull()
	}
	for _, e := range elems {
		if bson.Equal(vals[0], e) {
			return mkBool(true)
		}
	}
	return mkBool(false)
}

// opIndexOfArray returns the first index of a value within an optional [start,
// end) window, or -1.
func opIndexOfArray(vals []bson.RawValue) bson.RawValue {
	if isNullish(vals[0]) {
		return mkNull()
	}
	elems, err := arrayElements(vals[0])
	if err != nil {
		return mkNull()
	}
	start := 0
	if len(vals) >= 3 {
		if v, ok := intArg(vals[2]); ok {
			start = v
		}
	}
	end := len(elems)
	if len(vals) == 4 {
		if v, ok := intArg(vals[3]); ok && v < end {
			end = v
		}
	}
	if start < 0 {
		start = 0
	}
	for i := start; i < end && i < len(elems); i++ {
		if bson.Equal(elems[i], vals[1]) {
			return mkInt32(int32(i))
		}
	}
	return mkInt32(-1)
}

// opIsArray reports whether the operand is an array.
func opIsArray(vals []bson.RawValue) bson.RawValue {
	return mkBool(vals[0].Type == bson.TypeArray)
}

// opArrayToObject turns [{k,v},...] or [[k,v],...] into a document.
func opArrayToObject(vals []bson.RawValue) bson.RawValue {
	if isNullish(vals[0]) {
		return mkNull()
	}
	elems, err := arrayElements(vals[0])
	if err != nil {
		return mkNull()
	}
	b := bson.NewBuilder()
	for _, el := range elems {
		k, v, ok := pairKV(el)
		if !ok {
			return mkNull()
		}
		b.AppendValue(k, v)
	}
	return mkDoc(b.Build())
}

// pairKV reads a {k,v} document or a [k, v] array element.
func pairKV(el bson.RawValue) (string, bson.RawValue, bool) {
	switch el.Type {
	case bson.TypeDocument:
		d := el.Document()
		kv, ok1 := d.Lookup("k")
		vv, ok2 := d.Lookup("v")
		if !ok1 || !ok2 {
			return "", missing, false
		}
		ks, ok := strOf(kv)
		if !ok {
			return "", missing, false
		}
		return ks, vv, true
	case bson.TypeArray:
		parts, err := arrayElements(el)
		if err != nil || len(parts) != 2 {
			return "", missing, false
		}
		ks, ok := strOf(parts[0])
		if !ok {
			return "", missing, false
		}
		return ks, parts[1], true
	default:
		return "", missing, false
	}
}

// opObjectToArray turns a document into [{k,v},...] in field order.
func opObjectToArray(vals []bson.RawValue) bson.RawValue {
	if isNullish(vals[0]) {
		return mkNull()
	}
	if vals[0].Type != bson.TypeDocument {
		return mkNull()
	}
	elems, err := vals[0].Document().Elements()
	if err != nil {
		return mkNull()
	}
	out := make([]bson.RawValue, 0, len(elems))
	for _, e := range elems {
		out = append(out, mkDoc(bson.NewBuilder().
			AppendString("k", e.Key).
			AppendValue("v", e.Value).
			Build()))
	}
	return mkArray(out)
}

// ---- binding operators ($filter, $map, $reduce, $let) --------------------

// compileLet binds {vars} then evaluates {in} in the extended environment.
func compileLet(arg bson.RawValue) (Expr, error) {
	if arg.Type != bson.TypeDocument {
		return nil, ErrBadExpr
	}
	d := arg.Document()
	vv, ok1 := d.Lookup("vars")
	inv, ok2 := d.Lookup("in")
	if !ok1 || !ok2 || vv.Type != bson.TypeDocument {
		return nil, ErrBadExpr
	}
	velems, err := vv.Document().Elements()
	if err != nil {
		return nil, err
	}
	names := make([]string, len(velems))
	exprs := make([]Expr, len(velems))
	for i, e := range velems {
		ce, cerr := compileExpr(e.Value)
		if cerr != nil {
			return nil, cerr
		}
		names[i] = e.Key
		exprs[i] = ce
	}
	ine, err := compileExpr(inv)
	if err != nil {
		return nil, err
	}
	return letExpr{names: names, exprs: exprs, in: ine}, nil
}

type letExpr struct {
	names []string
	exprs []Expr
	in    Expr
}

func (e letExpr) eval(c *evalCtx) bson.RawValue {
	cc := c
	for i, n := range e.names {
		cc = cc.withVar(n, e.exprs[i].eval(c))
	}
	return e.in.eval(cc)
}

// compileFilter compiles $filter {input, as?, cond, limit?}.
func compileFilter(arg bson.RawValue) (Expr, error) {
	if arg.Type != bson.TypeDocument {
		return nil, ErrBadExpr
	}
	d := arg.Document()
	inv, ok1 := d.Lookup("input")
	cv, ok2 := d.Lookup("cond")
	if !ok1 || !ok2 {
		return nil, ErrBadExpr
	}
	ine, err := compileExpr(inv)
	if err != nil {
		return nil, err
	}
	conde, err := compileExpr(cv)
	if err != nil {
		return nil, err
	}
	as := "this"
	if av, ok := d.Lookup("as"); ok {
		if s, sok := strOf(av); sok && s != "" {
			as = s
		}
	}
	var limitE Expr
	if lv, ok := d.Lookup("limit"); ok {
		limitE, err = compileExpr(lv)
		if err != nil {
			return nil, err
		}
	}
	return filterExpr{input: ine, as: as, cond: conde, limit: limitE}, nil
}

type filterExpr struct {
	input Expr
	as    string
	cond  Expr
	limit Expr
}

func (e filterExpr) eval(c *evalCtx) bson.RawValue {
	iv := e.input.eval(c)
	if isNullish(iv) {
		return mkNull()
	}
	elems, err := arrayElements(iv)
	if err != nil {
		return mkNull()
	}
	limit := -1
	if e.limit != nil {
		if n, ok := intArg(e.limit.eval(c)); ok {
			limit = n
		}
	}
	var out []bson.RawValue
	for _, el := range elems {
		if truthy(e.cond.eval(c.withVar(e.as, el))) {
			out = append(out, el)
			if limit >= 0 && len(out) >= limit {
				break
			}
		}
	}
	return mkArray(out)
}

// compileMap compiles $map {input, as?, in}.
func compileMap(arg bson.RawValue) (Expr, error) {
	if arg.Type != bson.TypeDocument {
		return nil, ErrBadExpr
	}
	d := arg.Document()
	inv, ok1 := d.Lookup("input")
	inExpr, ok2 := d.Lookup("in")
	if !ok1 || !ok2 {
		return nil, ErrBadExpr
	}
	ine, err := compileExpr(inv)
	if err != nil {
		return nil, err
	}
	bodyE, err := compileExpr(inExpr)
	if err != nil {
		return nil, err
	}
	as := "this"
	if av, ok := d.Lookup("as"); ok {
		if s, sok := strOf(av); sok && s != "" {
			as = s
		}
	}
	return mapExpr{input: ine, as: as, body: bodyE}, nil
}

type mapExpr struct {
	input Expr
	as    string
	body  Expr
}

func (e mapExpr) eval(c *evalCtx) bson.RawValue {
	iv := e.input.eval(c)
	if isNullish(iv) {
		return mkNull()
	}
	elems, err := arrayElements(iv)
	if err != nil {
		return mkNull()
	}
	out := make([]bson.RawValue, len(elems))
	for i, el := range elems {
		v := e.body.eval(c.withVar(e.as, el))
		if isMissing(v) {
			v = mkNull()
		}
		out[i] = v
	}
	return mkArray(out)
}

// compileReduce compiles $reduce {input, initialValue, in}, binding $$value and
// $$this each step.
func compileReduce(arg bson.RawValue) (Expr, error) {
	if arg.Type != bson.TypeDocument {
		return nil, ErrBadExpr
	}
	d := arg.Document()
	inv, ok1 := d.Lookup("input")
	initv, ok2 := d.Lookup("initialValue")
	inExpr, ok3 := d.Lookup("in")
	if !ok1 || !ok2 || !ok3 {
		return nil, ErrBadExpr
	}
	ine, err := compileExpr(inv)
	if err != nil {
		return nil, err
	}
	inite, err := compileExpr(initv)
	if err != nil {
		return nil, err
	}
	bodyE, err := compileExpr(inExpr)
	if err != nil {
		return nil, err
	}
	return reduceExpr{input: ine, init: inite, body: bodyE}, nil
}

type reduceExpr struct {
	input, init, body Expr
}

func (e reduceExpr) eval(c *evalCtx) bson.RawValue {
	iv := e.input.eval(c)
	if isNullish(iv) {
		return mkNull()
	}
	elems, err := arrayElements(iv)
	if err != nil {
		return mkNull()
	}
	acc := e.init.eval(c)
	for _, el := range elems {
		cc := c.withVar("value", acc).withVar("this", el)
		acc = e.body.eval(cc)
	}
	return acc
}

// compileSortArray compiles $sortArray {input, sortBy}.
func compileSortArray(arg bson.RawValue) (Expr, error) {
	if arg.Type != bson.TypeDocument {
		return nil, ErrBadExpr
	}
	d := arg.Document()
	inv, ok1 := d.Lookup("input")
	sv, ok2 := d.Lookup("sortBy")
	if !ok1 || !ok2 {
		return nil, ErrBadExpr
	}
	ine, err := compileExpr(inv)
	if err != nil {
		return nil, err
	}
	return sortArrayExpr{input: ine, sortBy: sv}, nil
}

type sortArrayExpr struct {
	input  Expr
	sortBy bson.RawValue
}

func (e sortArrayExpr) eval(c *evalCtx) bson.RawValue {
	iv := e.input.eval(c)
	if isNullish(iv) {
		return mkNull()
	}
	elems, err := arrayElements(iv)
	if err != nil {
		return mkNull()
	}
	out := make([]bson.RawValue, len(elems))
	copy(out, elems)
	keys := sortArrayKeys(e.sortBy)
	sort.SliceStable(out, func(i, j int) bool {
		return sortArrayLess(out[i], out[j], keys)
	})
	return mkArray(out)
}

// sortArrayKey is one sort term: an empty path means sort whole values.
type sortArrayKey struct {
	path []string
	desc bool
}

// sortArrayKeys reads the sortBy spec: 1 or -1 sorts whole values, a document
// names per-field directions.
func sortArrayKeys(spec bson.RawValue) []sortArrayKey {
	if spec.Type == bson.TypeDocument {
		elems, err := spec.Document().Elements()
		if err == nil {
			keys := make([]sortArrayKey, 0, len(elems))
			for _, e := range elems {
				dir, _, _ := numOf(e.Value)
				keys = append(keys, sortArrayKey{path: splitPath(e.Key), desc: dir < 0})
			}
			return keys
		}
	}
	dir, _, _ := numOf(spec)
	return []sortArrayKey{{desc: dir < 0}}
}

func sortArrayLess(a, b bson.RawValue, keys []sortArrayKey) bool {
	for _, k := range keys {
		av, bv := a, b
		if len(k.path) > 0 {
			av = resolvePath(a, k.path)
			bv = resolvePath(b, k.path)
		}
		c := bson.Compare(cmpVal(av), cmpVal(bv))
		if c == 0 {
			continue
		}
		if k.desc {
			return c > 0
		}
		return c < 0
	}
	return false
}

// compileZip compiles $zip {inputs, useLongestLength?, defaults?}.
func compileZip(arg bson.RawValue) (Expr, error) {
	if arg.Type != bson.TypeDocument {
		return nil, ErrBadExpr
	}
	d := arg.Document()
	iv, ok := d.Lookup("inputs")
	if !ok {
		return nil, ErrBadExpr
	}
	ine, err := compileExpr(iv)
	if err != nil {
		return nil, err
	}
	z := zipExpr{inputs: ine}
	if lv, ok := d.Lookup("useLongestLength"); ok {
		z.longest = truthy(lv)
	}
	if dv, ok := d.Lookup("defaults"); ok {
		z.defaults, err = compileExpr(dv)
		if err != nil {
			return nil, err
		}
	}
	return z, nil
}

type zipExpr struct {
	inputs   Expr
	longest  bool
	defaults Expr
}

func (e zipExpr) eval(c *evalCtx) bson.RawValue {
	iv := e.inputs.eval(c)
	if isNullish(iv) {
		return mkNull()
	}
	rows, err := arrayElements(iv)
	if err != nil {
		return mkNull()
	}
	cols := make([][]bson.RawValue, len(rows))
	maxLen, minLen := 0, -1
	for i, r := range rows {
		if isNullish(r) {
			return mkNull()
		}
		els, eerr := arrayElements(r)
		if eerr != nil {
			return mkNull()
		}
		cols[i] = els
		if len(els) > maxLen {
			maxLen = len(els)
		}
		if minLen < 0 || len(els) < minLen {
			minLen = len(els)
		}
	}
	n := minLen
	if e.longest {
		n = maxLen
	}
	if n < 0 {
		n = 0
	}
	var defs []bson.RawValue
	if e.defaults != nil {
		if dv := e.defaults.eval(c); dv.Type == bson.TypeArray {
			defs, _ = arrayElements(dv)
		}
	}
	out := make([]bson.RawValue, n)
	for j := 0; j < n; j++ {
		tuple := make([]bson.RawValue, len(cols))
		for i, col := range cols {
			if j < len(col) {
				tuple[i] = col[j]
			} else if i < len(defs) {
				tuple[i] = defs[i]
			} else {
				tuple[i] = mkNull()
			}
		}
		out[j] = mkArray(tuple)
	}
	return mkArray(out)
}
