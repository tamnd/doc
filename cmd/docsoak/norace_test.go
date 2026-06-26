//go:build !race

package main

// raceEnabled is false in an ordinary build, so the soak smoke test runs its full
// latency-degradation gate.
const raceEnabled = false
