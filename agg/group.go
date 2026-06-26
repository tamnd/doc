package agg

import (
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/tamnd/doc/bson"
)

// ---- $group --------------------------------------------------------------

// compileGroup compiles a $group stage: a required _id grouping key and any
// number of accumulator fields (spec 2061 doc 12 §7).
func compileGroup(arg bson.RawValue) (stageSpec, error) {
	if arg.Type != bson.TypeDocument {
		return nil, ErrBadStage
	}
	elems, err := arg.Document().Elements()
	if err != nil {
		return nil, err
	}
	st := &groupStage{}
	for _, e := range elems {
		if e.Key == "_id" {
			ex, cerr := compileExpr(e.Value)
			if cerr != nil {
				return nil, cerr
			}
			st.keyExpr = ex
			continue
		}
		spec, cerr := compileAccumulator(e.Key, e.Value)
		if cerr != nil {
			return nil, cerr
		}
		st.accs = append(st.accs, spec)
	}
	if st.keyExpr == nil {
		return nil, ErrBadStage
	}
	return st, nil
}

type groupStage struct {
	keyExpr Expr
	accs    []accSpec
}

func (s *groupStage) open(in src, ec *execCtx) src {
	return &groupSrc{in: in, stage: s, ec: ec}
}

type groupSrc struct {
	in     src
	stage  *groupStage
	ec     *execCtx
	out    []bson.Raw
	i      int
	loaded bool
}

// group holds one group's emitted _id value and its live accumulator states.
type group struct {
	id    bson.RawValue
	accum []accState
}

func (s *groupSrc) next() (bson.Raw, error) {
	if !s.loaded {
		if err := s.load(); err != nil {
			return nil, err
		}
		s.loaded = true
	}
	if s.i >= len(s.out) {
		return nil, io.EOF
	}
	d := s.out[s.i]
	s.i++
	return d, nil
}

// load performs the hash aggregation: bucket every input document by its grouping
// key, update each group's accumulators, then materialize the output documents in
// first-seen group order (spec 2061 doc 12 §7.4).
func (s *groupSrc) load() error {
	groups := map[string]*group{}
	var order []string
	for {
		doc, err := s.in.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		ctx := docCtx(doc, s.ec)
		keyVal := s.stage.keyExpr.eval(ctx)
		if isMissing(keyVal) {
			keyVal = mkNull()
		}
		k := groupKey(keyVal)
		g, ok := groups[k]
		if !ok {
			g = &group{id: keyVal, accum: make([]accState, len(s.stage.accs))}
			for i, a := range s.stage.accs {
				g.accum[i] = a.make()
			}
			groups[k] = g
			order = append(order, k)
		}
		for _, a := range g.accum {
			if err := a.step(ctx); err != nil {
				return err
			}
		}
	}
	s.out = make([]bson.Raw, 0, len(order))
	for _, k := range order {
		g := groups[k]
		b := bson.NewBuilder().AppendValue("_id", g.id)
		for i := range s.stage.accs {
			v := g.accum[i].result()
			if !isMissing(v) {
				b.AppendValue(s.stage.accs[i].field, v)
			}
		}
		s.out = append(s.out, b.Build())
	}
	return nil
}

// groupKey builds a canonical string key for a grouping value. Numbers compare by
// value across int32/int64/double (1 and 1.0 group together, matching MongoDB);
// null and missing share a key; documents and arrays recurse.
func groupKey(v bson.RawValue) string {
	var b strings.Builder
	writeKey(&b, v)
	return b.String()
}

func writeKey(b *strings.Builder, v bson.RawValue) {
	switch v.Type {
	case 0, bson.TypeNull, bson.TypeUndefined:
		b.WriteByte('Z')
	case bson.TypeInt32, bson.TypeInt64, bson.TypeDouble:
		writeNumKey(b, v)
	case bson.TypeBoolean:
		if v.Boolean() {
			b.WriteString("B1")
		} else {
			b.WriteString("B0")
		}
	case bson.TypeString:
		b.WriteByte('S')
		b.WriteString(v.StringValue())
	case bson.TypeDocument:
		b.WriteByte('{')
		els, _ := v.Document().Elements()
		for _, e := range els {
			b.WriteString(e.Key)
			b.WriteByte('=')
			writeKey(b, e.Value)
			b.WriteByte(',')
		}
		b.WriteByte('}')
	case bson.TypeArray:
		b.WriteByte('[')
		els, _ := arrayElements(v)
		for _, e := range els {
			writeKey(b, e)
			b.WriteByte(',')
		}
		b.WriteByte(']')
	default:
		b.WriteByte('R')
		b.WriteByte(byte(v.Type))
		b.Write(v.Data)
	}
}

