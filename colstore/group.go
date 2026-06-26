package colstore

import (
	"math"
	"strconv"
	"strings"

	"github.com/tamnd/doc/bson"
)

// This file is the vectorized $group executor (spec 2061 doc 19 §6.3, doc 12 §7).
// The reconstruct path (reconstruct.go) rebuilds one BSON document per visible row
// and replays the unchanged aggregation pipeline over them, which is correct by
// construction but pays a document build and a field re-lookup per row. For the
// analytical shape the column store is built for (a $group with field-path keys and
// numeric accumulators, no leading $match), that per-row document is pure overhead:
// the accumulators only ever read the columns the group already decoded.
//
// GroupExec folds the columns straight into per-group accumulators, so a million-row
// $sum touches the decoded float column and nothing else. It reproduces the heap
// path's accumulator semantics exactly, value for value and type for type, so the
// output is byte-identical to the reconstruct path. The differential test in
// collection/columngroup_test.go pins that equality across random inputs; anything
// this executor cannot reproduce exactly is rejected at parse time and falls back to
// reconstruct.

// AccKind is the accumulator a group output field computes.
type AccKind uint8

const (
	AccSum AccKind = iota
	AccAvg
	AccMin
	AccMax
	AccCount
)

// numKind mirrors the aggregation engine's numeric width lattice (agg/value.go):
// int32 < int64 < double, so a sum of integers stays integer and a sum that touches
// a double widens to double, matching MongoDB's widest-operand result rule.
type numKind uint8

const (
	kindInt32 numKind = iota
	kindInt64
	kindDouble
)

// ConstInt32 and ConstInt64 are the exported widths the parser tags a $sum integer
// constant with, so the executor reproduces the constant's numeric type exactly.
const (
	ConstInt32 = kindInt32
	ConstInt64 = kindInt64
)

func widen(a, b numKind) numKind {
	if a > b {
		return a
	}
	return b
}

// AccSpec describes one accumulator the executor must compute, parsed from a $group
// body. Field is the projected column the accumulator reads; an empty Field with a
// non-Count kind means a constant integer argument (the $sum: 1 counting idiom),
// carried in ConstI with width ConstKind.
type AccSpec struct {
	Out       string // output field name in the group document
	Kind      AccKind
	Field     string // column read by the accumulator, "" for a constant or count
	ConstI    int64  // constant integer argument, used when Field == "" and Kind == AccSum
	ConstKind numKind
}

// GroupSpec is a fully vectorizable $group: a key that is either a projected field
// path (KeyField set) or the whole collection (KeyField empty, one null-keyed group),
// plus the accumulators in output order.
type GroupSpec struct {
	KeyField string
	Accs     []AccSpec
}

// accState is one group's running accumulator state, one per AccSpec.
type accState struct {
	spec AccSpec
	// sum
	iSum float64Sum
	// avg
	fAvg float64
	nAvg int64
	// min/max
	set     bool
	bestVal Value
	bestRV  bson.RawValue
	// count
	count int64
}

// float64Sum carries the dual int64/float64 running total and the running numeric
// width a $sum needs to reproduce sumAcc exactly.
type float64Sum struct {
	i    int64
	f    float64
	kind numKind
}

// groupState is the set of accumulator states for one group plus its emitted _id.
type groupState struct {
	id   Value
	accs []accState
}

// GroupExec runs spec over every visible row at snapshot snap and returns the group
// output documents in first-seen order, byte-identical to the reconstruct path. It
// returns ok=false only if a column the spec needs is not stored, in which case the
// caller falls back to reconstruct.
func (s *Store) GroupExec(snap uint64, spec GroupSpec) (out []bson.Raw, ok bool) {
	need := groupNeed(spec)
	for _, f := range need {
		if !s.Covers([]string{f}) {
			return nil, false
		}
	}

	accIdx := make([]int, len(spec.Accs))
	for i, a := range spec.Accs {
		accIdx[i] = indexOf(need, a.Field)
	}
	newGroup := func(id Value) *groupState {
		g := &groupState{id: id, accs: make([]accState, len(spec.Accs))}
		for i := range spec.Accs {
			g.accs[i].spec = spec.Accs[i]
		}
		return g
	}
	stepGroup := func(g *groupState, row []Value) {
		for i := range g.accs {
			var v Value
			if accIdx[i] >= 0 {
				v = row[accIdx[i]]
			}
			g.accs[i].step(v)
		}
	}

	var order []*groupState
	keyIdx := indexOf(need, spec.KeyField)
	if keyIdx < 0 {
		// Whole-collection aggregate: one null-keyed group, no per-row key to hash and
		// no map lookup, so the scan stays allocation-free on the hot path.
		g := newGroup(NullValue)
		err := s.scan(snap, need, nil, func(row []Value) { stepGroup(g, row) })
		if err != nil {
			return nil, false
		}
		order = []*groupState{g}
		return emitGroups(order, spec), true
	}

	byKey := make(map[string]*groupState)
	err := s.scan(snap, need, nil, func(row []Value) {
		key := row[keyIdx]
		hk := groupKeyOf(key)
		g := byKey[hk]
		if g == nil {
			g = newGroup(key)
			byKey[hk] = g
			order = append(order, g)
		}
		stepGroup(g, row)
	})
	if err != nil {
		return nil, false
	}
	return emitGroups(order, spec), true
}

