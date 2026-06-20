package oracle

import (
	"slices"

	"github.com/tamnd/doc/bson"
)

// distinctField is the synthetic field a distinct value is wrapped under so a
// scalar can travel through the document-shaped Result. Both targets wrap the
// same way, so the diff compares the values directly.
const distinctField = "v"

// WrapDistinctValue frames one distinct value as a single-field document {v:
// value}, the shape both targets carry distinct results in.
func WrapDistinctValue(v bson.RawValue) bson.Raw {
	return bson.NewBuilder().AppendValue(distinctField, v).Build()
}

// NormalizeDistinctDocs sorts wrapped distinct documents by their value under the
// BSON total order and drops duplicates, so the reference and the subject present
// distinct results in the same deterministic order regardless of how each target
// produced them.
func NormalizeDistinctDocs(docs []bson.Raw) []bson.Raw {
	if len(docs) == 0 {
		return nil
	}
	slices.SortFunc(docs, func(a, b bson.Raw) int {
		av, _ := a.Lookup(distinctField)
		bv, _ := b.Lookup(distinctField)
		return bson.Compare(av, bv)
	})
	out := make([]bson.Raw, 0, len(docs))
	out = append(out, docs[0])
	for _, d := range docs[1:] {
		prev, _ := out[len(out)-1].Lookup(distinctField)
		cur, _ := d.Lookup(distinctField)
		if bson.Compare(prev, cur) != 0 {
			out = append(out, d)
		}
	}
	return out
}
