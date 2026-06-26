//go:build linux

package wire

import "os"

// openFDCount returns the number of file descriptors this process holds open, read from the
// /proc/self/fd directory. The boolean is false when the count cannot be taken.
func openFDCount() (int, bool) {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0, false
	}
	// One of the entries is the directory handle opened to read /proc/self/fd itself; the
	// off-by-one does not matter against the slack the caller compares with.
	return len(entries), true
}
