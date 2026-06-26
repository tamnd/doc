//go:build race

package wire

// raceEnabled is true when the binary is built with -race. The race detector slows the accept
// loop by roughly an order of magnitude, so the scale test runs a smaller fan-out under it:
// the goal under -race is to catch data races in the connection lifecycle, which a few hundred
// connections exercise as well as ten thousand.
const raceEnabled = true
