package wire

import (
	"context"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/tamnd/doc"
	"github.com/tamnd/doc/bson"
)

// startServer opens a fresh database and serves it with the given options on a loopback
// port, returning the server and bound address. Cleanup cancels the context and closes the
// database.
func startServer(t *testing.T, opts Options) (*Server, string) {
	t.Helper()
	db, err := doc.Open(filepath.Join(t.TempDir(), "wire.doc"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	srv := NewServer(db, opts)
	ctx, cancel := context.WithCancel(context.Background())
	addrCh := make(chan string, 1)
	go func() { _ = srv.ListenAndServe(ctx, "localhost:0", func(a string) { addrCh <- a }) }()
	var addr string
	select {
	case addr = <-addrCh:
	case <-time.After(5 * time.Second):
		t.Fatal("server never reported ready")
	}
	t.Cleanup(func() {
		cancel()
		_ = db.Close()
	})
	return srv, addr
}

func handshake(t *testing.T, nc net.Conn, reqID int32) {
	t.Helper()
	sendCommand(t, nc, reqID, cmdDoc(
		func(b *bson.Builder) { b.AppendInt32("hello", 1) },
		func(b *bson.Builder) { b.AppendString("$db", "admin") },
	))
}

func TestMaxConnsRejectsOverLimit(t *testing.T) {
	srv, addr := startServer(t, Options{MaxConns: 1})

	first, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial first: %v", err)
	}
	defer func() { _ = first.Close() }()
	handshake(t, first, 1)
	if srv.ConnCount() != 1 {
		t.Fatalf("ConnCount = %d, want 1", srv.ConnCount())
	}

	// The second connection is over the cap; the server closes it without a reply.
	second, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial second: %v", err)
	}
	defer func() { _ = second.Close() }()
	_ = second.SetReadDeadline(time.Now().Add(5 * time.Second))
	var one [1]byte
	if _, err := second.Read(one[:]); err != io.EOF {
		t.Fatalf("over-limit read = %v, want EOF", err)
	}
	if got := srv.Rejected(); got != 1 {
		t.Fatalf("Rejected = %d, want 1", got)
	}

	// Closing the first frees a slot, so a fresh connection is accepted again.
	_ = first.Close()
	waitFor(t, func() bool { return srv.ConnCount() == 0 }, 5*time.Second, "first conn to drain")
	third, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial third: %v", err)
	}
	defer func() { _ = third.Close() }()
	handshake(t, third, 1)
	if srv.ConnCount() != 1 {
		t.Fatalf("ConnCount after reuse = %d, want 1", srv.ConnCount())
	}
}

func TestIdleTimeoutClosesConnection(t *testing.T) {
	_, addr := startServer(t, Options{MaxConnIdle: 80 * time.Millisecond})
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = nc.Close() }()
	handshake(t, nc, 1)

	// Send nothing further. The server closes the idle connection, which the client sees as
	// EOF on the next read.
	_ = nc.SetReadDeadline(time.Now().Add(5 * time.Second))
	var one [1]byte
	if _, err := nc.Read(one[:]); err != io.EOF {
		t.Fatalf("idle read = %v, want EOF", err)
	}
}

func TestIdleTimeoutExemptInTransaction(t *testing.T) {
	_, addr := startServer(t, Options{MaxConnIdle: 80 * time.Millisecond})
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = nc.Close() }()
	handshake(t, nc, 1)
	lsid := makeLsid(8)

	// Open a transaction, then sit idle past the idle window. The connection must survive,
	// because a connection inside an open transaction is exempt from the idle timeout.
	sendCommand(t, nc, 2, docOf(
		func(b *bson.Builder) {
			txnInsert(b, "idle", lsid, 1, true, docOf(func(d *bson.Builder) { d.AppendInt32("x", 1) }))
		},
		func(b *bson.Builder) { b.AppendString("$db", "test") },
	))
	time.Sleep(200 * time.Millisecond)

	// The connection is still alive: committing succeeds rather than failing on a closed
	// socket.
	commit := commitTxn(t, nc, 3, lsid, 1)
	if !ok1(commit) {
		t.Fatalf("commit after idle-exempt wait = %v, want ok", commit)
	}
}

// waitFor polls cond until it is true or the timeout elapses.
func waitFor(t *testing.T, cond func() bool, timeout time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
