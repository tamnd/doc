//go:build !linux

package wire

// openFDCount cannot enumerate descriptors portably off Linux, so it reports that no count is
// available and the caller skips the file-descriptor assertion. The goroutine-leak check, the
// stronger signal here since each connection goroutine owns its descriptor, still runs.
func openFDCount() (int, bool) { return 0, false }