// writeNumKey canonicalizes a number: an integral value of any numeric type maps
// to the same key, so 1 (int32), 1 (int64), and 1.0 (double) coincide.
func writeNumKey(b *strings.Builder, v bson.RawValue) {
	i, f, k := numOf(v)
	if k == kindDouble {
		if f == math.Trunc(f) && f >= -9.2e18 && f <= 9.2e18 {
			b.WriteByte('I')
			b.WriteString(strconv.FormatInt(int64(f), 10))
			return
		}
		b.WriteByte('F')
		b.WriteString(strconv.FormatUint(math.Float64bits(f), 16))
		return
	}
	b.WriteByte('I')
	b.WriteString(strconv.FormatInt(i, 10))
}

// ---- accumulators --------------------------------------------------------

// accState is one group's running state for one accumulator field.
type accState interface {
	step(ctx *evalCtx) error
	result() bson.RawValue
}

// accSpec pairs an output field name with a factory making a fresh state per group.
type accSpec struct {
	field string
	make  func() accState
}

// compileAccumulator compiles {field: {accName: arg}} into an accSpec.
func compileAccumulator(field string, v bson.RawValue) (accSpec, error) {
	if v.Type != bson.TypeDocument {
		return accSpec{}, ErrBadStage
	}
	elems, err := v.Document().Elements()
	if err != nil {
		return accSpec{}, err
	}
	if len(elems) != 1 {
		return accSpec{}, ErrBadStage
	}
	name, arg := elems[0].Key, elems[0].Value
	mk, err := buildAccumulator(name, arg)
	if err != nil {
		return accSpec{}, err
	}
	return accSpec{field: field, make: mk}, nil
}

func buildAccumulator(name string, arg bson.RawValue) (func() accState, error) {
	switch name {
	case "$sum":
		return argAcc(arg, func(e Expr) accState { return &sumAcc{arg: e} })
	case "$avg":
		return argAcc(arg, func(e Expr) accState { return &avgAcc{arg: e} })
	case "$min":
		return argAcc(arg, func(e Expr) accState { return &minMaxAcc{arg: e, max: false} })
	case "$max":
		return argAcc(arg, func(e Expr) accState { return &minMaxAcc{arg: e, max: true} })
	case "$first":
		return argAcc(arg, func(e Expr) accState { return &firstLastAcc{arg: e, last: false} })
	case "$last":
		return argAcc(arg, func(e Expr) accState { return &firstLastAcc{arg: e, last: true} })
	case "$push":
		return argAcc(arg, func(e Expr) accState { return &pushAcc{arg: e} })
	case "$addToSet":
		return argAcc(arg, func(e Expr) accState { return &addToSetAcc{arg: e} })
	case "$mergeObjects":
		return argAcc(arg, func(e Expr) accState { return &mergeObjectsAcc{arg: e} })
	case "$stdDevPop":
		return argAcc(arg, func(e Expr) accState { return &stdDevAcc{arg: e, sample: false} })
	case "$stdDevSamp":
		return argAcc(arg, func(e Expr) accState { return &stdDevAcc{arg: e, sample: true} })
	case "$count":
		return func() accState { return &countAcc{} }, nil
	case "$top", "$bottom":
		return topBottomAcc(arg, name == "$bottom", false)
	case "$topN", "$bottomN":
		return topBottomAcc(arg, name == "$bottomN", true)
	case "$firstN", "$lastN":
		return firstLastNAcc(arg, name == "$lastN")
	case "$maxN", "$minN":
		return maxMinNAcc(arg, name == "$maxN")
	default:
		return nil, ErrBadStage
	}
}

