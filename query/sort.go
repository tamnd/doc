package query

import (
	"fmt"
	"sort"

	"github.com/tamnd/doc/bson"
)

// Sort orders result documents by one or more keys, each ascending or descending,
// using the BSON total order (spec 2061 doc 11 §6). The sort document's field
// order is the key precedence: {a:1,b:-1} sorts by a ascending, breaking ties by b
// descending. A missing sort field compares as the smallest value (it sorts first
// ascending), matching MongoDB's treatment of a missing field as a null-like
// minimum within the sort.
type Sort struct {
	keys []sortKey
}

type sortKey struct {
	path []string
	desc bool
}

// CompileSort parses a sort document. A nil or empty sort is a no-op. Each value
// must be 1 (ascending) or -1 (descending).
func CompileSort(s bson.Raw) (*Sort, error) {
	if len(s) == 0 {
		return &Sort{}, nil
	}
	elems, err := s.Elements()
	if err != nil {
		return nil, err
	}
	out := &Sort{keys: make([]sortKey, 0, len(elems))}
	for _, e := range elems {
		dir, ok := e.Value.AsFloat64()
		if !ok || (dir != 1 && dir != -1) {
			return nil, fmt.Errorf("%w: sort direction for %q must be 1 or -1", ErrBadQuery, e.Key)
		}
		out.keys = append(out.keys, sortKey{path: splitPath(e.Key), desc: dir == -1})
	}
	return out, nil
}

// Empty reports whether the sort imposes no ordering.
func (s *Sort) Empty() bool { return len(s.keys) == 0 }

// Apply sorts docs in place by the compiled keys. The sort is stable, so
// documents equal on every key keep their input order.
func (s *Sort) Apply(docs []bson.Raw) {
	if s.Empty() || len(docs) < 2 {
		return
	}
	sort.SliceStable(docs, func(i, j int) bool {
		return s.less(docs[i], docs[j])
	})
}

func (s *Sort) less(a, b bson.Raw) bool {
	for _, k := range s.keys {
		c := bson.Compare(sortValue(a, k.path), sortValue(b, k.path))
		if k.desc {
			c = -c
		}
		if c != 0 {
			return c < 0
		}
	}
	return false
}

// sortValue resolves a sort key against a document. A field that resolves to
// several values (an array field) sorts by its minimum for an ascending key, which
// the comparator approximates by taking the smallest resolved value; a missing
// field yields a synthetic null, the smallest non-MinKey value.
func sortValue(d bson.Raw, path []string) bson.RawValue {
	values, present := traverse(d, path)
	if !present || len(values) == 0 {
		return nullValue
	}
	min := values[0]
	for _, v := range values[1:] {
		if bson.Compare(v, min) < 0 {
			min = v
		}
	}
	return min
}

// nullValue is a standalone BSON null value used as the sort key for a missing
// field.
var nullValue = func() bson.RawValue {
	d := bson.NewBuilder().AppendNull("n").Build()
	v, _ := d.Lookup("n")
	return v
}()

func splitPath(key string) []string {
	if key == "" {
		return []string{""}
	}
	out := make([]string, 0, 4)
	start := 0
	for i := 0; i < len(key); i++ {
		if key[i] == '.' {
			out = append(out, key[start:i])
			start = i + 1
		}
	}
	return append(out, key[start:])
}
