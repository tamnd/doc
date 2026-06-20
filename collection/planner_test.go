package collection

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/catalog"
)

// filterInt builds {field: v}.
func filterInt(field string, v int32) bson.Raw {
	return bson.NewBuilder().AppendInt32(field, v).Build()
}

// filterStr builds {field: s}.
func filterStr(field, s string) bson.Raw {
	return bson.NewBuilder().AppendString(field, s).Build()
}

// stageContains walks a plan-tree node down the inputStage chain looking for a
// stage with the given name.
func stageContains(node bson.Raw, want string) bool {
	if st, ok := node.Lookup("stage"); ok && st.StringValue() == want {
		return true
	}
	child, ok := node.Lookup("inputStage")
	if !ok {
		return false
	}
	return stageContains(child.Document(), want)
}

// hasStage reports whether the winning plan in an explain document contains a stage
// with the given name.
func hasStage(t *testing.T, explain bson.Raw, want string) bool {
	t.Helper()
	qp, ok := explain.Lookup("queryPlanner")
	if !ok {
		t.Fatal("explain has no queryPlanner")
	}
	wp, ok := qp.Document().Lookup("winningPlan")
	if !ok {
		t.Fatal("explain has no winningPlan")
	}
	return stageContains(wp.Document(), want)
}

// TestPlannerPicksIndexForSelectiveEquality checks the planner scans the whole
// collection when no index helps, then switches to an index scan once a selective
// equality index exists.
func TestPlannerPicksIndexForSelectiveEquality(t *testing.T) {
	c := newTestColl(t)
	for i := int32(1); i <= 50; i++ {
		if _, err := c.InsertOne(docInt(i, "age", i)); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	filter := filterInt("age", 25)

	ex, err := c.Explain(filter, FindOptions{}, "queryPlanner")
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if !hasStage(t, ex, "COLLSCAN") {
		t.Fatal("expected a collection scan before the index exists")
	}
	if hasStage(t, ex, "IXSCAN") {
		t.Fatal("did not expect an index scan before the index exists")
	}

	if _, err := c.CreateIndex(IndexModel{Key: []catalog.KeyPart{{Field: "age"}}}); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}

	ex, err = c.Explain(filter, FindOptions{}, "queryPlanner")
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if !hasStage(t, ex, "IXSCAN") {
		t.Fatal("expected an index scan once the index exists")
	}

	docs, err := c.Find(filter)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("Find returned %d docs, want 1", len(docs))
	}
	if v, _ := docs[0].Lookup("age"); v.Int32() != 25 {
		t.Fatalf("age = %d, want 25", v.Int32())
	}
}

// TestCoveredScanMatchesFetch checks that a covered index scan, which reconstructs
// the projected field from the index key without touching the heap, returns the same
// documents as the equivalent fetch-based scan.
func TestCoveredScanMatchesFetch(t *testing.T) {
	c := newTestColl(t)
	cities := []string{"paris", "tokyo", "paris", "rome", "paris"}
	for i, city := range cities {
		if _, err := c.InsertOne(docStr(int32(i+1), "city", city)); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	filter := filterStr("city", "paris")
	proj := bson.NewBuilder().AppendInt32("city", 1).AppendInt32("_id", 0).Build()
	opts := FindOptions{Projection: proj}

	want, err := c.FindWith(filter, opts)
	if err != nil {
		t.Fatalf("FindWith before index: %v", err)
	}

	if _, err := c.CreateIndex(IndexModel{Key: []catalog.KeyPart{{Field: "city"}}}); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}

	ex, err := c.Explain(filter, opts, "queryPlanner")
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if !hasStage(t, ex, "PROJECTION_COVERED") {
		t.Fatal("expected a covered projection stage")
	}
	if hasStage(t, ex, "FETCH") {
		t.Fatal("a covered scan must not fetch from the heap")
	}

	got, err := c.FindWith(filter, opts)
	if err != nil {
		t.Fatalf("FindWith after index: %v", err)
	}
	if len(got) != len(want) || len(got) != 3 {
		t.Fatalf("covered scan returned %d docs, fetch returned %d, want 3", len(got), len(want))
	}
	for i := range got {
		if !bytes.Equal(got[i], want[0]) {
			t.Fatalf("covered doc[%d] = %v, want %v", i, got[i], want[0])
		}
	}
}

// TestMultikeyEqualityDedupes checks that an equality match on a multikey index
// finds every document holding the value and returns each document once, even when
// several array entries point back to the same document.
func TestMultikeyEqualityDedupes(t *testing.T) {
	c := newTestColl(t)
	if _, err := c.InsertOne(docArr(1, "tags", 3)); err != nil { // [0,1,2]
		t.Fatalf("insert: %v", err)
	}
	if _, err := c.InsertOne(docArr(2, "tags", 5)); err != nil { // [0,1,2,3,4]
		t.Fatalf("insert: %v", err)
	}
	name, err := c.CreateIndex(IndexModel{Key: []catalog.KeyPart{{Field: "tags"}}})
	if err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	if got := c.entryCount(name); got != 8 {
		t.Fatalf("multikey entry count = %d, want 8", got)
	}

	only2, err := c.Find(filterInt("tags", 4))
	if err != nil {
		t.Fatalf("Find tags=4: %v", err)
	}
	if len(only2) != 1 {
		t.Fatalf("tags=4 matched %d docs, want 1", len(only2))
	}

	both, err := c.Find(filterInt("tags", 1))
	if err != nil {
		t.Fatalf("Find tags=1: %v", err)
	}
	if len(both) != 2 {
		t.Fatalf("tags=1 matched %d docs, want 2", len(both))
	}
}

// TestIndexHeapConsistencyAfterRandomOps inserts documents, builds an index, then
// applies a random sequence of deletes and field updates. After each batch the index
// access path must return exactly the documents a full scan would, and the index
// entry count must equal the live document count.
func TestIndexHeapConsistencyAfterRandomOps(t *testing.T) {
	c := newTestColl(t)
	r := rand.New(rand.NewSource(1))
	const n = 60
	live := make(map[int32]int32) // id -> age
	for i := int32(1); i <= n; i++ {
		age := int32(r.Intn(10))
		if _, err := c.InsertOne(docInt(i, "age", age)); err != nil {
			t.Fatalf("insert: %v", err)
		}
		live[i] = age
	}
	name, err := c.CreateIndex(IndexModel{Key: []catalog.KeyPart{{Field: "age"}}})
	if err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}

	for range 120 {
		id := int32(r.Intn(n) + 1)
		if r.Intn(3) == 0 {
			if _, err := c.DeleteOne(filterID(id)); err != nil {
				t.Fatalf("delete: %v", err)
			}
			delete(live, id)
			continue
		}
		newAge := int32(r.Intn(10))
		set := bson.NewBuilder().AppendInt32("age", newAge).Build()
		upd := bson.NewBuilder().AppendDocument("$set", set).Build()
		res, err := c.UpdateOne(filterID(id), upd)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if res.Matched > 0 {
			live[id] = newAge
		}
	}

	if got := c.entryCount(name); got != uint64(len(live)) {
		t.Fatalf("index entry count = %d, want %d live documents", got, len(live))
	}
	for v := range int32(10) {
		got, err := c.Find(filterInt("age", v))
		if err != nil {
			t.Fatalf("Find age=%d: %v", v, err)
		}
		want := 0
		for _, age := range live {
			if age == v {
				want++
			}
		}
		if len(got) != want {
			t.Errorf("age=%d: index path returned %d docs, want %d", v, len(got), want)
		}
	}
}
