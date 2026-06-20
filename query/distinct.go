package query

import "github.com/tamnd/doc/bson"

// Distinct returns the distinct field values a path reaches across a document, in
// the form MongoDB's distinct command produces them: the path is resolved with
// implicit array traversal (so "a.b" descends through arrays of sub-documents),
// and a resolved value that is itself an array is unwound into its elements (so
// distinct over an array field yields the element values, not the array). Callers
// accumulate and deduplicate the values across all matching documents.
//
// The returned values alias doc's bytes; clone doc to retain them past its
// lifetime.
func Distinct(doc bson.Raw, field string) []bson.RawValue {
	resolved, _ := traverse(doc, splitPath(field))
	var out []bson.RawValue
	for _, v := range resolved {
		if v.Type == bson.TypeArray {
			out = append(out, arrayElems(v)...)
			continue
		}
		out = append(out, v)
	}
	return out
}
