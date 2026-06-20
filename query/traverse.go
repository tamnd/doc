package query

import "github.com/tamnd/doc/bson"

// traverse resolves a dotted path within a document and returns every value the
// path reaches, with MongoDB's implicit array traversal (spec 2061 doc 08 §12).
//
// The walk descends field by field. When a path component meets an array, two
// things happen: if the component is a numeric string it indexes the array
// positionally, and in every case the remaining path is also applied to each
// element (the array "fan-out"), so a path like "a.b" against {a:[{b:1},{b:2}]}
// yields both 1 and 2. The second return value reports whether the path resolved
// to at least one value; a false there is the "missing field" signal the field
// predicates use for the null/missing rule.
func traverse(doc bson.Raw, path []string) ([]bson.RawValue, bool) {
	var out []bson.RawValue
	root := bson.RawValue{Type: bson.TypeDocument, Data: doc}
	found := descend(root, path, &out)
	return out, found
}

func descend(v bson.RawValue, path []string, out *[]bson.RawValue) bool {
	if len(path) == 0 {
		*out = append(*out, v)
		return true
	}
	switch v.Type {
	case bson.TypeDocument:
		sub, ok := v.Document().Lookup(path[0])
		if !ok {
			return false
		}
		return descend(sub, path[1:], out)
	case bson.TypeArray:
		found := false
		if idx, ok := arrayIndex(path[0]); ok {
			if elem, ok := nthElement(v, idx); ok {
				if descend(elem, path[1:], out) {
					found = true
				}
			}
		}
		// Fan-out: apply the whole remaining path to each element so a field
		// name resolves through every array entry.
		for _, e := range arrayElems(v) {
			if descend(e, path, out) {
				found = true
			}
		}
		return found
	default:
		return false
	}
}

// arrayElems returns the element values of an array (or document) value in stored
// order. A non-array value yields no elements.
func arrayElems(v bson.RawValue) []bson.RawValue {
	if v.Type != bson.TypeArray && v.Type != bson.TypeDocument {
		return nil
	}
	elems, err := v.Document().Elements()
	if err != nil {
		return nil
	}
	out := make([]bson.RawValue, len(elems))
	for i, e := range elems {
		out[i] = e.Value
	}
	return out
}

// nthElement returns the value at position idx of an array value.
func nthElement(v bson.RawValue, idx int) (bson.RawValue, bool) {
	elems := arrayElems(v)
	if idx < 0 || idx >= len(elems) {
		return bson.RawValue{}, false
	}
	return elems[idx], true
}

// arrayIndex parses a path component as a non-negative array index. It accepts
// only canonical decimal digits with no leading zeros (other than "0" itself), so
// "01" is treated as a field name, matching MongoDB's index recognition.
func arrayIndex(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	if s == "0" {
		return 0, true
	}
	if s[0] == '0' {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}
