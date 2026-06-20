package oracle

import (
	"strings"
	"testing"

	"github.com/tamnd/doc/catalog"
)

// indexableField returns a top-level filter field a read probe can be indexed on,
// or ok=false when the probe is not a read or its filter has no plain field. Dotted
// paths and _id are skipped: _id already has the implicit index, and a dotted path
// keeps the derived spec simple.
func indexableField(op Op) (string, bool) {
	switch op.Kind {
	case OpFind, OpFindOne, OpCount:
	default:
		return "", false
	}
	if len(op.Filter) == 0 {
		return "", false
	}
	elems, err := op.Filter.Elements()
	if err != nil {
		return "", false
	}
	for _, e := range elems {
		if e.Key == "_id" || strings.HasPrefix(e.Key, "$") || strings.Contains(e.Key, ".") {
			continue
		}
		return e.Key, true
	}
	return "", false
}

// runProbe runs a case's setup, then optionally creates an index, then runs the
// probe and returns its result.
func runProbe(t *testing.T, c Case, idx *Op) Result {
	t.Helper()
	target := NewDocTarget(fixedGen())
	defer target.Close()
	if err := target.Reset(); err != nil {
		t.Fatalf("reset for %q: %v", c.Name, err)
	}
	for i, op := range c.Setup {
		if _, err := target.Exec(op); err != nil {
			t.Fatalf("%q setup[%d]: %v", c.Name, i, err)
		}
	}
	if idx != nil {
		if _, err := target.Exec(*idx); err != nil {
			t.Fatalf("%q createIndex: %v", c.Name, err)
		}
	}
	res, err := target.Exec(c.Probe)
	if err != nil {
		t.Fatalf("%q probe: %v", c.Name, err)
	}
	return res
}

// TestSecondaryIndexDoesNotChangeResults runs every read probe in the corpus twice:
// once with no secondary index, and once after building an index on a field the
// filter touches. The two results must be identical, since a secondary index is a
// pure access-path optimization that the residual filter still backs (spec 2061
// doc 11 §5).
func TestSecondaryIndexDoesNotChangeResults(t *testing.T) {
	checked := 0
	for _, c := range Corpus() {
		field, ok := indexableField(c.Probe)
		if !ok {
			continue
		}
		base := runProbe(t, c, nil)
		idxOp := Op{
			Kind:       OpCreateIndex,
			Collection: c.Probe.Collection,
			Index:      &IndexModel{Key: []catalog.KeyPart{{Field: field}}},
		}
		withIndex := runProbe(t, c, &idxOp)
		if detail, eq := compare(base, withIndex); !eq {
			t.Errorf("case %q field %q: index changed results: %s", c.Name, field, detail)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no corpus read probe was eligible for an index, the test covered nothing")
	}
	t.Logf("checked %d corpus probes for index result-equivalence", checked)
}
