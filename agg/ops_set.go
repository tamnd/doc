package agg

import (
	"github.com/tamnd/doc/bson"
)

// containsVal reports membership by BSON equality.
func containsVal(set []bson.RawValue, v bson.RawValue) bool {
	for _, e := range set {
		if bson.Equal(e, v) {
			return true
		}
	}
	return false
}

// dedupe removes BSON-equal duplicates, preserving first-seen order.
func dedupe(in []bson.RawValue) []bson.RawValue {
	out := make([]bson.RawValue, 0, len(in))
	for _, v := range in {
		if !containsVal(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// opSetEquals reports whether all operands hold the same set of values.
func opSetEquals(vals []bson.RawValue) bson.RawValue {
	first, err := arrayElements(vals[0])
	if err != nil {
		return mkNull()
	}
	base := dedupe(first)
	for _, v := range vals[1:] {
		els, eerr := arrayElements(v)
		if eerr != nil {
			return mkNull()
		}
		other := dedupe(els)
		if len(other) != len(base) {
			return mkBool(false)
		}
		for _, b := range base {
			if !containsVal(other, b) {
				return mkBool(false)
			}
		}
	}
	return mkBool(true)
}

// opSetIntersection returns the deduplicated values common to every operand.
func opSetIntersection(vals []bson.RawValue) bson.RawValue {
	first, err := arrayElements(vals[0])
	if err != nil {
		return mkNull()
	}
	result := dedupe(first)
	for _, v := range vals[1:] {
		els, eerr := arrayElements(v)
		if eerr != nil {
			return mkNull()
		}
		var kept []bson.RawValue
		for _, r := range result {
			if containsVal(els, r) {
				kept = append(kept, r)
			}
		}
		result = kept
	}
	return mkArray(result)
}

// opSetUnion returns the deduplicated union of all operands.
func opSetUnion(vals []bson.RawValue) bson.RawValue {
	var all []bson.RawValue
	for _, v := range vals {
		els, err := arrayElements(v)
		if err != nil {
			return mkNull()
		}
		all = append(all, els...)
	}
	return mkArray(dedupe(all))
}

// opSetDifference returns the values in the first set absent from the second.
func opSetDifference(vals []bson.RawValue) bson.RawValue {
	a, err := arrayElements(vals[0])
	if err != nil {
		return mkNull()
	}
	b, err := arrayElements(vals[1])
	if err != nil {
		return mkNull()
	}
	var out []bson.RawValue
	for _, v := range dedupe(a) {
		if !containsVal(b, v) {
			out = append(out, v)
		}
	}
	return mkArray(out)
}

// opSetIsSubset reports whether every member of the first set is in the second.
func opSetIsSubset(vals []bson.RawValue) bson.RawValue {
	a, err := arrayElements(vals[0])
	if err != nil {
		return mkNull()
	}
	b, err := arrayElements(vals[1])
	if err != nil {
		return mkNull()
	}
	for _, v := range a {
		if !containsVal(b, v) {
			return mkBool(false)
		}
	}
	return mkBool(true)
}

// opAnyElementTrue reports whether any array element is truthy.
func opAnyElementTrue(vals []bson.RawValue) bson.RawValue {
	els, err := arrayElements(vals[0])
	if err != nil {
		return mkNull()
	}
	for _, v := range els {
		if truthy(v) {
			return mkBool(true)
		}
	}
	return mkBool(false)
}

// opAllElementsTrue reports whether every array element is truthy.
func opAllElementsTrue(vals []bson.RawValue) bson.RawValue {
	els, err := arrayElements(vals[0])
	if err != nil {
		return mkNull()
	}
	for _, v := range els {
		if !truthy(v) {
			return mkBool(false)
		}
	}
	return mkBool(true)
}