// emitGroups materializes the group output documents in first-seen order: _id first,
// then each accumulator's present result in declaration order, matching the pipeline's
// group output shape (agg/group.go).
func emitGroups(order []*groupState, spec GroupSpec) []bson.Raw {
	out := make([]bson.Raw, 0, len(order))
	for _, g := range order {
		b := bson.NewBuilder().AppendValue("_id", g.id.ToRawValue())
		for i := range g.accs {
			rv, present := g.accs[i].result()
			if present {
				b.AppendValue(g.accs[i].spec.Out, rv)
			}
		}
		out = append(out, b.Build())
	}
	return out
}

// groupNeed is the distinct, non-empty set of columns the spec reads: the key field
// and every accumulator's field argument.
func groupNeed(spec GroupSpec) []string {
	fs := []string{spec.KeyField}
	for _, a := range spec.Accs {
		fs = append(fs, a.Field)
	}
	return dedupFields(fs...)
}

// step folds one value into the accumulator, mirroring the matching agg accumulator
// (agg/group.go) so the result is identical to the reconstruct path.
func (a *accState) step(v Value) {
	switch a.spec.Kind {
	case AccSum:
		a.stepSum(v)
	case AccAvg:
		if _, f, _, ok := numParts(v); ok {
			a.fAvg += f
			a.nAvg++
		}
	case AccMin, AccMax:
		a.stepMinMax(v)
	case AccCount:
		a.count++
	}
}

// stepSum reproduces sumAcc: a field argument adds the numeric value and widens, a
// constant integer argument adds the constant once per row.
func (a *accState) stepSum(v Value) {
	if a.spec.Field == "" {
		// Constant integer argument: add it once per row, the $sum: <int> idiom.
		a.iSum.i += a.spec.ConstI
		a.iSum.f += float64(a.spec.ConstI)
		a.iSum.kind = widen(a.iSum.kind, a.spec.ConstKind)
		return
	}
	i, f, k, ok := numParts(v)
	if !ok {
		return
	}
	a.iSum.i += i
	a.iSum.f += f
	a.iSum.kind = widen(a.iSum.kind, k)
}

// stepMinMax reproduces minMaxAcc, tracking the best value by BSON order and ignoring
// missing values. The cheap comparison keeps numeric, string, bool, and null columns
// allocation-free; only a genuine update or a cross-type comparison reconstructs the
// candidate to settle it against bson.Compare.
func (a *accState) stepMinMax(v Value) {
	if v.Kind == KindNull {
		return // a missing or null projected field is ignored, like the heap path
	}
	if !a.set {
		a.bestVal, a.bestRV, a.set = v, v.ToRawValue(), true
		return
	}
	max := a.spec.Kind == AccMax
	if c, ok := cheapCompare(v, a.bestVal); ok {
		if (max && c > 0) || (!max && c < 0) {
			a.bestVal, a.bestRV = v, v.ToRawValue()
		}
		return
	}
	rv := v.ToRawValue()
	c := bson.Compare(rv, a.bestRV)
	if (max && c > 0) || (!max && c < 0) {
		a.bestVal, a.bestRV = v, rv
	}
}

// result returns the accumulator's output value and whether it is present. A missing
// result (an empty $min or $max group) is dropped from the output document, matching
// the pipeline's skip of a missing accumulator result.
func (a *accState) result() (bson.RawValue, bool) {
	switch a.spec.Kind {
	case AccSum:
		return mkNumValue(a.iSum.i, a.iSum.f, a.iSum.kind), true
	case AccAvg:
		if a.nAvg == 0 {
			return rawNull(), true // avg of an empty group is null, and null is emitted
		}
		return rawDouble(a.fAvg / float64(a.nAvg)), true
	case AccMin, AccMax:
		if !a.set {
			return rawNull(), true
		}
		return a.bestRV, true
	case AccCount:
		return mkNumValue(a.count, float64(a.count), kindInt32), true
	}
	return bson.RawValue{}, false
}

