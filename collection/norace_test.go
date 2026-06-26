//go:build !race

package collection

// raceEnabled is false in an ordinary build, so the latency-budget harness measures and
// asserts its percentiles normally.
const raceEnabled = false
