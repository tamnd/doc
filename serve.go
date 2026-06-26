package doc

import (
	"context"
	"net"
	"net/http"
	"net/http/pprof"
	"time"
)

// The observability server is the M7 server primer (spec 2061 doc 19 §22, M7): a small
// HTTP listener that exposes the operational endpoints a monitoring system scrapes,
// without the MongoDB wire protocol, which arrives in M8. It lets an operator point
// Prometheus and a load balancer's health checks at a live embedded database today.

// ServeOptions configures the observability HTTP server (spec 2061 doc 18 §2.4).
type ServeOptions struct {
	// Addr is the listen address, host:port. An empty host listens on all interfaces;
	// a zero port asks the OS for a free one, which Ready reports back.
	Addr string

	// Pprof, when true, mounts the net/http/pprof handlers under /debug/pprof so an
	// operator can capture CPU and heap profiles from the running process (spec §21.7).
	Pprof bool

	// Ready, if set, is called once with the bound address the moment the listener is
	// up, before the server starts accepting. A test uses it to learn the port chosen
	// for a zero-port Addr.
	Ready func(addr string)
}

// ObservabilityHandler returns the HTTP handler the observability server mounts (spec
// 2061 doc 18 §2.4): /metrics in Prometheus text form, /healthz for liveness, and
// /readyz for readiness. It is exported so an embedding application can mount the same
// endpoints on its own server instead of running doc's.
func (db *DB) ObservabilityHandler(pprofEnabled bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", db.handleMetrics)
	mux.HandleFunc("/healthz", db.handleHealthz)
	mux.HandleFunc("/readyz", db.handleReadyz)
	if pprofEnabled {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}
	return mux
}

// handleMetrics writes the Prometheus text exposition. A closed database has no live
// metrics to report, so the probe gets a 503.
func (db *DB) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if db.isClosed() {
		http.Error(w, "database is closed", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	if err := db.WritePrometheus(r.Context(), w); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// handleHealthz is the liveness probe: 200 with body "ok" while the database is open,
// 503 once it has closed.
func (db *DB) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	if db.isClosed() {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok"))
}

// handleReadyz is the readiness probe: 200 with body "ready" when the database can
// serve reads, 503 otherwise. An open database, read-only or not, can serve reads.
func (db *DB) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if db.isClosed() {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ready"))
}

// Serve runs the observability HTTP server until ctx is canceled, then shuts it down
// gracefully (spec 2061 doc 18 §2.4). It binds the listener synchronously so a bind
// error returns before the call blocks, and reports the bound address through
// opts.Ready. The wire-protocol server is M8; this serves only the scrape endpoints.
func (db *DB) Serve(ctx context.Context, opts ServeOptions) error {
	if opts.Addr == "" {
		opts.Addr = "localhost:9091"
	}
	ln, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return err
	}
	if opts.Ready != nil {
		opts.Ready(ln.Addr().String())
	}
	srv := &http.Server{
		Handler:           db.ObservabilityHandler(opts.Pprof),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}
