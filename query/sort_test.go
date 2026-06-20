package query

import (
	"testing"

	"github.com/tamnd/doc/bson"
)

func idOf(t *testing.T, d bson.Raw) int32 {
	t.Helper()
	v, ok := d.Lookup("_id")
	if !ok {
		t.Fatal("missing _id")
	}
	return v.Int32()
}

func ids(t *testing.T, docs []bson.Raw) []int32 {
	out := make([]int32, len(docs))
	for i, d := range docs {
		out[i] = idOf(t, d)
	}
	return out
}

func eqInts(a, b []int32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// rec builds a record with an _id and an age.
func rec(id int32, age int32) bson.Raw {
	return doc(func(b *bson.Builder) { b.AppendInt32("_id", id) }, fInt("age", age))
}

func TestSortAscending(t *testing.T) {
	docs := []bson.Raw{rec(1, 30), rec(2, 10), rec(3, 20)}
	s, err := CompileSort(doc(fInt("age", 1)))
	if err != nil {
		t.Fatalf("CompileSort: %v", err)
	}
	s.Apply(docs)
	if got := ids(t, docs); !eqInts(got, []int32{2, 3, 1}) {
		t.Errorf("ascending sort ids = %v, want [2 3 1]", got)
	}
}

func TestSortDescending(t *testing.T) {
	docs := []bson.Raw{rec(1, 30), rec(2, 10), rec(3, 20)}
	s, _ := CompileSort(doc(func(b *bson.Builder) { b.AppendInt32("age", -1) }))
	s.Apply(docs)
	if got := ids(t, docs); !eqInts(got, []int32{1, 3, 2}) {
		t.Errorf("descending sort ids = %v, want [1 3 2]", got)
	}
}

func TestSortMultiKey(t *testing.T) {
	// Same age, tie-break by _id descending.
	docs := []bson.Raw{rec(1, 10), rec(3, 10), rec(2, 10)}
	s, _ := CompileSort(doc(fInt("age", 1), func(b *bson.Builder) { b.AppendInt32("_id", -1) }))
	s.Apply(docs)
	if got := ids(t, docs); !eqInts(got, []int32{3, 2, 1}) {
		t.Errorf("multi-key sort ids = %v, want [3 2 1]", got)
	}
}

func TestSortMissingFieldSortsFirst(t *testing.T) {
	withAge := rec(1, 50)
	noAge := doc(func(b *bson.Builder) { b.AppendInt32("_id", 2) })
	docs := []bson.Raw{withAge, noAge}
	s, _ := CompileSort(doc(fInt("age", 1)))
	s.Apply(docs)
	if got := ids(t, docs); !eqInts(got, []int32{2, 1}) {
		t.Errorf("missing field should sort first ascending: ids = %v, want [2 1]", got)
	}
}

func TestSortStable(t *testing.T) {
	// All equal on the sort key; input order must be preserved.
	docs := []bson.Raw{rec(5, 1), rec(4, 1), rec(6, 1)}
	s, _ := CompileSort(doc(fInt("age", 1)))
	s.Apply(docs)
	if got := ids(t, docs); !eqInts(got, []int32{5, 4, 6}) {
		t.Errorf("stable sort should preserve input order: ids = %v, want [5 4 6]", got)
	}
}

func TestSortBadDirection(t *testing.T) {
	if _, err := CompileSort(doc(fInt("age", 2))); err == nil {
		t.Error("sort direction other than 1/-1 should error")
	}
}