// numParts classifies a column value the way numOf classifies a reconstructed BSON
// value: KindInt reconstructs as int64, KindFloat as double, and bool is not numeric
// here (unlike $avg's coercion, min/max and the reconstructed-doc sum never see a
// bool as a number because the column reconstructs bool as a boolean). A bool column
// value therefore does not feed $sum, matching the reconstruct path exactly.
func numParts(v Value) (i int64, f float64, k numKind, ok bool) {
	switch v.Kind {
	case KindInt:
		return v.I, float64(v.I), kindInt64, true
	case KindFloat:
		return 0, v.F, kindDouble, true
	default:
		return 0, 0, 0, false
	}
}

// cheapCompare compares two column values without allocating when they share a
// comparable family that agrees with bson.Compare: two finite numbers within the
// exact-float range, two strings, two booleans, or two nulls. It returns ok=false for
// anything else (mixed types, large integers, opaque values), leaving those to a
// bson.Compare on reconstructed values.
func cheapCompare(a, b Value) (int, bool) {
	an, bn := a.Kind == KindInt || a.Kind == KindFloat, b.Kind == KindInt || b.Kind == KindFloat
	if an && bn {
		// Stay on the float fast path only where it agrees with bson.Compare's
		// arbitrary-precision numeric order: integers beyond 2^53 are not exactly
		// representable, so defer those.
		if a.Kind == KindInt && (a.I > 1<<53 || a.I < -(1<<53)) {
			return 0, false
		}
		if b.Kind == KindInt && (b.I > 1<<53 || b.I < -(1<<53)) {
			return 0, false
		}
		af, _ := a.AsFloat()
		bf, _ := b.AsFloat()
		switch {
		case af < bf:
			return -1, true
		case af > bf:
			return 1, true
		default:
			return 0, true
		}
	}
	if a.Kind != b.Kind {
		return 0, false
	}
	switch a.Kind {
	case KindString:
		return strings.Compare(a.S, b.S), true
	case KindBool:
		switch {
		case a.I < b.I:
			return -1, true
		case a.I > b.I:
			return 1, true
		default:
			return 0, true
		}
	case KindNull:
		return 0, true
	default:
		return 0, false
	}
}

// groupKeyOf builds the canonical group key for a column value, reproducing the
// aggregation engine's groupKey/writeKey (agg/group.go) for the reconstructed value
// so a column group buckets rows exactly as the pipeline would. KindInt reconstructs
// as int64 and KindFloat as double, which is what the key writer sees on the
// reconstruct path.
func groupKeyOf(v Value) string {
	var b strings.Builder
	writeColumnKey(&b, v)
	return b.String()
}

func writeColumnKey(b *strings.Builder, v Value) {
	switch v.Kind {
	case KindNull:
		b.WriteByte('Z')
	case KindInt:
		b.WriteByte('I')
		b.WriteString(strconv.FormatInt(v.I, 10))
	case KindFloat:
		f := v.F
		if f == math.Trunc(f) && f >= -9.2e18 && f <= 9.2e18 {
			b.WriteByte('I')
			b.WriteString(strconv.FormatInt(int64(f), 10))
			return
		}
		b.WriteByte('F')
		b.WriteString(strconv.FormatUint(math.Float64bits(f), 16))
	case KindBool:
		if v.I != 0 {
			b.WriteString("B1")
		} else {
			b.WriteString("B0")
		}
	case KindString:
		b.WriteByte('S')
		b.WriteString(v.S)
	default: // KindOther: opaque raw bytes keyed by their BSON type and payload
		b.WriteByte('R')
		b.WriteByte(v.tag)
		b.WriteString(v.S)
	}
}

// mkNumValue builds a numeric result of the given width, reproducing agg's mkNum:
// int32 promotes to int64 when it overflows the 32-bit range, and a double width
// returns the float total.
func mkNumValue(i int64, f float64, k numKind) bson.RawValue {
	switch k {
	case kindInt32:
		if i >= math.MinInt32 && i <= math.MaxInt32 {
			return rawInt32(int32(i))
		}
		return rawInt64(i)
	case kindInt64:
		return rawInt64(i)
	default:
		return rawDouble(f)
	}
}

func rawInt32(v int32) bson.RawValue {
	return scratch(bson.NewBuilder().AppendInt32("v", v))
}

func rawInt64(v int64) bson.RawValue {
	return scratch(bson.NewBuilder().AppendInt64("v", v))
}

func rawDouble(v float64) bson.RawValue {
	return scratch(bson.NewBuilder().AppendDouble("v", v))
}

func rawNull() bson.RawValue {
	return scratch(bson.NewBuilder().AppendNull("v"))
}
