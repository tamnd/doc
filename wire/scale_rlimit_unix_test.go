//go:build unix

package wire

import (
	"syscall"
	"testing"
)

// capToFDLimit makes sure the process can open enough file descriptors for `want`
// connections, raising the soft RLIMIT_NOFILE toward the hard limit when it can. The test
// uses two descriptors per connection (both ends live in this process) plus headroom for the
// listener, the database, and the runtime. If even the hard limit cannot cover `want`, the
// target is lowered to what fits and the shortfall is logged rather than failing, so the
// leak check still runs at whatever scale the host allows.
func capToFDLimit(t *testing.T, want int) int {
	t.Helper()
	const perConn = 2
	const headroom = 256
	need := uint64(want*perConn + headroom)

	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		t.Logf("getrlimit failed (%v); proceeding without adjustment", err)
		return want
	}
	if lim.Cur < need {
		raised := lim
		raised.Cur = lim.Max
		if raised.Cur > need {
			raised.Cur = need
		}
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &raised); err == nil {
			lim = raised
		}
	}
	if lim.Cur >= need {
		return want
	}
	fit := (int(lim.Cur) - headroom) / perConn
	if fit < 1 {
		fit = 1
	}
	t.Logf("fd limit %d caps the run at %d connections (wanted %d)", lim.Cur, fit, want)
	return fit
}
