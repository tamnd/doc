// Package catalog is doc's index catalog: the persistent registry of secondary
// indexes on a collection and the document-to-key extraction those indexes share
// with the write path and the planner (spec 2061 doc 09 §7, doc 07). The _id
// index is implicit and always present (it owns the file header's catalog-root
// slot); the catalog records only the secondary indexes a user creates, each with
// its key spec, options, multikey flag, and B-tree root page.
//
// M3-c persists single-field, compound, multikey, unique, sparse, partial, and
// TTL-tagged index definitions. The TTL background deleter, and the text, geo,
// wildcard, and hashed index kinds, are later milestones (spec 2061 doc 19 §22).
package catalog

import (
	"errors"
	"strings"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/index"
)

// Errors surfaced by catalog validation and DDL.
var (
	// ErrIndexExists reports a createIndex whose name or normalized key spec
	// already names an index on the collection.
	ErrIndexExists = errors.New("catalog: index already exists")
	// ErrIndexNotFound reports a dropIndex naming an absent index.
	ErrIndexNotFound = errors.New("catalog: index not found")
	// ErrCannotDropID reports an attempt to drop the mandatory _id index.
	ErrCannotDropID = errors.New("catalog: cannot drop the _id index")
	// ErrBadIndexSpec reports a malformed or unsupported index specification.
	ErrBadIndexSpec = errors.New("catalog: invalid index specification")
	// ErrParallelArrays reports a document with arrays at two different fields of
	// one compound index (spec 2061 doc 07 §7.4).
	ErrParallelArrays = errors.New("catalog: cannot index parallel arrays")
)

// IDIndexName is the reserved name of the always-present _id index.
const IDIndexName = "_id_"

// KeyPart is one field of an index key spec: a dotted field path and its sort
// direction (descending when Desc is true, MongoDB's -1).
type KeyPart struct {
	Field string
	Desc  bool
}

// IndexSpec describes one secondary index: its name, ordered key spec, options,
// learned multikey flag, and the page number of its B-tree root (NullPage until
// the first key is inserted).
type IndexSpec struct {
	Name               string
	Key                []KeyPart
	Unique             bool
	Sparse             bool
	PartialFilter      bson.Raw // nil = not a partial index
	ExpireAfterSeconds int64    // 0 = not a TTL index
	Multikey           bool
	Root               uint32
}

// DefaultName returns the MongoDB-style default index name for a key spec:
// each field joined to its numeric direction with underscores, e.g. {age:1}
// becomes "age_1" and {a:1,b:-1} becomes "a_1_b_-1".
func DefaultName(key []KeyPart) string {
	var b strings.Builder
	for i, p := range key {
		if i > 0 {
			b.WriteByte('_')
		}
		b.WriteString(p.Field)
		b.WriteByte('_')
		if p.Desc {
			b.WriteString("-1")
		} else {
			b.WriteString("1")
		}
	}
	return b.String()
}

// validate checks a spec for the constraints the catalog enforces at create time
// (spec 2061 doc 09 §8.5): a non-empty key, no empty field names, and the TTL
// restriction to a single field.
func (s *IndexSpec) validate() error {
	if len(s.Key) == 0 {
		return ErrBadIndexSpec
	}
	for _, p := range s.Key {
		if p.Field == "" {
			return ErrBadIndexSpec
		}
	}
	if s.ExpireAfterSeconds > 0 && len(s.Key) != 1 {
		return ErrBadIndexSpec
	}
	return nil
}

// Keys extracts the encoded index keys for a document under this spec, with
// multikey array expansion (spec 2061 doc 07 §7). It returns the per-document key
// list (one entry for a scalar index, one per array element for a multikey field),
// whether the document should be indexed at all (false only for a sparse index
// whose fields are all missing), and whether an array was expanded (so the caller
// can set the multikey flag). A document with arrays at two compound fields is
// rejected with ErrParallelArrays.
func (s *IndexSpec) Keys(doc bson.Raw) (keys [][]byte, indexed bool, multikey bool, err error) {
	perField := make([][]bson.RawValue, len(s.Key))
	anyPresent := false
	arrayField := -1
	for i, p := range s.Key {
		vals, present, sawArray := fieldValues(doc, p.Field)
		if present {
			anyPresent = true
		}
		if sawArray {
			if arrayField >= 0 {
				return nil, false, false, ErrParallelArrays
			}
			arrayField = i
			multikey = true
		}
		if len(vals) == 0 {
			// Missing field, or an empty array: index as a single null so the
			// document is still found by {field: null} (spec 2061 doc 07 §5.2).
			vals = []bson.RawValue{nullValue()}
		}
		perField[i] = vals
	}
	if s.Sparse && !anyPresent {
		return nil, false, multikey, nil
	}
	// Partial filter is evaluated by the caller (it needs the query package);
	// here we only build keys. Cartesian expansion is along the single array
	// field, since parallel arrays are rejected above.
	fan := 1
	if arrayField >= 0 {
		fan = len(perField[arrayField])
	}
	keys = make([][]byte, 0, fan)
	for j := 0; j < fan; j++ {
		var k []byte
		for i, p := range s.Key {
			v := perField[i][0]
			if i == arrayField {
				v = perField[i][j]
			}
			k, err = index.AppendField(k, v, p.Desc)
			if err != nil {
				return nil, false, multikey, err
			}
		}
		keys = append(keys, k)
	}
	return keys, true, multikey, nil
}

// fieldValues resolves a dotted path against a document with implicit array
// traversal, returning the leaf values (a terminal array fanned into its
// elements), whether the path was present, and whether any array was traversed
// or expanded (spec 2061 doc 07 §7.3).
func fieldValues(doc bson.Raw, path string) (vals []bson.RawValue, present bool, sawArray bool) {
	root := bson.RawValue{Type: bson.TypeDocument, Data: doc}
	present = descend(root, strings.Split(path, "."), &vals, &sawArray)
	return vals, present, sawArray
}

func descend(v bson.RawValue, parts []string, out *[]bson.RawValue, sawArray *bool) bool {
	if len(parts) == 0 {
		if v.Type == bson.TypeArray {
			*sawArray = true
			*out = append(*out, arrayElems(v)...)
		} else {
			*out = append(*out, v)
		}
		return true
	}
	switch v.Type {
	case bson.TypeDocument:
		sub, ok := v.Document().Lookup(parts[0])
		if !ok {
			return false
		}
		return descend(sub, parts[1:], out, sawArray)
	case bson.TypeArray:
		*sawArray = true
		any := false
		for _, e := range arrayElems(v) {
			if descend(e, parts, out, sawArray) {
				any = true
			}
		}
		return any
	default:
		return false
	}
}

// arrayElems returns the element values of a BSON array value, in order.
func arrayElems(v bson.RawValue) []bson.RawValue {
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

// nullValue returns the BSON null value used for missing indexed fields.
func nullValue() bson.RawValue { return bson.RawValue{Type: bson.TypeNull} }
