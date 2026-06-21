package agg

import (
	"io"
	"math"
	"sort"

	"github.com/tamnd/doc/bson"
)

// ---- $sortByCount --------------------------------------------------------

// compileSortByCount desugars {$sortByCount: <expr>} into a $group counting each
// distinct value followed by a descending $sort on the count (spec 2061 doc 12
// §14.3).
func compileSortByCount(arg bson.RawValue) (stageSpec, error) {
	groupDoc := bson.NewBuilder().
		AppendValue("_id", arg).
		AppendDocument("count", bson.NewBuilder().AppendInt32("$sum", 1).Build()).
		Build()
	g, err := compileGroup(mkDoc(groupDoc))
	if err != nil {
		return nil, err
	}
	sortDoc := bson.NewBuilder().AppendInt32("count", -1).Build()
	s, err := compileSort(mkDoc(sortDoc))
	if err != nil {
		return nil, err
	}
	return &multiStage{stages: []stageSpec{g, s}}, nil
}

// ---- $bucket -------------------------------------------------------------

// compileBucket compiles a manual histogram stage (spec 2061 doc 12 §14.1).
func compileBucket(arg bson.RawValue) (stageSpec, error) {
	if arg.Type != bson.TypeDocument {
		return nil, ErrBadStage
	}
	d := arg.Document()
	gbv, ok := d.Lookup("groupBy")
	if !ok {
		return nil, ErrBadStage
	}
	groupBy, err := compileExpr(gbv)
	if err != nil {
		return nil, err
	}
	bv, ok := d.Lookup("boundaries")
	if !ok || bv.Type != bson.TypeArray {
		return nil, ErrBadStage
	}
	bounds, err := arrayElements(bv)
	if err != nil {
		return nil, err
	}
	if len(bounds) < 2 {
		return nil, ErrBadStage
	}
	for i := 1; i < len(bounds); i++ {
		if bson.Compare(bounds[i-1], bounds[i]) >= 0 {
			return nil, ErrBadStage
		}
	}
	st := &bucketStage{groupBy: groupBy, bounds: bounds}
	if dv, ok := d.Lookup("default"); ok {
		st.hasDefault = true
		st.defaultVal = dv
	}
	accs, err := bucketOutput(d)
	if err != nil {
		return nil, err
	}
	st.accs = accs
	return st, nil
}

// bucketOutput compiles the output accumulator spec, defaulting to a single count.
func bucketOutput(d bson.Raw) ([]accSpec, error) {
	ov, ok := d.Lookup("output")
	if !ok {
		mk, _ := buildAccumulator("$sum", mkInt32(1))
		return []accSpec{{field: "count", make: mk}}, nil
	}
	if ov.Type != bson.TypeDocument {
		return nil, ErrBadStage
	}
	elems, err := ov.Document().Elements()
	if err != nil {
		return nil, err
	}
	var accs []accSpec
	for _, e := range elems {
		spec, cerr := compileAccumulator(e.Key, e.Value)
		if cerr != nil {
			return nil, cerr
		}
		accs = append(accs, spec)
	}
	return accs, nil
}

type bucketStage struct {
	groupBy    Expr
	bounds     []bson.RawValue
	hasDefault bool
	defaultVal bson.RawValue
	accs       []accSpec
}

func (s *bucketStage) open(in src, ec *execCtx) src {
	return &bucketSrc{in: in, stage: s, ec: ec}
}

type bucketSrc struct {
	in     src
	stage  *bucketStage
	ec     *execCtx
	out    []bson.Raw
	i      int
	loaded bool
}

