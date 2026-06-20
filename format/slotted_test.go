package format

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

func newHeapPage(t *testing.T, ps uint32) SlottedPage {
	t.Helper()
	p := make([]byte, ps)
	InitPage(p, PageHeap, 1)
	return OpenSlotted(p)
}

func TestSlottedAddAndRead(t *testing.T) {
	s := newHeapPage(t, PageSize8K)
	payloads := [][]byte{
		[]byte("alpha"),
		[]byte("a much longer payload than the first one"),
		{},
		bytes.Repeat([]byte{0xAB}, 1000),
	}
	slots := make([]int, len(payloads))
	for i, p := range payloads {
		slot, err := s.AddCell(p)
		if err != nil {
			t.Fatalf("AddCell %d: %v", i, err)
		}
		slots[i] = slot
		if slot != i {
			t.Fatalf("slot %d = %d, want %d (monotonic)", i, slot, i)
		}
	}
	for i, p := range payloads {
		got, err := s.Cell(slots[i])
		if err != nil {
			t.Fatalf("Cell %d: %v", i, err)
		}
		if !bytes.Equal(got, p) {
			t.Fatalf("cell %d = %q, want %q", i, got, p)
		}
	}
	if s.SlotCount() != len(payloads) {
		t.Fatalf("SlotCount = %d, want %d", s.SlotCount(), len(payloads))
	}
}

func TestSlottedFreeBytesAccounting(t *testing.T) {
	s := newHeapPage(t, PageSize8K)
	before := s.FreeBytes()
	if before != BodySize(PageSize8K) {
		t.Fatalf("initial FreeBytes = %d, want %d", before, BodySize(PageSize8K))
	}
	_, err := s.AddCell(bytes.Repeat([]byte{1}, 100))
	if err != nil {
		t.Fatal(err)
	}
	after := s.FreeBytes()
	// One 100-byte cell plus one 4-byte slot consumed.
	if before-after != 104 {
		t.Fatalf("consumed %d bytes, want 104", before-after)
	}
}

func TestSlottedDeleteAndLiveness(t *testing.T) {
	s := newHeapPage(t, PageSize8K)
	for i := 0; i < 5; i++ {
		if _, err := s.AddCell([]byte(fmt.Sprintf("row-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteCell(2); err != nil {
		t.Fatal(err)
	}
	if s.IsLive(2) {
		t.Fatal("slot 2 should be dead after delete")
	}
	if _, err := s.Cell(2); !errors.Is(err, ErrSlotDead) {
		t.Fatalf("reading dead slot: err = %v, want ErrSlotDead", err)
	}
	// Surviving slots keep their numbers and contents.
	for _, i := range []int{0, 1, 3, 4} {
		if !s.IsLive(i) {
			t.Fatalf("slot %d should still be live", i)
		}
		got, _ := s.Cell(i)
		if string(got) != fmt.Sprintf("row-%d", i) {
			t.Fatalf("slot %d = %q after delete of 2", i, got)
		}
	}
}

func TestSlottedBadSlot(t *testing.T) {
	s := newHeapPage(t, PageSize8K)
	if _, err := s.Cell(0); !errors.Is(err, ErrBadSlot) {
		t.Fatalf("err = %v, want ErrBadSlot", err)
	}
	if err := s.DeleteCell(99); !errors.Is(err, ErrBadSlot) {
		t.Fatalf("err = %v, want ErrBadSlot", err)
	}
}

func TestSlottedCompactReclaimsAndKeepsRIDs(t *testing.T) {
	s := newHeapPage(t, PageSize8K)
	// Fill with 200-byte cells.
	const cellSize = 200
	for i := 0; i < 10; i++ {
		if _, err := s.AddCell(bytes.Repeat([]byte{byte(i)}, cellSize)); err != nil {
			t.Fatal(err)
		}
	}
	// Delete the even slots, leaving holes.
	for i := 0; i < 10; i += 2 {
		if err := s.DeleteCell(i); err != nil {
			t.Fatal(err)
		}
	}
	freeBefore := s.FreeBytes()
	s.Compact()
	freeAfter := s.FreeBytes()
	if freeAfter <= freeBefore {
		t.Fatalf("compaction did not increase contiguous free space: %d -> %d", freeBefore, freeAfter)
	}
	// Live slots keep their numbers and exact bytes after compaction.
	for i := 1; i < 10; i += 2 {
		if !s.IsLive(i) {
			t.Fatalf("slot %d should be live post-compaction", i)
		}
		got, err := s.Cell(i)
		if err != nil {
			t.Fatalf("Cell %d post-compaction: %v", i, err)
		}
		if !bytes.Equal(got, bytes.Repeat([]byte{byte(i)}, cellSize)) {
			t.Fatalf("slot %d corrupted by compaction", i)
		}
	}
}

func TestSlottedAddTriggersCompaction(t *testing.T) {
	// A small page where reuse of deleted space requires compaction.
	s := newHeapPage(t, PageSize4K)
	body := BodySize(PageSize4K)
	// Each cell ~ body/4 so four fit; deleting then re-adding forces compaction.
	cell := body/4 - 8
	for i := 0; i < 4; i++ {
		if _, err := s.AddCell(bytes.Repeat([]byte{byte(i)}, cell)); err != nil {
			t.Fatalf("initial add %d: %v", i, err)
		}
	}
	// Delete two non-adjacent cells; their space is now fragmented holes.
	if err := s.DeleteCell(0); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteCell(2); err != nil {
		t.Fatal(err)
	}
	// A new cell that fits only after compaction reclaims the holes.
	if _, err := s.AddCell(bytes.Repeat([]byte{0x55}, cell)); err != nil {
		t.Fatalf("add after deletes should succeed via compaction: %v", err)
	}
}

func TestSlottedNoSpace(t *testing.T) {
	s := newHeapPage(t, PageSize4K)
	// One cell that nearly fills the page.
	big := BodySize(PageSize4K) - 8
	if _, err := s.AddCell(bytes.Repeat([]byte{1}, big)); err != nil {
		t.Fatalf("first big add: %v", err)
	}
	// A second cell cannot fit.
	if _, err := s.AddCell([]byte("x")); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("err = %v, want ErrNoSpace", err)
	}
}

func TestSlottedUpdateInPlace(t *testing.T) {
	s := newHeapPage(t, PageSize8K)
	slot, _ := s.AddCell([]byte("hello"))
	if err := s.UpdateInPlace(slot, []byte("world")); err != nil {
		t.Fatalf("same-size update: %v", err)
	}
	got, _ := s.Cell(slot)
	if string(got) != "world" {
		t.Fatalf("cell = %q, want world", got)
	}
	// A different length cannot be updated in place.
	if err := s.UpdateInPlace(slot, []byte("longer")); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("err = %v, want ErrNoSpace", err)
	}
}

func TestSlottedReplaceCell(t *testing.T) {
	s := newHeapPage(t, PageSize8K)
	a, _ := s.AddCell([]byte("alpha"))
	b, _ := s.AddCell([]byte("bravo"))

	// Grow slot a in place: its RID (slot number) is unchanged, its neighbor
	// survives, and the new bytes read back.
	if err := s.ReplaceCell(a, []byte("a much longer alpha payload")); err != nil {
		t.Fatalf("grow replace: %v", err)
	}
	got, _ := s.Cell(a)
	if string(got) != "a much longer alpha payload" {
		t.Fatalf("cell a = %q after grow", got)
	}
	if got, _ := s.Cell(b); string(got) != "bravo" {
		t.Fatalf("neighbor b = %q, want bravo", got)
	}

	// Shrink it back: still the same slot, neighbor still intact.
	if err := s.ReplaceCell(a, []byte("hi")); err != nil {
		t.Fatalf("shrink replace: %v", err)
	}
	if got, _ := s.Cell(a); string(got) != "hi" {
		t.Fatalf("cell a = %q after shrink", got)
	}
	if got, _ := s.Cell(b); string(got) != "bravo" {
		t.Fatalf("neighbor b = %q after shrink", got)
	}
}

func TestSlottedReplaceCellNoSpace(t *testing.T) {
	s := newHeapPage(t, PageSize4K)
	a, _ := s.AddCell([]byte("x"))
	// Fill the rest of the page so a's cell cannot grow even after compaction.
	for s.FreeBytes() > 8 {
		if _, err := s.AddCell(make([]byte, 64)); err != nil {
			break
		}
	}
	big := make([]byte, PageSize4K) // larger than any page can hold
	if err := s.ReplaceCell(a, big); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("err = %v, want ErrNoSpace", err)
	}
	// The original cell must be untouched after a failed replace.
	if got, _ := s.Cell(a); string(got) != "x" {
		t.Fatalf("cell a = %q after failed replace, want x", got)
	}
}