// argAcc compiles a single-expression accumulator argument and returns a factory.
func argAcc(arg bson.RawValue, fn func(Expr) accState) (func() accState, error) {
	e, err := compileExpr(arg)
	if err != nil {
		return nil, err
	}
	return func() accState { return fn(e) }, nil
}

// sumAcc sums non-numeric-skipping values, widening int32→int64→double and using
// the float total only when a double has been seen (spec 2061 doc 12 §7.3).
type sumAcc struct {
	arg  Expr
	iSum int64
	fSum float64
	kind numKind
}

func (a *sumAcc) step(ctx *evalCtx) error {
	i, f, k := numOf(a.arg.eval(ctx))
	if k == kindNotNum {
		return nil
	}
	a.iSum += i
	a.fSum += f
	a.kind = widen(a.kind, k)
	return nil
}

func (a *sumAcc) result() bson.RawValue {
	if a.kind == kindDouble {
		return mkDouble(a.fSum)
	}
	return mkNum(a.iSum, a.fSum, a.kind)
}

// avgAcc averages non-null numeric values, returning null for an empty group.
type avgAcc struct {
	arg Expr
	sum float64
	n   int64
}

func (a *avgAcc) step(ctx *evalCtx) error {
	_, f, k := numOf(a.arg.eval(ctx))
	if k == kindNotNum {
		return nil
	}
	a.sum += f
	a.n++
	return nil
}

func (a *avgAcc) result() bson.RawValue {
	if a.n == 0 {
		return mkNull()
	}
	return mkDouble(a.sum / float64(a.n))
}

// minMaxAcc tracks the minimum or maximum by BSON order, ignoring missing values.
type minMaxAcc struct {
	arg  Expr
	max  bool
	best bson.RawValue
	set  bool
}

func (a *minMaxAcc) step(ctx *evalCtx) error {
	v := a.arg.eval(ctx)
	if isMissing(v) {
		return nil
	}
	if !a.set {
		a.best, a.set = v, true
		return nil
	}
	c := bson.Compare(v, a.best)
	if (a.max && c > 0) || (!a.max && c < 0) {
		a.best = v
	}
	return nil
}

func (a *minMaxAcc) result() bson.RawValue {
	if !a.set {
		return mkNull()
	}
	return a.best
}

// firstLastAcc keeps the first or last value in stream order.
type firstLastAcc struct {
	arg  Expr
	last bool
	val  bson.RawValue
	set  bool
}

func (a *firstLastAcc) step(ctx *evalCtx) error {
	if a.set && !a.last {
		return nil
	}
	a.val = a.arg.eval(ctx)
	a.set = true
	return nil
}

func (a *firstLastAcc) result() bson.RawValue {
	if !a.set {
		return mkNull()
	}
	return a.val
}

// pushAcc collects every non-missing value, including nulls and duplicates.
type pushAcc struct {
	arg  Expr
	vals []bson.RawValue
}

func (a *pushAcc) step(ctx *evalCtx) error {
	v := a.arg.eval(ctx)
	if !isMissing(v) {
		a.vals = append(a.vals, v)
	}
	return nil
}

func (a *pushAcc) result() bson.RawValue { return mkArray(a.vals) }

// addToSetAcc collects distinct non-missing values (set semantics, unordered).
type addToSetAcc struct {
	arg  Expr
	vals []bson.RawValue
}

func (a *addToSetAcc) step(ctx *evalCtx) error {
	v := a.arg.eval(ctx)
	if !isMissing(v) && !containsVal(a.vals, v) {
		a.vals = append(a.vals, v)
	}
	return nil
}

func (a *addToSetAcc) result() bson.RawValue { return mkArray(a.vals) }

// mergeObjectsAcc merges document values; later fields win. Null and missing are
// ignored; a non-document value is an error.
type mergeObjectsAcc struct {
	arg    Expr
	merged map[string]bson.RawValue
	order  []string
}

