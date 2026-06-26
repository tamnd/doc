package collection

import (
	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/colstore"
)

// This file routes a covered $group through the column store's vectorized executor
// (spec 2061 doc 19 §6.3). The reconstruct path in columnstore.go rebuilds a BSON
// document per visible row and replays the pipeline; for a plain $group over field
// paths and numeric accumulators that document is overhead the executor avoids by
// folding the decoded columns straight into per-group accumulators. The executor
// reproduces the aggregation engine's accumulator semantics exactly, so the result
// is byte-identical to the reconstruct path, which the differential test pins.
//
// The fast path is deliberately narrow. It fires only for a single terminal $group
// with no leading $match, so the pipeline's $match (the source of truth for predicate
// semantics) never needs to run as a backstop. A leading $match still routes through
// the reconstruct path, which keeps the pipeline's $match as the correctness check.
// Any $group shape the executor cannot reproduce value-for-value is rejected here and
// falls back to reconstruct, never to a wrong answer.

// columnGroup runs an eligible terminal $group through the vectorized executor and
// returns the final group documents. ok=false means the pipeline is not an eligible
// vectorizable $group, or the column store does not cover it or is not the cheaper
// plan, and the caller should run the ordinary path.
func (t *Txn) columnGroup(pipeline []bson.Raw) (docs []bson.Raw, ok bool) {
	c := t.c
	if c.cstore == nil {
		return nil, false
	}
	if len(pipeline) != 1 {
		return nil, false // a leading $match keeps the reconstruct path and its $match backstop
	}
	name, body, ok := singleStage(pipeline[0])
	if !ok || name != "$group" {
		return nil, false
	}
	spec, ok := parseVectorGroup(body)
	if !ok {
		return nil, false
	}
	if !c.cstore.Covers(groupSpecFields(spec)) {
		return nil, false
	}
	if !c.cstore.PreferOverHeap(t.startVer, nil) {
		return nil, false
	}
	return c.cstore.GroupExec(t.startVer, spec)
}

// groupSpecFields lists every column the group spec reads, so coverage can be checked
// before the executor runs.
func groupSpecFields(spec colstore.GroupSpec) []string {
	var fs []string
	if spec.KeyField != "" {
		fs = append(fs, spec.KeyField)
	}
	for _, a := range spec.Accs {
		if a.Field != "" {
			fs = append(fs, a.Field)
		}
	}
	return fs
}

// parseVectorGroup recognizes the $group shapes the vectorized executor reproduces
// exactly: a key that is a single field path or the whole collection (a null or
// missing _id), and accumulators limited to $sum and $avg of a field path, $sum of an
// integer constant, $min and $max of a field path, and $count. Anything else (a
// composite or constant _id, a double constant, a nested expression argument, an
// accumulator the executor does not vectorize) returns false so the heap path runs.
func parseVectorGroup(body bson.Raw) (colstore.GroupSpec, bool) {
	els, err := body.Elements()
	if err != nil {
		return colstore.GroupSpec{}, false
	}
	var spec colstore.GroupSpec
	sawID := false
	for _, e := range els {
		if e.Key == idFieldName {
			sawID = true
			kf, ok := vectorKeyField(e.Value)
			if !ok {
				return colstore.GroupSpec{}, false
			}
			spec.KeyField = kf
			continue
		}
		acc, ok := vectorAccumulator(e.Key, e.Value)
		if !ok {
			return colstore.GroupSpec{}, false
		}
		spec.Accs = append(spec.Accs, acc)
	}
	if !sawID {
		return colstore.GroupSpec{}, false
	}
	return spec, true
}

// vectorKeyField extracts the group key field path, or "" for a whole-collection
// group. A field path "$f" groups by f; a null or missing _id groups the whole
// collection. A constant scalar, document, array, or expression _id is not
// vectorized (false), since the executor only emits a field value or null as the _id.
func vectorKeyField(v bson.RawValue) (string, bool) {
	switch v.Type {
	case bson.TypeNull, 0:
		return "", true
	case bson.TypeString:
		s := v.StringValue()
		if len(s) > 0 && s[0] == '$' {
			path := s[1:]
			if path == "" || path == idFieldName {
				return "", false
			}
			return path, true
		}
		return "", false // a constant string _id needs the constant emitted, not vectorized
	default:
		return "", false
	}
}

// vectorAccumulator parses one {out: {op: arg}} accumulator into an AccSpec the
// executor can run, or false to reject the whole group.
func vectorAccumulator(out string, v bson.RawValue) (colstore.AccSpec, bool) {
	if v.Type != bson.TypeDocument {
		return colstore.AccSpec{}, false
	}
	inner, err := v.Document().Elements()
	if err != nil || len(inner) != 1 {
		return colstore.AccSpec{}, false
	}
	op, arg := inner[0].Key, inner[0].Value
	switch op {
	case "$sum":
		return vectorSum(out, arg)
	case "$avg":
		if field, ok := vectorFieldArg(arg); ok {
			return colstore.AccSpec{Out: out, Kind: colstore.AccAvg, Field: field}, true
		}
		return colstore.AccSpec{}, false
	case "$min":
		if field, ok := vectorFieldArg(arg); ok {
			return colstore.AccSpec{Out: out, Kind: colstore.AccMin, Field: field}, true
		}
		return colstore.AccSpec{}, false
	case "$max":
		if field, ok := vectorFieldArg(arg); ok {
			return colstore.AccSpec{Out: out, Kind: colstore.AccMax, Field: field}, true
		}
		return colstore.AccSpec{}, false
	case "$count":
		// {$count: {}} counts rows; any other argument shape is not the $count idiom.
		if arg.Type == bson.TypeDocument {
			if els, err := arg.Document().Elements(); err == nil && len(els) == 0 {
				return colstore.AccSpec{Out: out, Kind: colstore.AccCount}, true
			}
		}
		return colstore.AccSpec{}, false
	default:
		return colstore.AccSpec{}, false
	}
}

// vectorSum parses a $sum argument: a field path sums that column, an integer
// constant sums the constant once per row (the counting idiom). A double constant is
// rejected because reproducing the pipeline's repeated float addition bit-for-bit is
// not worth a fast path; it falls back to reconstruct.
func vectorSum(out string, arg bson.RawValue) (colstore.AccSpec, bool) {
	if field, ok := vectorFieldArg(arg); ok {
		return colstore.AccSpec{Out: out, Kind: colstore.AccSum, Field: field}, true
	}
	switch arg.Type {
	case bson.TypeInt32:
		return colstore.AccSpec{Out: out, Kind: colstore.AccSum, ConstI: int64(arg.Int32()), ConstKind: colstore.ConstInt32}, true
	case bson.TypeInt64:
		return colstore.AccSpec{Out: out, Kind: colstore.AccSum, ConstI: arg.Int64(), ConstKind: colstore.ConstInt64}, true
	default:
		return colstore.AccSpec{}, false
	}
}

// vectorFieldArg returns the field path of a "$f" accumulator argument, rejecting a
// path into _id, a nested expression, or a constant.
func vectorFieldArg(arg bson.RawValue) (string, bool) {
	if arg.Type != bson.TypeString {
		return "", false
	}
	s := arg.StringValue()
	if len(s) < 2 || s[0] != '$' {
		return "", false
	}
	path := s[1:]
	if path == idFieldName {
		return "", false
	}
	return path, true
}
