package sys

import (
	"testing"
	"time"
)

func TestManualClock(t *testing.T) {
	start := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	c := NewManualClock(start)
	if !c.Now().Equal(start) {
		t.Fatalf("Now = %v, want %v", c.Now(), start)
	}
	c.Advance(90 * time.Minute)
	if got := c.Now(); !got.Equal(start.Add(90 * time.Minute)) {
		t.Fatalf("after advance Now = %v", got)
	}
	c.Set(start)
	if !c.Now().Equal(start) {
		t.Fatal("Set did not reset clock")
	}
}

func TestSystemClockUTC(t *testing.T) {
	now := SystemClock{}.Now()
	if now.Location() != time.UTC {
		t.Fatalf("SystemClock.Now location = %v, want UTC", now.Location())
	}
	if d := time.Since(now); d < -time.Second || d > time.Minute {
		t.Fatalf("SystemClock.Now too far from real now: %v", d)
	}
}

func TestObjectIDFields(t *testing.T) {
	var id ObjectID
	if !id.IsZero() {
		t.Fatal("zero ObjectID should report IsZero")
	}
	id[0] = 0x01
	if id.IsZero() {
		t.Fatal("non-zero ObjectID should not report IsZero")
	}
	// Hex is 24 lowercase hex chars.
	h := ObjectID{0xAB, 0xCD}.Hex()
	if len(h) != 24 {
		t.Fatalf("Hex len = %d, want 24", len(h))
	}
	if h[:4] != "abcd" {
		t.Fatalf("Hex prefix = %q, want abcd", h[:4])
	}
}

func TestObjectIDTimestamp(t *testing.T) {
	clk := NewManualClock(time.Unix(1_700_000_000, 0).UTC())
	g := NewObjectIDGenerator(clk)
	id := g.NewID()
	if got := id.Timestamp(); got != 1_700_000_000 {
		t.Fatalf("Timestamp = %d, want 1700000000", got)
	}
}

func TestObjectIDGeneratorUniqueAndMonotonic(t *testing.T) {
	clk := NewManualClock(time.Unix(1_700_000_000, 0).UTC())
	g := NewObjectIDGenerator(clk)
	seen := make(map[ObjectID]bool)
	var prevCounter uint32
	for i := 0; i < 1000; i++ {
		id := g.NewID()
		if seen[id] {
			t.Fatalf("duplicate id at %d: %s", i, id.Hex())
		}
		seen[id] = true
		// The counter (last 3 bytes) increments by one each call within a second.
		c := uint32(id[9])<<16 | uint32(id[10])<<8 | uint32(id[11])
		if i > 0 && c != prevCounter+1 {
			t.Fatalf("counter not monotonic at %d: %d after %d", i, c, prevCounter)
		}
		prevCounter = c
	}
}

func TestFixedIDGeneratorDeterministic(t *testing.T) {
	g1 := &FixedIDGenerator{Timestamp: 42, Prefix: [5]byte{1, 2, 3, 4, 5}}
	g2 := &FixedIDGenerator{Timestamp: 42, Prefix: [5]byte{1, 2, 3, 4, 5}}
	for i := 0; i < 100; i++ {
		if g1.NewID() != g2.NewID() {
			t.Fatalf("FixedIDGenerator not reproducible at %d", i)
		}
	}
	// First id has counter 1, timestamp 42.
	g := &FixedIDGenerator{Timestamp: 42}
	id := g.NewID()
	if id.Timestamp() != 42 {
		t.Fatalf("timestamp = %d, want 42", id.Timestamp())
	}
	if id[11] != 1 {
		t.Fatalf("first counter byte = %d, want 1", id[11])
	}
}
