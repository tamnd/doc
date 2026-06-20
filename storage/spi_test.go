package storage

import (
	"testing"
)

func TestRIDEncodeDecodeRoundTrip(t *testing.T) {
	cases := []RID{
		{PageNo: 0, Slot: 0},
		{PageNo: 1, Slot: 0},
		{PageNo: 42, Slot: 7},
		{PageNo: 0xFFFFFFFE, Slot: 0xFFFE},
		{PageNo: 123456, Slot: 999},
	}
	for _, want := range cases {
		got := DecodeRID(want.Encode())
		if got != want {
			t.Fatalf("round-trip: got %+v, want %+v", got, want)
		}
	}
}

func TestRIDEncodingOrderPreserving(t *testing.T) {
	// The packed uint64 (page<<32 | slot) sorts in page-then-slot order, which
	// keeps heap-order scans monotonic (spec 2061 doc 04 §2.2).
	a := RID{PageNo: 1, Slot: 5}.Encode()
	b := RID{PageNo: 1, Slot: 6}.Encode()
	c := RID{PageNo: 2, Slot: 0}.Encode()
	if a >= b || b >= c {
		t.Fatalf("RID encoding is not page-then-slot monotonic: %d %d %d", a, b, c)
	}
}

func TestRIDEncodingMiddleBitsZero(t *testing.T) {
	// Slot occupies only the low 16 bits; bits 16..31 are reserved zero.
	v := RID{PageNo: 0xABCD, Slot: 0xFFFF}.Encode()
	if mid := (v >> 16) & 0xFFFF; mid != 0 {
		t.Fatalf("reserved middle bits = %#x, want 0", mid)
	}
}

func TestNullRID(t *testing.T) {
	if !NullRID.IsNull() {
		t.Fatal("NullRID should report IsNull")
	}
	if NullRID.IsValid() {
		t.Fatal("NullRID should not be valid")
	}
	if (RID{PageNo: 1, Slot: 0}).IsNull() {
		t.Fatal("a real RID should not report IsNull")
	}
	if !(RID{PageNo: 1, Slot: 0}).IsValid() {
		t.Fatal("a real RID should be valid")
	}
}

func TestRIDValidityPageZero(t *testing.T) {
	// Page 0 is the header page; no record cell ever lives there, so a RID into
	// page 0 is not a valid record pointer.
	if (RID{PageNo: 0, Slot: 0}).IsValid() {
		t.Fatal("RID on page 0 should not be valid")
	}
}
