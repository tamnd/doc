package wire

import (
	"encoding/binary"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tamnd/doc/bson"
)

// TestScaleConcurrentConnections holds a large number of connections open at once, each past
// the handshake, and proves the server carries them without leaking goroutines or file
// descriptors once they close. The spec's exit criterion for M8-e is 10,000 concurrent
// connections (doc 19 §22); the full count runs only outside -short, where the default count
// is small enough to stay friendly on a developer laptop.
//
// The count is the spec's 10,000 by default and can be overridden with DOC_SCALE_CONNS for a
// heavier or lighter run. Under -short it drops to 200 so the standard test pass stays quick.
func TestScaleConcurrentConnections(t *testing.T) {
	target := 10000
	if testing.Short() {
		target = 200
	}
	if raceEnabled && target > 1000 {
		target = 1000
	}
	if v := os.Getenv("DOC_SCALE_CONNS"); v != "" {
		if n, err := parsePositive(v); err == nil {
			target = n
		}
	}
	target = capToFDLimit(t, target)

	// MaxConns has to clear the target, and the idle timeout stays off so a parked connection
	// is not closed underneath the test while the fan-out is still ramping up.
	srv, addr := startServer(t, Options{MaxConns: target + 16})

	baselineGo := stableGoroutines()
	baselineFD, fdOK := openFDCount()

	var (
		wg       sync.WaitGroup
		dialErrs atomic.Int64
		hsErrs   atomic.Int64
		conns    = make([]net.Conn, target)
	)

	// Phase one: open every connection and complete its handshake. Each connection is held
	// open (not closed) so all `target` of them are live at the same moment. The number of
	// dials in flight at once is bounded so the listener's accept backlog keeps up; blasting
	// all `target` dials simultaneously overflows the kernel's accept queue and the surplus
	// clients see connection-refused. Holding the connection open after the handshake is
	// separate from that bound, so the live set still climbs to `target`.
	sem := make(chan struct{}, 128)
	for i := 0; i < target; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			nc := dialWithRetry(addr)
			<-sem
			if nc == nil {
				dialErrs.Add(1)
				return
			}
			conns[i] = nc
			if !helloOK(nc) {
				hsErrs.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if n := dialErrs.Load(); n != 0 {
		t.Fatalf("%d of %d connections failed to dial", n, target)
	}
	if n := hsErrs.Load(); n != 0 {
		t.Fatalf("%d of %d connections failed the handshake", n, target)
	}
	waitFor(t, func() bool { return srv.ConnCount() == target }, 30*time.Second,
		"all connections to register")

	if fdOK {
		if peak, ok := openFDCount(); ok && peak < baselineFD+target/2 {
			t.Fatalf("fd count at peak (%d) did not rise with %d live connections from baseline %d",
				peak, target, baselineFD)
		}
	}

	// Phase two: close every connection and wait for the server to account for it. A leak
	// shows up here as a ConnCount that never returns to zero.
	for _, nc := range conns {
		if nc != nil {
			_ = nc.Close()
		}
	}
	waitFor(t, func() bool { return srv.ConnCount() == 0 }, 30*time.Second,
		"all connections to drain")

	// The per-connection goroutines must unwind. The comparison is against a stable baseline
	// taken before the fan-out; a small slack absorbs scheduler and runtime background work.
	endGo := stableGoroutines()
	if leaked := endGo - baselineGo; leaked > target/10+16 {
		t.Fatalf("goroutine leak: baseline %d, after drain %d (leaked %d)", baselineGo, endGo, leaked)
	}

	// File descriptors must come back too: the post-drain count falls near the baseline
	// rather than staying at the peak.
	if fdOK {
		if after, ok := openFDCount(); ok && after > baselineFD+target/10+16 {
			t.Fatalf("fd leak: baseline %d, after drain %d", baselineFD, after)
		}
	}
}

// TestScaleRejectsBeyondCap confirms the connection cap holds under a concurrent rush: with a
// small MaxConns and many simultaneous dialers, the accepted set never exceeds the cap and
// the overflow is counted as rejected, with no goroutine left behind once everything closes.
func TestScaleRejectsBeyondCap(t *testing.T) {
	const limit = 32
	const rush = 256
	srv, addr := startServer(t, Options{MaxConns: limit})

	baselineGo := stableGoroutines()
	var wg sync.WaitGroup
	conns := make([]net.Conn, rush)
	for i := 0; i < rush; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			nc, err := net.Dial("tcp", addr)
			if err != nil {
				return
			}
			conns[i] = nc
			// Read once: an accepted connection blocks here, an over-cap one reads EOF at once.
			_ = nc.SetReadDeadline(time.Now().Add(2 * time.Second))
			var one [1]byte
			_, _ = nc.Read(one[:])
		}(i)
	}
	wg.Wait()

	if got := srv.ConnCount(); got > limit {
		t.Fatalf("ConnCount = %d, exceeds cap %d", got, limit)
	}
	if rej := srv.Rejected(); rej == 0 {
		t.Fatalf("Rejected = 0, want > 0 under a %d-deep rush at cap %d", rush, limit)
	}

	for _, nc := range conns {
		if nc != nil {
			_ = nc.Close()
		}
	}
	waitFor(t, func() bool { return srv.ConnCount() == 0 }, 10*time.Second, "rush to drain")
	endGo := stableGoroutines()
	if leaked := endGo - baselineGo; leaked > limit {
		t.Fatalf("goroutine leak after rush: baseline %d, after %d", baselineGo, endGo)
	}
}

