package doc

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// serveTestDB opens a database with a little data so /metrics has something to report.
func serveTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "serve.doc"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	c := db.Database("d").Collection("c")
	for i := 0; i < 10; i++ {
		if _, err := c.InsertOne(context.Background(), M{"_id": i, "n": i}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return db
}

func getBody(t *testing.T, srv *httptest.Server, path string) (int, string) {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestObservabilityEndpoints(t *testing.T) {
	db := serveTestDB(t)
	defer func() { _ = db.Close() }()
	srv := httptest.NewServer(db.ObservabilityHandler(false))
	defer srv.Close()

	if code, body := getBody(t, srv, "/healthz"); code != http.StatusOK || body != "ok" {
		t.Fatalf("/healthz = %d %q, want 200 ok", code, body)
	}
	if code, body := getBody(t, srv, "/readyz"); code != http.StatusOK || body != "ready" {
		t.Fatalf("/readyz = %d %q, want 200 ready", code, body)
	}
	code, body := getBody(t, srv, "/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics = %d, want 200", code)
	}
	if !strings.Contains(body, "doc_") {
		t.Fatalf("/metrics missing doc_ series:\n%s", body)
	}
}

func TestObservabilityMetricsContentType(t *testing.T) {
	db := serveTestDB(t)
	defer func() { _ = db.Close() }()
	srv := httptest.NewServer(db.ObservabilityHandler(false))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/plain") || !strings.Contains(ct, "0.0.4") {
		t.Fatalf("/metrics content-type = %q, want prometheus text 0.0.4", ct)
	}
}

func TestObservabilityProbesAfterClose(t *testing.T) {
	db := serveTestDB(t)
	srv := httptest.NewServer(db.ObservabilityHandler(false))
	defer srv.Close()
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if code, _ := getBody(t, srv, "/healthz"); code != http.StatusServiceUnavailable {
		t.Fatalf("/healthz after close = %d, want 503", code)
	}
	if code, _ := getBody(t, srv, "/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz after close = %d, want 503", code)
	}
}

func TestPprofMountedOnlyWhenEnabled(t *testing.T) {
	db := serveTestDB(t)
	defer func() { _ = db.Close() }()

	off := httptest.NewServer(db.ObservabilityHandler(false))
	defer off.Close()
	if code, _ := getBody(t, off, "/debug/pprof/"); code != http.StatusNotFound {
		t.Fatalf("/debug/pprof with pprof off = %d, want 404", code)
	}

	on := httptest.NewServer(db.ObservabilityHandler(true))
	defer on.Close()
	if code, _ := getBody(t, on, "/debug/pprof/"); code != http.StatusOK {
		t.Fatalf("/debug/pprof with pprof on = %d, want 200", code)
	}
}

// TestServeStartsAndStopsOnContext checks the blocking Serve loop binds, reports its
// address, answers a probe, and returns cleanly when the context is canceled.
func TestServeStartsAndStopsOnContext(t *testing.T) {
	db := serveTestDB(t)
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	addrCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- db.Serve(ctx, ServeOptions{
			Addr:  "localhost:0",
			Ready: func(addr string) { addrCh <- addr },
		})
	}()

	select {
	case addr := <-addrCh:
		code, body := 0, ""
		resp, err := http.Get("http://" + addr + "/healthz")
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		code = resp.StatusCode
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		body = string(b)
		if code != http.StatusOK || body != "ok" {
			t.Fatalf("probe = %d %q, want 200 ok", code, body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server never reported ready")
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Serve returned %v, want nil on context cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop after context cancel")
	}
}

func TestServeReturnsBindError(t *testing.T) {
	db := serveTestDB(t)
	defer func() { _ = db.Close() }()
	// An address already in use surfaces the bind error synchronously.
	ln := httptest.NewServer(db.ObservabilityHandler(false))
	defer ln.Close()
	taken := strings.TrimPrefix(ln.URL, "http://")
	if err := db.Serve(context.Background(), ServeOptions{Addr: taken}); err == nil {
		t.Fatal("Serve on a taken address should fail")
	}
}