func (a *mergeObjectsAcc) step(ctx *evalCtx) error {
	v := a.arg.eval(ctx)
	if isNullish(v) {
		return nil
	}
	if v.Type != bson.TypeDocument {
		return ErrBadExpr
	}
	if a.merged == nil {
		a.merged = map[string]bson.RawValue{}
	}
	els, err := v.Document().Elements()
	if err != nil {
		return err
	}
	for _, e := range els {
		if _, seen := a.merged[e.Key]; !seen {
			a.order = append(a.order, e.Key)
		}
		a.merged[e.Key] = e.Value
	}
	return nil
}

func (a *mergeObjectsAcc) result() bson.RawValue {
	b := bson.NewBuilder()
	for _, k := range a.order {
		b.AppendValue(k, a.merged[k])
	}
	return mkDoc(b.Build())
}

// stdDevAcc computes population or sample standard deviation via Welford's
// online algorithm over numeric values (spec 2061 doc 12 §7.3).
type stdDevAcc struct {
	arg    Expr
	sample bool
	n      int64
	mean   float64
	m2     float64
}

func (a *stdDevAcc) step(ctx *evalCtx) error {
	_, f, k := numOf(a.arg.eval(ctx))
	if k == kindNotNum {
		return nil
	}
	a.n++
	delta := f - a.mean
	a.mean += delta / float64(a.n)
	a.m2 += delta * (f - a.mean)
	return nil
}

func (a *stdDevAcc) result() bson.RawValue {
	if a.sample {
		if a.n < 2 {
			return mkNull()
		}
		return mkDouble(math.Sqrt(a.m2 / float64(a.n-1)))
	}
	if a.n == 0 {
		return mkNull()
	}
	return mkDouble(math.Sqrt(a.m2 / float64(a.n)))
}

// countAcc counts documents in the group regardless of value.
type countAcc struct{ n int64 }

func (a *countAcc) step(_ *evalCtx) error { a.n++; return nil }

func (a *countAcc) result() bson.RawValue { return mkNum(a.n, float64(a.n), kindInt32) }

// ---- N / sort-based accumulators -----------------------------------------

// topBottomAccState holds one group's collected (doc, output) pairs for
// $top/$topN/$bottom/$bottomN: it keeps each document and its output value, then
// sorts by sortBy and slices the top or bottom N.
type topBottomAccState struct {
	spec      *sortSpec
	output    Expr
	nExpr     Expr
	bottom    bool
	withN     bool
	docs      []bson.Raw
	outs      []bson.RawValue
	n         int
	nResolved bool
}

func makeTopBottom(spec *sortSpec, output, nExpr Expr, bottom, withN bool) func() accState {
	return func() accState {
		return &topBottomAccState{spec: spec, output: output, nExpr: nExpr, bottom: bottom, withN: withN}
	}
}

func (a *topBottomAccState) step(ctx *evalCtx) error {
	if a.withN && !a.nResolved {
		n, ok := intArg(a.nExpr.eval(ctx))
		if !ok || n <= 0 {
			return ErrBadExpr
		}
		a.n, a.nResolved = n, true
	}
	a.docs = append(a.docs, ctx.root)
	a.outs = append(a.outs, a.output.eval(ctx))
	return nil
}

func (a *topBottomAccState) result() bson.RawValue {
	idx := make([]int, len(a.docs))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(i, j int) bool {
		return a.spec.compare(a.docs[idx[i]], a.docs[idx[j]]) < 0
	})
	if a.bottom {
		for i, j := 0, len(idx)-1; i < j; i, j = i+1, j-1 {
			idx[i], idx[j] = idx[j], idx[i]
		}
	}
	if !a.withN {
		if len(idx) == 0 {
			return mkNull()
		}
		return a.outs[idx[0]]
	}
	n := a.n
	if n > len(idx) {
		n = len(idx)
	}
	out := make([]bson.RawValue, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, a.outs[idx[i]])
	}
	return mkArray(out)
}

// firstLastNAccState backs $firstN/$lastN: the first or last N values in stream
// order.
type firstLastNAccState struct {
	input     Expr
	nExpr     Expr
	last      bool
	vals      []bson.RawValue
	n         int
	nResolved bool
}

func (a *firstLastNAccState) step(ctx *evalCtx) error {
	if !a.nResolved {
		n, ok := intArg(a.nExpr.eval(ctx))
		if !ok || n <= 0 {
			return ErrBadExpr
		}
		a.n, a.nResolved = n, true
	}
	a.vals = append(a.vals, a.input.eval(ctx))
	return nil
}

