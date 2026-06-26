package wire

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tamnd/doc"
)

// Connection-management defaults match the serve command's documented defaults (spec 2061
// doc 16 §2.2, §15). A zero field in Options falls back to these.
const (
	DefaultMaxConns     = 200
	DefaultMaxConnIdle  = 10 * time.Minute
	DefaultDrainTimeout = 30 * time.Second
)

// Server speaks the MongoDB wire protocol over a net.Listener, dispatching commands to
// an open *doc.DB. One Server owns one database handle; a single writer sits behind any
// number of connections (spec 2061 doc 16 §1.4).
type Server struct {
	db   *doc.DB
	opts Options

	processID doc.ObjectID // stable for the life of the process, used in topologyVersion
	connSeq   atomic.Int64 // assigns connectionId per accepted connection
	reqSeq    atomic.Int64 // assigns requestID on server-originated messages

	cursors *cursorStore

	connCount atomic.Int64 // live connection count, checked against MaxConns on accept
	rejected  atomic.Int64 // connections refused because the limit was reached

	mu    sync.Mutex
	conns map[*conn]struct{} // live connections, for graceful shutdown
}

// Options configures a Server. The zero value is usable; NewServer fills the defaults.
type Options struct {
	// MaxMessageBytes caps an accepted message; zero means DefaultMaxMessageBytes.
	MaxMessageBytes int32
	// ReadOnly advertises a read-only server in the hello response.
	ReadOnly bool
	// AuthRequired turns on authentication: every connection must authenticate before
	// running any command beyond the handshake, and commands are checked against the
	// authenticated user's roles (spec 2061 doc 16 §19). The loopback exception lets an
	// operator create the first user while none exist.
	AuthRequired bool
	// TLS configures transport encryption for the listener. The zero value leaves TLS off,
	// which is acceptable only for a loopback-bound listener (spec 2061 doc 16 §9, doc 17
	// §12). A configured client CA also enables the MONGODB-X509 mechanism.
	TLS TLSOptions
	// MaxConns caps simultaneous connections; a new TCP connection over the cap is accepted
	// and immediately closed so the client sees a reset (spec 2061 doc 16 §15.6). Zero means
	// DefaultMaxConns.
	MaxConns int
	// MaxConnIdle closes a connection that has received no message for this long. A
	// connection inside an open transaction is exempt, since the transaction lifetime limit
	// governs it instead (spec 2061 doc 16 §15.4). Zero means DefaultMaxConnIdle; a negative
	// value disables the idle timeout.
	MaxConnIdle time.Duration
	// DrainTimeout bounds how long graceful shutdown waits for in-flight commands before it
	// forces remaining connections closed (spec 2061 doc 16 §15.5). Zero means
	// DefaultDrainTimeout.
	DrainTimeout time.Duration
	// SlowOpThreshold logs any command that takes at least this long (spec 2061 doc 16
	// §2.2, --log-slow-ops). Zero disables slow-command logging.
	SlowOpThreshold time.Duration
	// Logger receives connection lifecycle events; nil means slog.Default.
	Logger *slog.Logger
}

// NewServer builds a Server over an already-open database. The database outlives the
// server: closing the server does not close db.
func NewServer(db *doc.DB, opts Options) *Server {
	if opts.MaxMessageBytes <= 0 {
		opts.MaxMessageBytes = DefaultMaxMessageBytes
	}
	if opts.MaxConns <= 0 {
		opts.MaxConns = DefaultMaxConns
	}
	if opts.MaxConnIdle == 0 {
		opts.MaxConnIdle = DefaultMaxConnIdle
	}
	if opts.DrainTimeout <= 0 {
		opts.DrainTimeout = DefaultDrainTimeout
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Server{
		db:        db,
		opts:      opts,
		processID: doc.NewObjectID(),
		cursors:   newCursorStore(),
		conns:     make(map[*conn]struct{}),
	}
}

// Serve accepts connections on ln until ctx is canceled or Accept fails permanently.
// On cancel it stops accepting, closes every live connection, and waits for the
// per-connection goroutines to drain before returning.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	// Apply the TLS mode to the listener before accepting. A bad certificate or CA file is
	// surfaced here rather than per connection.
	cfg, err := buildTLSConfig(s.opts.TLS)
	if err != nil {
		return err
	}
	ln = wrapListener(ln, cfg, s.opts.TLS.Mode)

	// A watcher goroutine drains the server when the context is canceled: it stops
	// accepting, gives in-flight commands a window to finish, then forces the rest closed
	// (spec 2061 doc 16 §15.5).
	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
		case <-done:
		}
		_ = ln.Close()
		s.drain()
	}()

	for {
		nc, err := ln.Accept()
		if err != nil {
			close(done)
			wg.Wait()
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		// Enforce the connection cap before spending a goroutine on the connection. A
		// connection over the limit is closed at once, which the client sees as a reset
		// (spec 2061 doc 16 §15.6).
		if s.connCount.Add(1) > int64(s.opts.MaxConns) {
			s.connCount.Add(-1)
			s.rejected.Add(1)
			_ = nc.Close()
			s.opts.Logger.Debug("wire connection rejected: limit reached", "maxConns", s.opts.MaxConns)
			continue
		}
		c := s.newConn(nc)
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.serve(ctx)
			s.removeConn(c)
		}()
	}
}

// drain runs graceful shutdown after the listener is closed. It repeatedly interrupts idle
// reads so a connection between commands observes the shutdown promptly, lets in-flight
// commands finish and flush their replies, and after DrainTimeout forces any straggler
// closed (spec 2061 doc 16 §15.5).
func (s *Server) drain() {
	deadline := time.Now().Add(s.opts.DrainTimeout)
	for s.connCount.Load() > 0 && time.Now().Before(deadline) {
		s.interruptReads()
		time.Sleep(5 * time.Millisecond)
	}
	s.closeAllConns()
}

// interruptReads pushes every live connection's read deadline into the past, unblocking a
// connection parked in a read so it can notice the shutdown. A command already in flight is
// untouched, since it is not reading, and its reply still flushes.
func (s *Server) interruptReads() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.conns {
		_ = c.nc.SetReadDeadline(time.Now().Add(-time.Second))
	}
}

// Rejected reports the number of connections refused because MaxConns was reached.
func (s *Server) Rejected() int64 { return s.rejected.Load() }

// ListenAndServe binds addr and serves until ctx is canceled. The ready callback, if
// set, receives the bound address (useful when addr uses port 0).
func (s *Server) ListenAndServe(ctx context.Context, addr string, ready func(string)) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	if ready != nil {
		ready(ln.Addr().String())
	}
	return s.Serve(ctx, ln)
}

// nextRequestID hands out the requestID the server stamps on a message it originates.
func (s *Server) nextRequestID() int32 { return int32(s.reqSeq.Add(1)) }

func (s *Server) newConn(nc net.Conn) *conn {
	c := &conn{
		srv: s,
		nc:  nc,
		id:  int32(s.connSeq.Add(1)),
	}
	s.mu.Lock()
	s.conns[c] = struct{}{}
	s.mu.Unlock()
	return c
}

func (s *Server) removeConn(c *conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	s.cursors.closeForConn(c.id)
	s.connCount.Add(-1)
}

func (s *Server) closeAllConns() {
	s.mu.Lock()
	for c := range s.conns {
		_ = c.nc.Close()
	}
	s.mu.Unlock()
}

// ConnCount reports the number of live connections, for tests and diagnostics.
func (s *Server) ConnCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}
