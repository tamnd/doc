package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/tamnd/doc"
	"github.com/tamnd/doc/wire"
)

// subServe runs the MongoDB wire-protocol server over the open database (spec 2061 doc 16
// §2). It binds a TCP listener, accepts driver connections, and serves the full command
// surface, sessions, and transactions until the process is interrupted. The observability
// HTTP surface from the M7 primer is opt-in behind --http.
func (a *app) subServe() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ready := func(bound string) {
		_, _ = fmt.Fprintf(os.Stderr, "doc serve listening on mongodb://%s\n", bound)
		_, _ = fmt.Fprintln(os.Stderr, "press Ctrl-C to stop")
	}
	return a.runServe(ctx, parseFlags(a.cfg.subArgs), ready)
}

// runServe is the body of the serve command, separated from signal handling so a test can
// drive it with its own context and ready hook. It binds the wire listener, optionally the
// HTTP surface, and serves until ctx is canceled.
func (a *app) runServe(ctx context.Context, fs flagSet, ready func(string)) int {
	bind := valueOr(fs, "bind", "127.0.0.1")
	port := valueOr(fs, "port", "27017")
	addr := net.JoinHostPort(bind, port)

	useTLS := fs.bools["tls"]
	if !useTLS && !isLoopbackHost(bind) {
		return reportTop(cliError{code: exitUsage, msg: fmt.Sprintf(
			"refusing to serve %s without TLS; pass --tls (with --tls-cert and --tls-key) or bind a loopback address", addr)})
	}

	opts, err := buildServeOptions(fs, a.cfg.readonly)
	if err != nil {
		return reportTop(cliError{code: exitUsage, msg: err.Error()})
	}

	srv := wire.NewServer(a.db, opts)

	// The HTTP observability surface (metrics, healthz, readyz) runs alongside the wire
	// server when requested, sharing the same shutdown context.
	var httpErr chan error
	if fs.bools["http"] {
		httpBind := valueOr(fs, "http-bind", bind)
		httpPort := valueOr(fs, "http-port", "27018")
		httpAddr := net.JoinHostPort(httpBind, httpPort)
		httpErr = make(chan error, 1)
		go func() {
			httpErr <- a.db.Serve(ctx, doc.ServeOptions{
				Addr:  httpAddr,
				Pprof: fs.bools["pprof"],
				Ready: func(bound string) {
					_, _ = fmt.Fprintf(os.Stderr, "doc serve HTTP surface on http://%s (metrics, healthz, readyz)\n", bound)
				},
			})
		}()
	}

	// announce calls the supplied ready hook, then notes the server's security posture.
	announce := func(bound string) {
		if ready != nil {
			ready(bound)
		}
		if opts.AuthRequired {
			_, _ = fmt.Fprintln(os.Stderr, "authentication required")
		}
		if opts.TLS.Mode == wire.TLSRequire {
			_, _ = fmt.Fprintln(os.Stderr, "TLS required")
		}
		if opts.ReadOnly {
			_, _ = fmt.Fprintln(os.Stderr, "read-only: write commands are refused")
		}
	}

	if err := srv.ListenAndServe(ctx, addr, announce); err != nil {
		return reportTop(cliError{code: exitIOError, msg: err.Error()})
	}
	if httpErr != nil {
		if err := <-httpErr; err != nil {
			return reportTop(cliError{code: exitIOError, msg: err.Error()})
		}
	}
	return exitOK
}

// buildServeOptions turns the serve flags into wire.Options, validating the TLS and
// connection-management settings (spec 2061 doc 16 §2.2).
func buildServeOptions(fs flagSet, globalReadonly bool) (wire.Options, error) {
	opts := wire.Options{
		ReadOnly:     globalReadonly || fs.bools["readonly"],
		AuthRequired: fs.bools["auth"],
		Logger:       buildServeLogger(fs.values["log-format"]),
	}

	maxConns, err := parseIntFlag(fs, "max-conns", wire.DefaultMaxConns)
	if err != nil {
		return wire.Options{}, err
	}
	opts.MaxConns = maxConns

	idle, err := parseDurationFlag(fs, "max-conn-idle", wire.DefaultMaxConnIdle)
	if err != nil {
		return wire.Options{}, err
	}
	opts.MaxConnIdle = idle

	slow, err := parseDurationFlag(fs, "log-slow-ops", 100*time.Millisecond)
	if err != nil {
		return wire.Options{}, err
	}
	opts.SlowOpThreshold = slow

	if fs.bools["tls"] {
		tlsOpts, err := buildServeTLS(fs)
		if err != nil {
			return wire.Options{}, err
		}
		opts.TLS = tlsOpts
	}
	return opts, nil
}

// buildServeTLS assembles the TLS options, requiring a certificate and key. A CA file
// turns on client-certificate verification, which the MONGODB-X509 mechanism needs.
func buildServeTLS(fs flagSet) (wire.TLSOptions, error) {
	cert := fs.values["tls-cert"]
	key := fs.values["tls-key"]
	if cert == "" || key == "" {
		return wire.TLSOptions{}, fmt.Errorf("--tls requires --tls-cert and --tls-key")
	}
	return wire.TLSOptions{
		Mode:     wire.TLSRequire,
		CertFile: cert,
		KeyFile:  key,
		CAFile:   fs.values["tls-ca"],
	}, nil
}

// buildServeLogger builds the structured logger for the server from --log-format, which is
// text (default) or json.
func buildServeLogger(format string) *slog.Logger {
	if format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

// valueOr reads a string flag, returning def when it was not given.
func valueOr(fs flagSet, name, def string) string {
	if v, ok := fs.values[name]; ok && v != "" {
		return v
	}
	return def
}

// parseIntFlag reads an integer flag, returning def when absent.
func parseIntFlag(fs flagSet, name string, def int) (int, error) {
	v, ok := fs.values[name]
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("bad --%s value: %s", name, v)
	}
	return n, nil
}

// parseDurationFlag reads a duration flag, returning def when absent.
func parseDurationFlag(fs flagSet, name string, def time.Duration) (time.Duration, error) {
	v, ok := fs.values[name]
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("bad --%s value: %s", name, v)
	}
	return d, nil
}

// isLoopbackHost reports whether a bind host names the loopback interface, the one case
// where serving without TLS is allowed (spec 2061 doc 17 §12).
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