func TestSlottedForwarding(t *testing.T) {
	s := newHeapPage(t, PageSize8K)
	slot, _ := s.AddCell([]byte("original record bytes that are long enough"))
	if err := s.SetForward(slot, 77, 5); err != nil {
		t.Fatalf("SetForward: %v", err)
	}
	if !s.IsForward(slot) {
		t.Fatal("slot should be a forwarding tombstone")
	}
	if s.IsLive(slot) {
		t.Fatal("forwarding slot should not be live")
	}
	page, target, ok := s.Forward(slot)
	if !ok || page != 77 || target != 5 {
		t.Fatalf("Forward = (%d,%d,%v), want (77,5,true)", page, target, ok)
	}
	// Forwarding survives compaction.
	s.Compact()
	page, target, ok = s.Forward(slot)
	if !ok || page != 77 || target != 5 {
		t.Fatalf("Forward post-compaction = (%d,%d,%v), want (77,5,true)", page, target, ok)
	}
}

func TestSlottedFillManySmallCells(t *testing.T) {
	s := newHeapPage(t, PageSize8K)
	n := 0
	for {
		_, err := s.AddCell([]byte("12345678"))
		if errors.Is(err, ErrNoSpace) {
			break
		}
		if err != nil {
			t.Fatalf("unexpected error at cell %d: %v", n, err)
		}
		n++
		if n > 10000 {
			t.Fatal("page never filled")
		}
	}
	// Every cell remains readable.
	for i := 0; i < n; i++ {
		got, err := s.Cell(i)
		if err != nil {
			t.Fatalf("Cell %d: %v", i, err)
		}
		if string(got) != "12345678" {
			t.Fatalf("cell %d = %q", i, got)
		}
	}
}

func BenchmarkSlottedAddCell(b *testing.B) {
	payload := bytes.Repeat([]byte{0xCD}, 64)
	for i := 0; i < b.N; i++ {
		p := make([]byte, PageSize8K)
		InitPage(p, PageHeap, 1)
		s := OpenSlotted(p)
		for {
			if _, err := s.AddCell(payload); errors.Is(err, ErrNoSpace) {
				break
			}
		}
	}
}
