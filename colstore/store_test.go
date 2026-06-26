package colstore

import (
	"testing"

	"github.com/tamnd/doc/bson"
)

// doc builds a small document with an int _id, a string category, and an int amount.
func mkRow(id int32, cat string, amt int32) bson.Raw {
	return bson.NewBuilder().
		AppendInt32("_id", id).
		AppendString("cat", cat).
		AppendInt32("amt", amt).
		Build()
}

func newAmtStore() *Store { return New(ModeTransactional, []string{"cat", "amt"}) }

func TestStoreGroupSum(t *testing.T) {
	s := newAmtStore()
	rows := []bson.Raw{
		mkRow(1, "a", 10), mkRow(2, "b", 20), mkRow(3, "a", 5),
		mkRow(4, "b", 1), mkRow(5, "a", 4),
	}
	for i, r := range rows {
		s.Insert(r, uint64(i+1))
	}
	s.Flush()

	groups := s.GroupBy(100, "cat", "amt", nil)
	want := map[string]float64{"a": 19, "b": 21}
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2", len(groups))
	}
	for _, g := range groups {
		if g.Key.Kind != KindString {
			t.Fatalf("group key kind = %v", g.Key.Kind)
		}
		if g.Sum != want[g.Key.S] {
			t.Fatalf("group %q sum = %v, want %v", g.Key.S, g.Sum, want[g.Key.S])
		}
	}
	// First-seen order is "a" then "b".
	if groups[0].Key.S != "a" || groups[1].Key.S != "b" {
		t.Fatalf("group order = %q,%q want a,b", groups[0].Key.S, groups[1].Key.S)
	}
}

func TestStoreAvgMinMaxCount(t *testing.T) {
	s := New(ModeTransactional, []string{"amt"})
	for i, amt := range []int32{2, 4, 6, 8} {
		s.Insert(bson.NewBuilder().AppendInt32("_id", int32(i)).AppendInt32("amt", amt).Build(), uint64(i+1))
	}
	s.Flush()
	g := s.GroupBy(100, "", "amt", nil)
	if len(g) != 1 {
		t.Fatalf("got %d groups, want 1 (whole collection)", len(g))
	}
	avg, ok := g[0].Avg()
	if !ok || avg != 5 {
		t.Fatalf("avg = %v ok=%v, want 5", avg, ok)
	}
	if g[0].Count != 4 {
		t.Fatalf("count = %d, want 4", g[0].Count)
	}
	if mn, _ := g[0].Min.AsFloat(); mn != 2 {
		t.Fatalf("min = %v, want 2", mn)
	}
	if mx, _ := g[0].Max.AsFloat(); mx != 8 {
		t.Fatalf("max = %v, want 8", mx)
	}
}

// TestStoreMVCCVisibility checks a reader at an old snapshot does not see rows
// written later, and sees a row that a later version deleted.
func TestStoreMVCCVisibility(t *testing.T) {
	s := New(ModeTransactional, []string{"amt"})
	s.Insert(mkRow(1, "a", 10), 5)
	s.Insert(mkRow(2, "a", 20), 10)
	s.Flush()

	if got := s.RowCount(7); got != 1 {
		t.Fatalf("snapshot 7 row count = %d, want 1 (only the v5 insert)", got)
	}
	if got := s.RowCount(10); got != 2 {
		t.Fatalf("snapshot 10 row count = %d, want 2", got)
	}

	// Delete row 1 at version 15. A reader at 12 still sees it; a reader at 15 does not.
	s.Delete(mkRow(1, "a", 10), 15)
	if got := s.RowCount(12); got != 2 {
		t.Fatalf("snapshot 12 after delete-at-15 = %d, want 2", got)
	}
	if got := s.RowCount(20); got != 1 {
		t.Fatalf("snapshot 20 after delete-at-15 = %d, want 1", got)
	}
}

// TestStoreUpdateTombstonesOld checks an update hides the old value and the sum at a
// new snapshot reflects only the new value.
func TestStoreUpdateTombstonesOld(t *testing.T) {
	s := New(ModeTransactional, []string{"amt"})
	s.Insert(mkRow(1, "a", 10), 5)
	s.Flush()
	old := mkRow(1, "a", 10)
	s.Update(old, mkRow(1, "a", 99), 10)
	s.Flush()

	if sum, _ := s.SumField(7, "amt", nil); sum != 10 {
		t.Fatalf("snapshot 7 sum = %v, want 10 (pre-update)", sum)
	}
	if sum, _ := s.SumField(12, "amt", nil); sum != 99 {
		t.Fatalf("snapshot 12 sum = %v, want 99 (post-update)", sum)
	}
}

