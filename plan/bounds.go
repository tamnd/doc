package plan

import (
	"fmt"
	"strings"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/catalog"
	"github.com/tamnd/doc/index"
	"github.com/tamnd/doc/storage"
)

// Interval is the logical bound on one index field, the planner's per-field range
// before it is encoded into bytes (spec 2061 doc 10 §6.2). An open end is marked
// by HasLo or HasHi being false, which the encoder fills with the MinKey or MaxKey
// sentinel. AlwaysTrue is the unconstrained interval (MinKey, MaxKey); AlwaysFalse
// is empty. Point reports an equality, where Lo and Hi are the same value. Tight
// reports whether the interval captures its predicate exactly, so no residual
// filter is needed for that field.
type Interval struct {
	Lo          bson.RawValue
	HasLo       bool
	LoInclusive bool
	Hi          bson.RawValue
	HasHi       bool
	HiInclusive bool
	Point       bool
	AlwaysTrue  bool
	AlwaysFalse bool
	Tight       bool
}

// fieldBound accumulates the boundable predicates the filter places on one field:
// an equality, and a lower and upper comparison. Non-boundable predicates on the
// field (for example $ne or $regex) are not recorded here; the residual filter
// re-applies the full match, so leaving them out only widens the scan.
type fieldBound struct {
	hasEq bool
	eq    bson.RawValue
	hasLo bool
	lo    bson.RawValue
	loInc bool
	hasHi bool
	hi    bson.RawValue
	hiInc bool
}

// extractBounds walks the filter's top-level field predicates and any top-level
// $and, recording the boundable comparison on each field. Predicates inside $or,
// $nor, or $not are deliberately ignored: they are not global AND constraints, so
// using them as index bounds could drop matching documents. The residual filter
// still enforces the complete match, so the extracted bounds only ever need to be
// a superset.
func extractBounds(filter bson.Raw) map[string]*fieldBound {
	out := make(map[string]*fieldBound)
	collectBounds(filter, out)
	return out
}

func collectBounds(d bson.Raw, out map[string]*fieldBound) {
	elems, err := d.Elements()
	if err != nil {
		return
	}
	for _, e := range elems {
		if e.Key == "$and" {
			for _, sub := range arrayDocs(e.Value) {
				collectBounds(sub, out)
			}
			continue
		}
		if strings.HasPrefix(e.Key, "$") {
			continue // $or, $nor, and other logical operators are not global bounds
		}
		if isOperatorDoc(e.Value) {
			applyOps(out, e.Key, e.Value.Document())
			continue
		}
		setEq(out, e.Key, e.Value)
	}
}

func applyOps(out map[string]*fieldBound, field string, ops bson.Raw) {
	elems, err := ops.Elements()
	if err != nil {
		return
	}
	for _, e := range elems {
		switch e.Key {
		case "$eq":
			setEq(out, field, e.Value)
		case "$gt":
			mergeLo(out, field, e.Value, false)
		case "$gte":
			mergeLo(out, field, e.Value, true)
		case "$lt":
			mergeHi(out, field, e.Value, false)
		case "$lte":
			mergeHi(out, field, e.Value, true)
		}
	}
}

func boundFor(out map[string]*fieldBound, field string) *fieldBound {
	b := out[field]
	if b == nil {
		b = &fieldBound{}
		out[field] = b
	}
	return b
}

func setEq(out map[string]*fieldBound, field string, v bson.RawValue) {
	b := boundFor(out, field)
	b.hasEq = true
	b.eq = v
}

func mergeLo(out map[string]*fieldBound, field string, v bson.RawValue, inc bool) {
	b := boundFor(out, field)
	if !b.hasLo {
		b.hasLo, b.lo, b.loInc = true, v, inc
		return
	}
	switch bson.Compare(v, b.lo) {
	case 1:
		b.lo, b.loInc = v, inc
	case 0:
		b.loInc = b.loInc && inc // the tighter (exclusive) lower bound wins
	}
}