// dialWithRetry opens a connection, retrying a few times on the transient connection-refused
// that a saturated accept backlog produces under a heavy fan-out. It returns nil only after
// the attempts are exhausted.
func dialWithRetry(addr string) net.Conn {
	backoff := 2 * time.Millisecond
	for attempt := 0; attempt < 14; attempt++ {
		nc, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err == nil {
			return nc
		}
		time.Sleep(backoff)
		if backoff < 200*time.Millisecond {
			backoff *= 2
		}
	}
	return nil
}

// helloOK runs a hello handshake on a raw connection and reports whether the reply said ok.
// It is a lighter, testing.T-free variant of the shared handshake helper so it can run inside
// thousands of goroutines without funneling every failure through t.Fatalf from a non-test
// goroutine.
func helloOK(nc net.Conn) bool {
	hello := bson.NewBuilder().
		AppendInt32("hello", 1).
		AppendString("$db", "admin").
		Build()
	if _, err := nc.Write(encodeRequestOpMsg(1, hello)); err != nil {
		return false
	}
	_ = nc.SetReadDeadline(time.Now().Add(30 * time.Second))
	var hb [headerLen]byte
	if _, err := io.ReadFull(nc, hb[:]); err != nil {
		return false
	}
	length := int32(binary.LittleEndian.Uint32(hb[0:4]))
	if length < headerLen {
		return false
	}
	payload := make([]byte, length-headerLen)
	if _, err := io.ReadFull(nc, payload); err != nil {
		return false
	}
	in, err := parseOpMsg(payload)
	if err != nil {
		return false
	}
	_ = nc.SetReadDeadline(time.Time{})
	return ok1(in.body)
}

// stableGoroutines returns a goroutine count after letting the scheduler settle, so a sample
// is not skewed by goroutines that are seconds from exiting.
func stableGoroutines() int {
	prev := -1
	for i := 0; i < 50; i++ {
		runtime.GC()
		n := runtime.NumGoroutine()
		if n == prev {
			return n
		}
		prev = n
		time.Sleep(20 * time.Millisecond)
	}
	return prev
}

// parsePositive parses a positive base-10 integer, rejecting zero, negatives, and garbage.
func parsePositive(s string) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, os.ErrInvalid
		}
		n = n*10 + int(r-'0')
	}
	if n <= 0 {
		return 0, os.ErrInvalid
	}
	return n, nil
}
