package main

import (
	"testing"
	"time"
)

// TestSoakSmoke runs the soak harness for a couple of seconds with a small fleet so the
// test suite exercises the reader, writer, and monitor paths. The nightly job runs the
// same code with -duration 8h and the full fleet.
func TestSoakSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping soak smoke in -short")
	}
	if raceEnabled {
		t.Skip("soak degradation gate measures wall-clock latency, which the race detector makes too noisy to assert")
	}
	err := soak(2*time.Second, 500*time.Millisecond, 8, 2, 200, 0.50, 4096)
	if err != nil {
		t.Fatalf("soak smoke failed: %v", err)
	}
}