func mergeHi(out map[string]*fieldBound, field string, v bson.RawValue, inc bool) {
	b := boundFor(out, field)
	if !b.hasHi {
		b.hasHi, b.hi, b.hiInc = true, v, inc
		return
	}
	switch bson.Compare(v, b.hi) {
	case -1:
		b.hi, b.hiInc = v, inc
	case 0:
		b.hiInc = b.hiInc && inc
	}
}

// intervalOf reduces a field's accumulated bounds to one interval. An equality is
// a tight point; a lone comparison is a half-open range; no bound at all is the
// unconstrained AlwaysTrue interval.
func intervalOf(b *fieldBound) Interval {
	if b == nil {
		return Interval{AlwaysTrue: true}
	}
	if b.hasEq {
		return Interval{Lo: b.eq, Hi: b.eq, HasLo: true, HasHi: true, LoInclusive: true, HiInclusive: true, Point: true, Tight: true}
	}
	if !b.hasLo && !b.hasHi {
		return Interval{AlwaysTrue: true}
	}
	iv := Interval{Tight: true}
	if b.hasLo {
		iv.HasLo, iv.Lo, iv.LoInclusive = true, b.lo, b.loInc
	}
	if b.hasHi {
		iv.HasHi, iv.Hi, iv.HiInclusive = true, b.hi, b.hiInc
	}
	return iv
}

// scanBounds is the byte range and per-field intervals the planner hands the
// executor and the explain renderer. Lo and Hi are encoded field-key prefixes; the
// executor scans them inclusively at both ends and relies on the residual filter
// for exact boundary and trailing-field semantics, so the range is always a
// superset of the matching keys (spec 2061 doc 10 §6.3).
type scanBounds struct {
	Lo        storage.IndexKey
	Hi        storage.IndexKey
	Intervals []Interval
	Usable    bool // the leading field carried a bound, so the index narrows the scan
	Empty     bool // a field's interval was empty, so the scan returns nothing
	Tight     bool // every field interval is tight, so no residual filter is required
}

// buildScanBounds encodes a compound index's byte range from the per-field
// intervals, following the leftmost-prefix rule: equality fields chain exact
// encodings, the first range field opens the range, and every field after it is
// left unconstrained (widened with the MinKey and MaxKey sentinels) because the
// B-tree cannot tighten past a range (spec 2061 doc 10 §6.4). The returned range
// is a superset; the caller pairs it with a residual filter.
func buildScanBounds(key []catalog.KeyPart, fb map[string]*fieldBound) (scanBounds, error) {
	var lo, hi []byte
	ivs := make([]Interval, 0, len(key))
	usable := false
	tight := true
	stopped := false

	for i, kp := range key {
		iv := intervalOf(fb[kp.Field])

		if stopped {
			iv = Interval{AlwaysTrue: true}
			tight = false
			lo = append(lo, index.EncodeBoundMin(kp.Desc)...)
			hi = append(hi, index.EncodeBoundMax(kp.Desc)...)
			ivs = append(ivs, iv)
			continue
		}

		if iv.AlwaysFalse {
			ivs = append(ivs, iv)
			return scanBounds{Empty: true, Intervals: ivs, Usable: i == 0}, nil
		}

		if iv.AlwaysTrue {
			// No constraint on this field: widen it and stop tightening the rest.
			tight = false
			stopped = true
			lo = append(lo, index.EncodeBoundMin(kp.Desc)...)
			hi = append(hi, index.EncodeBoundMax(kp.Desc)...)
			ivs = append(ivs, iv)
			continue
		}

		if iv.Point {
			enc, err := index.EncodeField(iv.Lo, kp.Desc)
			if err != nil {
				// Unencodable point: treat the field as unconstrained from here.
				tight, stopped = false, true
				lo = append(lo, index.EncodeBoundMin(kp.Desc)...)
				hi = append(hi, index.EncodeBoundMax(kp.Desc)...)
				ivs = append(ivs, Interval{AlwaysTrue: true})
				continue
			}
			lo = append(lo, enc...)
			hi = append(hi, enc...)
			if i == 0 {
				usable = true
			}
			ivs = append(ivs, iv)
			continue
		}

		// A range field: encode its open-ended bounds, then stop tightening.
		loEnc, hiEnc, err := encodeRange(iv, kp.Desc)
		if err != nil {
			tight, stopped = false, true
			lo = append(lo, index.EncodeBoundMin(kp.Desc)...)
			hi = append(hi, index.EncodeBoundMax(kp.Desc)...)
			ivs = append(ivs, Interval{AlwaysTrue: true})
			continue
		}
		lo = append(lo, loEnc...)
		hi = append(hi, hiEnc...)
		if i == 0 {
			usable = true
		}
		if !iv.LoInclusive || !iv.HiInclusive {
			tight = false // inclusive scan plus residual filter handles exclusivity
		}
		stopped = true
		ivs = append(ivs, iv)
	}

	return scanBounds{Lo: lo, Hi: hi, Intervals: ivs, Usable: usable, Tight: tight}, nil
}

