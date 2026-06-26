//go:build !unix

package wire

import "testing"

// capToFDLimit is a no-op on platforms without RLIMIT_NOFILE: the run proceeds at the
// requested count and relies on the host's default descriptor budget.
func capToFDLimit(_ *testing.T, want int) int { return want }