func (s *bucketSrc) next() (bson.Raw, error) {
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

// load buckets each document by its groupBy value, accumulates per bucket, then
// emits bucket documents sorted by _id (spec 2061 doc 12 §14.1).
func (s *bucketSrc) load() error {
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
		v := s.stage.groupBy.eval(ctx)
		id, ok := s.bucketID(v)
		if !ok {
			return ErrBadStage
		}
		k := groupKey(id)
		g, exists := groups[k]
		if !exists {
			g = &group{id: id, accum: make([]accState, len(s.stage.accs))}
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
	out := make([]*group, 0, len(order))
	for _, k := range order {
		out = append(out, groups[k])
	}
	sort.SliceStable(out, func(i, j int) bool {
		return bson.Compare(out[i].id, out[j].id) < 0
	})
	s.out = make([]bson.Raw, 0, len(out))
	for _, g := range out {
		b := bson.NewBuilder().AppendValue("_id", g.id)
		for i := range s.stage.accs {
			b.AppendValue(s.stage.accs[i].field, g.accum[i].result())
		}
		s.out = append(s.out, b.Build())
	}
	return nil
}

// bucketID returns the lower boundary of the bucket containing v, or the default
// value when v falls outside [b0, bN). The bool is false when v is out of range
// and no default is configured.
func (s *bucketSrc) bucketID(v bson.RawValue) (bson.RawValue, bool) {
	bounds := s.stage.bounds
	if bson.Compare(v, bounds[0]) >= 0 && bson.Compare(v, bounds[len(bounds)-1]) < 0 {
		for i := 0; i+1 < len(bounds); i++ {
			if bson.Compare(v, bounds[i]) >= 0 && bson.Compare(v, bounds[i+1]) < 0 {
				return bounds[i], true
			}
		}
	}
	if s.stage.hasDefault {
		return s.stage.defaultVal, true
	}
	return bson.RawValue{}, false
}

// ---- $bucketAuto ---------------------------------------------------------

// compileBucketAuto compiles an automatic-boundary histogram (spec 2061 doc 12
// §14.2).
func compileBucketAuto(arg bson.RawValue) (stageSpec, error) {
	if arg.Type != bson.TypeDocument {
		return nil, ErrBadStage
	}
	d := arg.Document()
	gbv, ok := d.Lookup("groupBy")
	if !ok {
		return nil, ErrBadStage
	}
	groupBy, err := compileExpr(gbv)
	if err != nil {
		return nil, err
	}
	bv, ok := d.Lookup("buckets")
	if !ok {
		return nil, ErrBadStage
	}
	n, ok := intArg(bv)
	if !ok || n < 1 {
		return nil, ErrBadStage
	}
	st := &bucketAutoStage{groupBy: groupBy, buckets: n}
	if gv, ok := d.Lookup("granularity"); ok {
		s, sok := strOf(gv)
		if !sok {
			return nil, ErrBadStage
		}
		st.granularity = s
	}
	accs, err := bucketOutput(d)
	if err != nil {
		return nil, err
	}
	st.accs = accs
	return st, nil
}

type bucketAutoStage struct {
	groupBy     Expr
	buckets     int
	granularity string
	accs        []accSpec
}

func (s *bucketAutoStage) open(in src, ec *execCtx) src {
	return &bucketAutoSrc{in: in, stage: s, ec: ec}
}

type bucketAutoSrc struct {
	in     src
	stage  *bucketAutoStage
	ec     *execCtx
	out    []bson.Raw
	i      int
	loaded bool
}

func (s *bucketAutoSrc) next() (bson.Raw, error) {
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

// valuedDoc pairs a document with its evaluated groupBy value for sorting.
type valuedDoc struct {
	val bson.RawValue
	doc bson.Raw
}

// load sorts the groupBy values, splits them into roughly equal-count buckets,
// snaps boundaries to the granularity series when set, and emits one document per
// bucket with _id {min, max} (spec 2061 doc 12 §14.2).
func (s *bucketAutoSrc) load() error {
	var rows []valuedDoc
	for {
		doc, err := s.in.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		v := s.stage.groupBy.eval(docCtx(doc, s.ec))
		rows = append(rows, valuedDoc{val: v, doc: doc})
	}
	if len(rows) == 0 {
		return nil
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return bson.Compare(rows[i].val, rows[j].val) < 0
	})
	return s.emit(rows)
}

func (s *bucketAutoSrc) emit(rows []valuedDoc) error {
	n := len(rows)
	bucketCount := s.stage.buckets
	base := n / bucketCount
	rem := n % bucketCount
	start := 0
	for b := 0; b < bucketCount && start < n; b++ {
		size := base
		if b < rem {
			size++
		}
		if size == 0 {
			continue
		}
		end := start + size
		// Granularity may merge adjacent equal-valued runs; extend the bucket to
		// include every document sharing the boundary value.
		for end < n && bson.Compare(rows[end-1].val, rows[end].val) == 0 {
			end++
		}
		minV := rows[start].val
		var maxV bson.RawValue
		if end < n {
			maxV = rows[end].val
		} else {
			maxV = rows[n-1].val
		}
		if s.stage.granularity != "" {
			gmin, ok1 := snapGranularity(minV, s.stage.granularity, false)
			gmax, ok2 := snapGranularity(maxV, s.stage.granularity, true)
			if !ok1 || !ok2 {
				return ErrBadStage
			}
			minV, maxV = gmin, gmax
		}
		idDoc := bson.NewBuilder().AppendValue("min", minV).AppendValue("max", maxV).Build()
		acc := make([]accState, len(s.stage.accs))
		for i, a := range s.stage.accs {
			acc[i] = a.make()
		}
		for _, r := range rows[start:end] {
			ctx := docCtx(r.doc, s.ec)
			for _, a := range acc {
				if err := a.step(ctx); err != nil {
					return err
				}
			}
		}
		bd := bson.NewBuilder().AppendDocument("_id", idDoc)
		for i := range s.stage.accs {
			bd.AppendValue(s.stage.accs[i].field, acc[i].result())
		}
		s.out = append(s.out, bd.Build())
		start = end
	}
	return nil
}

// snapGranularity rounds a numeric boundary to the nearest preferred number in the
// given series (down for a lower bound, up for an upper bound). It reports false
// for a non-numeric value or an unknown series.
func snapGranularity(v bson.RawValue, series string, up bool) (bson.RawValue, bool) {
	_, f, k := numOf(v)
	if k == kindNotNum {
		return bson.RawValue{}, false
	}
	if series == "POWERSOF2" {
		return mkDouble(snapPowerOfTwo(f, up)), true
	}
	mant, ok := granularitySeries(series)
	if !ok {
		return bson.RawValue{}, false
	}
	r, ok := snapToSeries(f, mant, up)
	if !ok {
		return bson.RawValue{}, false
	}
	return mkDouble(r), true
}

// snapPowerOfTwo rounds a value to a power of two, down for a lower bound and up
// for an upper bound.
func snapPowerOfTwo(f float64, up bool) float64 {
	if f <= 0 {
		return 0
	}
	e := math.Log2(f)
	if up {
		return math.Pow(2, math.Ceil(e))
	}
	return math.Pow(2, math.Floor(e))
}

// snapToSeries rounds f up or down to a value of the form m*10^e where m is a
// series mantissa.
func snapToSeries(f float64, mant []float64, up bool) (float64, bool) {
	if f == 0 {
		return 0, true
	}
	neg := f < 0
	x := math.Abs(f)
	exp := math.Floor(math.Log10(x))
	// Build candidate values across the neighbouring decades.
	var cands []float64
	for e := exp - 1; e <= exp+1; e++ {
		scale := math.Pow(10, e)
		for _, m := range mant {
			cands = append(cands, m*scale)
		}
		cands = append(cands, 10*scale)
	}
	sort.Float64s(cands)
	if up {
		for _, c := range cands {
			if c >= x-1e-9 {
				return signed(c, neg), true
			}
		}
		return signed(cands[len(cands)-1], neg), true
	}
	best := cands[0]
	for _, c := range cands {
		if c <= x+1e-9 {
			best = c
		}
	}
	return signed(best, neg), true
}

func signed(v float64, neg bool) float64 {
	if neg {
		return -v
	}
	return v
}

// granularitySeries returns the mantissa table for a preferred-number series.
func granularitySeries(name string) ([]float64, bool) {
	switch name {
	case "R5":
		return []float64{1, 1.6, 2.5, 4, 6.3}, true
	case "R10":
		return []float64{1, 1.25, 1.6, 2, 2.5, 3.15, 4, 5, 6.3, 8}, true
	case "R20":
		return []float64{1, 1.12, 1.25, 1.4, 1.6, 1.8, 2, 2.24, 2.5, 2.8,
			3.15, 3.55, 4, 4.5, 5, 5.6, 6.3, 7.1, 8, 9}, true
	case "R40":
		return []float64{1, 1.06, 1.12, 1.18, 1.25, 1.32, 1.4, 1.5, 1.6, 1.7,
			1.8, 1.9, 2, 2.12, 2.24, 2.36, 2.5, 2.65, 2.8, 3,
			3.15, 3.35, 3.55, 3.75, 4, 4.25, 4.5, 4.75, 5, 5.3,
			5.6, 6, 6.3, 6.7, 7.1, 7.5, 8, 8.5, 9, 9.5}, true
	case "1-2-5":
		return []float64{1, 2, 5}, true
	case "E6":
		return []float64{1, 1.5, 2.2, 3.3, 4.7, 6.8}, true
	case "E12":
		return []float64{1, 1.2, 1.5, 1.8, 2.2, 2.7, 3.3, 3.9, 4.7, 5.6, 6.8, 8.2}, true
	case "E24":
		return []float64{1, 1.1, 1.2, 1.3, 1.5, 1.6, 1.8, 2, 2.2, 2.4, 2.7, 3,
			3.3, 3.6, 3.9, 4.3, 4.7, 5.1, 5.6, 6.2, 6.8, 7.5, 8.2, 9.1}, true
	case "POWERSOF2":
		return []float64{1, 2, 4, 8, 16, 32, 64, 128, 256, 512}, true
	default:
		return nil, false
	}
}
