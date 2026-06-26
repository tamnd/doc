package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/tamnd/doc"
)

// subServe runs the observability HTTP server over the open database (spec 2061 doc 18
// §2.4, the M7 server primer). It exposes /metrics, /healthz, and /readyz, optionally
// the pprof endpoints, and blocks until the process is interrupted. The MongoDB wire
// protocol arrives in M8; this serves only the scrape and health endpoints.
func (a *app) subServe() int {
	fs := parseFlags(a.cfg.subArgs)
	addr := fs.values["metrics-addr"]
	if addr == "" {
		addr = fs.values["addr"]
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := doc.ServeOptions{
		Addr:  addr,
		Pprof: fs.bools["pprof"],
		Ready: func(bound string) {
			_, _ = fmt.Fprintf(os.Stderr, "doc serve listening on http://%s (metrics, healthz, readyz)\n", bound)
			if fs.bools["pprof"] {
				_, _ = fmt.Fprintf(os.Stderr, "doc serve pprof at http://%s/debug/pprof/\n", bound)
			}
			_, _ = fmt.Fprintln(os.Stderr, "press Ctrl-C to stop")
		},
	}
	if err := a.db.Serve(ctx, opts); err != nil {
		return reportTop(cliError{code: exitIOError, msg: err.Error()})
	}
	return exitOK
}