// encodeRange encodes a range field's lower and upper byte bounds. For a
// descending field the byte order is inverted, so the larger value produces the
// smaller bytes: the lo byte bound comes from the field's high value and the hi
// byte bound from its low value.
func encodeRange(iv Interval, desc bool) (lo, hi []byte, err error) {
	loVal, loOpen := iv.Lo, !iv.HasLo
	hiVal, hiOpen := iv.Hi, !iv.HasHi
	if desc {
		loVal, loOpen, hiVal, hiOpen = iv.Hi, !iv.HasHi, iv.Lo, !iv.HasLo
	}
	if loOpen {
		lo = index.EncodeBoundMin(desc)
	} else {
		lo, err = index.EncodeField(loVal, desc)
		if err != nil {
			return nil, nil, err
		}
	}
	if hiOpen {
		hi = index.EncodeBoundMax(desc)
	} else {
		hi, err = index.EncodeField(hiVal, desc)
		if err != nil {
			return nil, nil, err
		}
	}
	return lo, hi, nil
}

// ---- filter-shape helpers ------------------------------------------------

// isOperatorDoc reports whether a value is an operator document, one whose first
// field name begins with "$". A plain document is an equality comparand.
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

// arrayDocs returns the document elements of a BSON array value, for walking the
// operands of a $and.
func arrayDocs(v bson.RawValue) []bson.Raw {
	if v.Type != bson.TypeArray {
		return nil
	}
	elems, err := v.Document().Elements()
	if err != nil {
		return nil
	}
	out := make([]bson.Raw, 0, len(elems))
	for _, e := range elems {
		if e.Value.Type == bson.TypeDocument {
			out = append(out, e.Value.Document())
		}
	}
	return out
}

// intervalString renders an interval as MongoDB's explain bound text, for example
// "[5, 5]", "(5, inf]", or "[\"a\", \"b\")".
func intervalString(iv Interval) string {
	if iv.AlwaysTrue {
		return "[MinKey, MaxKey]"
	}
	if iv.AlwaysFalse {
		return "(empty)"
	}
	lo, hi := "MinKey", "MaxKey"
	if iv.HasLo {
		lo = valueString(iv.Lo)
	}
	if iv.HasHi {
		hi = valueString(iv.Hi)
	}
	openB, closeB := "[", "]"
	if iv.HasLo && !iv.LoInclusive {
		openB = "("
	}
	if iv.HasHi && !iv.HiInclusive {
		closeB = ")"
	}
	return fmt.Sprintf("%s%s, %s%s", openB, lo, hi, closeB)
}

// valueString renders an index-bound value for explain text.
func valueString(v bson.RawValue) string {
	switch v.Type {
	case bson.TypeString:
		return fmt.Sprintf("%q", v.StringValue())
	case bson.TypeInt32:
		return fmt.Sprintf("%d", v.Int32())
	case bson.TypeInt64:
		return fmt.Sprintf("%d", v.Int64())
	case bson.TypeDouble:
		return fmt.Sprintf("%g", v.Double())
	case bson.TypeBoolean:
		return fmt.Sprintf("%t", v.Boolean())
	case bson.TypeObjectID:
		oid := v.ObjectID()
		return fmt.Sprintf("ObjectId(%x)", oid[:])
	default:
		return v.Type.String()
	}
}
