//go:build !race

package wire

// raceEnabled is false in an ordinary build, so the scale test runs the spec's full fan-out.
const raceEnabled = false
