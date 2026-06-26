//go:build race

package collection

// raceEnabled is true when the package is built with the race detector. The wall-clock
// latency-budget harness skips in this mode: race instrumentation slows every operation
// several fold and adds scheduling noise, so the measured percentiles say nothing about
// the latency the budgets are written against.
const raceEnabled = true

// allocRuns is the AllocsPerRun sample count under the race detector. A few hundred runs
// pin the per-op allocation budget as firmly as a few thousand without paying the race
// detector's per-op cost on every one.
const allocRuns = 200