func (a *firstLastNAccState) result() bson.RawValue {
	vals := a.vals
	if a.last {
		if len(vals) > a.n {
			vals = vals[len(vals)-a.n:]
		}
	} else {
		if len(vals) > a.n {
			vals = vals[:a.n]
		}
	}
	out := make([]bson.RawValue, len(vals))
	copy(out, vals)
	return mkArray(out)
}

// maxMinNAccState backs $maxN/$minN: the N largest or smallest values by BSON
// order, ignoring null and missing.
type maxMinNAccState struct {
	input     Expr
	nExpr     Expr
	max       bool
	vals      []bson.RawValue
	n         int
	nResolved bool
}

func (a *maxMinNAccState) step(ctx *evalCtx) error {
	if !a.nResolved {
		n, ok := intArg(a.nExpr.eval(ctx))
		if !ok || n <= 0 {
			return ErrBadExpr
		}
		a.n, a.nResolved = n, true
	}
	v := a.input.eval(ctx)
	if !isNullish(v) {
		a.vals = append(a.vals, v)
	}
	return nil
}

func (a *maxMinNAccState) result() bson.RawValue {
	vals := make([]bson.RawValue, len(a.vals))
	copy(vals, a.vals)
	sort.SliceStable(vals, func(i, j int) bool {
		c := bson.Compare(vals[i], vals[j])
		if a.max {
			return c > 0
		}
		return c < 0
	})
	if len(vals) > a.n {
		vals = vals[:a.n]
	}
	return mkArray(vals)
}

// topBottomAcc compiles {sortBy, output[, n]} for $top/$topN/$bottom/$bottomN.
func topBottomAcc(arg bson.RawValue, bottom, withN bool) (func() accState, error) {
	if arg.Type != bson.TypeDocument {
		return nil, ErrBadStage
	}
	d := arg.Document()
	sbv, ok := d.Lookup("sortBy")
	if !ok || sbv.Type != bson.TypeDocument {
		return nil, ErrBadStage
	}
	spec, err := compileSortSpec(sbv.Document())
	if err != nil {
		return nil, err
	}
	ov, ok := d.Lookup("output")
	if !ok {
		return nil, ErrBadStage
	}
	output, err := compileExpr(ov)
	if err != nil {
		return nil, err
	}
	var nExpr Expr
	if withN {
		nv, nok := d.Lookup("n")
		if !nok {
			return nil, ErrBadStage
		}
		if nExpr, err = compileExpr(nv); err != nil {
			return nil, err
		}
	}
	return makeTopBottom(spec, output, nExpr, bottom, withN), nil
}

// firstLastNAcc compiles {input, n} for $firstN/$lastN.
func firstLastNAcc(arg bson.RawValue, last bool) (func() accState, error) {
	input, nExpr, err := inputN(arg)
	if err != nil {
		return nil, err
	}
	return func() accState {
		return &firstLastNAccState{input: input, nExpr: nExpr, last: last}
	}, nil
}

// maxMinNAcc compiles {input, n} for $maxN/$minN.
func maxMinNAcc(arg bson.RawValue, max bool) (func() accState, error) {
	input, nExpr, err := inputN(arg)
	if err != nil {
		return nil, err
	}
	return func() accState {
		return &maxMinNAccState{input: input, nExpr: nExpr, max: max}
	}, nil
}

// inputN compiles the shared {input, n} argument of the N value accumulators.
func inputN(arg bson.RawValue) (Expr, Expr, error) {
	if arg.Type != bson.TypeDocument {
		return nil, nil, ErrBadStage
	}
	d := arg.Document()
	iv, ok := d.Lookup("input")
	if !ok {
		return nil, nil, ErrBadStage
	}
	input, err := compileExpr(iv)
	if err != nil {
		return nil, nil, err
	}
	nv, ok := d.Lookup("n")
	if !ok {
		return nil, nil, ErrBadStage
	}
	nExpr, err := compileExpr(nv)
	if err != nil {
		return nil, nil, err
	}
	return input, nExpr, nil
}
