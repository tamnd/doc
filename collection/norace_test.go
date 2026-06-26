//go:build !race

package collection

// raceEnabled is false in an ordinary build, so the latency-budget harness measures and
// asserts its percentiles normally.
const raceEnabled = false

// allocRuns is the AllocsPerRun sample count in an ordinary build, kept high so the
// per-op allocation budget is measured against a stable average.
const allocRuns = 2000
