//go:build race

package main

// raceEnabled is true when the binary is built with the race detector. The soak smoke
// test skips its latency-degradation gate in this mode: race instrumentation adds enough
// scheduling noise that a clean run still drifts past the degradation threshold.
const raceEnabled = true