// TestStoreZoneSkip builds enough rows to span several segments and confirms a range
// predicate prunes segments while still returning the correct rows.
func TestStoreZoneSkip(t *testing.T) {
	s := New(ModeTransactional, []string{"amt"})
	const total = SegmentSize * 4
	for i := 0; i < total; i++ {
		s.Insert(bson.NewBuilder().AppendInt32("_id", int32(i)).AppendInt32("amt", int32(i)).Build(), uint64(i+1))
	}
	s.Flush()
	if s.SegmentCount() != 4 {
		t.Fatalf("segment count = %d, want 4", s.SegmentCount())
	}

	// amt >= 3*SegmentSize keeps only the last segment's rows; the zone map must skip
	// the first three segments but the answer must still be exact.
	pred := &RangePred{Field: "amt", Op: "$gte", Bound: Value{Kind: KindInt, I: int64(3 * SegmentSize)}}
	sum, count := s.SumField(uint64(total+1), "amt", pred)
	if count != SegmentSize {
		t.Fatalf("predicate count = %d, want %d", count, SegmentSize)
	}
	var want float64
	for i := 3 * SegmentSize; i < total; i++ {
		want += float64(i)
	}
	if sum != want {
		t.Fatalf("predicate sum = %v, want %v", sum, want)
	}
}

// TestStoreCoversAndRebuild checks the covering test and the lazy-mode rebuild path.
func TestStoreCoversAndRebuild(t *testing.T) {
	s := New(ModeLazy, []string{"cat", "amt"})
	if !s.Covers([]string{"amt"}) || s.Covers([]string{"other"}) {
		t.Fatal("Covers wrong")
	}
	docs := []bson.Raw{mkRow(1, "a", 3), mkRow(2, "a", 7), mkRow(3, "b", 5)}
	s.Rebuild(docs, 50)
	g := s.GroupBy(100, "cat", "amt", nil)
	got := map[string]float64{}
	for _, x := range g {
		got[x.Key.S] = x.Sum
	}
	if got["a"] != 10 || got["b"] != 5 {
		t.Fatalf("rebuild group sums = %v, want a:10 b:5", got)
	}
	// Nothing is visible before the rebuild version.
	if n := s.RowCount(10); n != 0 {
		t.Fatalf("pre-rebuild snapshot saw %d rows, want 0", n)
	}
}

// TestSegmentPersistence round-trips a built segment through its serialized form.
func TestSegmentPersistence(t *testing.T) {
	vals := []Value{{Kind: KindInt, I: 100}, NullValue, {Kind: KindInt, I: 102}, {Kind: KindInt, I: 100}}
	rids := []uint64{10, 11, 12, 13}
	write := []uint64{1, 2, 3, 4}
	del := []uint64{0, 0, 7, 0}
	seg := buildSegment("amt", vals, rids, write, del)

	got, err := UnmarshalSegment(seg.MarshalBinary())
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Field != "amt" || got.FirstRID != 10 || got.n != 4 {
		t.Fatalf("header mismatch: %+v", got)
	}
	gv, err := got.Values()
	if err != nil {
		t.Fatalf("decode values: %v", err)
	}
	if !columnsEqual(vals, gv) {
		t.Fatalf("values mismatch: %v vs %v", vals, gv)
	}
	for i := range del {
		if got.del[i] != del[i] || got.write[i] != write[i] || got.rids[i] != rids[i] {
			t.Fatalf("entry %d metadata mismatch", i)
		}
	}
	if !got.hasZone {
		t.Fatal("zone map lost in round-trip")
	}
}

func TestUnmarshalSegmentCorrupt(t *testing.T) {
	for i, b := range [][]byte{{}, {0xff}, {0x01, 'x'}} {
		if _, err := UnmarshalSegment(b); err == nil {
			t.Fatalf("case %d: expected error", i)
		}
	}
}
