package wire

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"

	"github.com/tamnd/doc"
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

	mu    sync.Mutex
	conns map[*conn]struct{} // live connections, for graceful shutdown
}

// Options configures a Server. The zero value is usable; NewServer fills the defaults.
type Options struct {
	// MaxMessageBytes caps an accepted message; zero means DefaultMaxMessageBytes.
	MaxMessageBytes int32
	// ReadOnly advertises a read-only server in the hello response.
	ReadOnly bool
	// Logger receives connection lifecycle events; nil means slog.Default.
	Logger *slog.Logger
}

// NewServer builds a Server over an already-open database. The database outlives the
// server: closing the server does not close db.
func NewServer(db *doc.DB, opts Options) *Server {
	if opts.MaxMessageBytes <= 0 {
		opts.MaxMessageBytes = DefaultMaxMessageBytes
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
	// A watcher goroutine closes the listener when the context is canceled, which
	// unblocks Accept below.
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
		s.closeAllConns()
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
		c := s.newConn(nc)
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.serve(ctx)
			s.removeConn(c)
		}()
	}
}

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
