package main

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/tamnd/doc"
	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/wire"
)

func TestServeOptionsDefaults(t *testing.T) {
	opts, err := buildServeOptions(parseFlags(nil), false)
	if err != nil {
		t.Fatalf("buildServeOptions: %v", err)
	}
	if opts.MaxConns != wire.DefaultMaxConns {
		t.Fatalf("MaxConns = %d, want %d", opts.MaxConns, wire.DefaultMaxConns)
	}
	if opts.MaxConnIdle != wire.DefaultMaxConnIdle {
		t.Fatalf("MaxConnIdle = %v, want %v", opts.MaxConnIdle, wire.DefaultMaxConnIdle)
	}
	if opts.SlowOpThreshold != 100*time.Millisecond {
		t.Fatalf("SlowOpThreshold = %v, want 100ms", opts.SlowOpThreshold)
	}
	if opts.AuthRequired || opts.ReadOnly || opts.TLS.Mode != "" {
		t.Fatalf("defaults should be open: %+v", opts)
	}
}

func TestServeOptionsFromFlags(t *testing.T) {
	fs := parseFlags([]string{
		"--auth", "--readonly",
		"--max-conns", "50",
		"--max-conn-idle", "30s",
		"--log-slow-ops", "0s",
		"--tls", "--tls-cert", "/tmp/cert.pem", "--tls-key", "/tmp/key.pem", "--tls-ca", "/tmp/ca.pem",
		"--log-format", "json",
	})
	opts, err := buildServeOptions(fs, false)
	if err != nil {
		t.Fatalf("buildServeOptions: %v", err)
	}
	if !opts.AuthRequired {
		t.Fatal("AuthRequired = false, want true")
	}
	if !opts.ReadOnly {
		t.Fatal("ReadOnly = false, want true")
	}
	if opts.MaxConns != 50 {
		t.Fatalf("MaxConns = %d, want 50", opts.MaxConns)
	}
	if opts.MaxConnIdle != 30*time.Second {
		t.Fatalf("MaxConnIdle = %v, want 30s", opts.MaxConnIdle)
	}
	if opts.SlowOpThreshold != 0 {
		t.Fatalf("SlowOpThreshold = %v, want 0 (disabled)", opts.SlowOpThreshold)
	}
	if opts.TLS.Mode != wire.TLSRequire {
		t.Fatalf("TLS.Mode = %q, want requireTLS", opts.TLS.Mode)
	}
	if opts.TLS.CertFile != "/tmp/cert.pem" || opts.TLS.KeyFile != "/tmp/key.pem" || opts.TLS.CAFile != "/tmp/ca.pem" {
		t.Fatalf("TLS file paths not carried: %+v", opts.TLS)
	}
}

func TestServeGlobalReadonlyApplies(t *testing.T) {
	opts, err := buildServeOptions(parseFlags(nil), true)
	if err != nil {
		t.Fatalf("buildServeOptions: %v", err)
	}
	if !opts.ReadOnly {
		t.Fatal("a global --readonly should carry into the wire server")
	}
}

func TestServeTLSRequiresCertAndKey(t *testing.T) {
	_, err := buildServeOptions(parseFlags([]string{"--tls"}), false)
	if err == nil {
		t.Fatal("--tls without cert/key should error")
	}
}

func TestServeBadFlagValues(t *testing.T) {
	if _, err := buildServeOptions(parseFlags([]string{"--max-conns", "lots"}), false); err == nil {
		t.Fatal("bad --max-conns should error")
	}
	if _, err := buildServeOptions(parseFlags([]string{"--max-conn-idle", "soon"}), false); err == nil {
		t.Fatal("bad --max-conn-idle should error")
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for _, h := range []string{"127.0.0.1", "::1", "localhost", "127.0.0.5"} {
		if !isLoopbackHost(h) {
			t.Errorf("isLoopbackHost(%q) = false, want true", h)
		}
	}
	for _, h := range []string{"0.0.0.0", "1.2.3.4", "192.168.1.10", "::"} {
		if isLoopbackHost(h) {
			t.Errorf("isLoopbackHost(%q) = true, want false", h)
		}
	}
}

func TestServeSyncDefaultsToFull(t *testing.T) {
	cfg, _, err := parseArgs([]string{"db.doc", "serve"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if cfg.sync != doc.SyncFull {
		t.Fatalf("serve sync = %v, want SyncFull", cfg.sync)
	}
}

func TestServeSyncRespectsExplicitFlag(t *testing.T) {
	cfg, _, err := parseArgs([]string{"--sync", "normal", "db.doc", "serve"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if cfg.sync != doc.SyncNormal {
		t.Fatalf("serve sync = %v, want SyncNormal (explicit flag wins)", cfg.sync)
	}
}

func TestServeNonLoopbackWithoutTLSRefused(t *testing.T) {
	a := &app{cfg: &config{}}
	fs := parseFlags([]string{"--bind", "0.0.0.0", "--port", "0"})
	code := a.runServe(context.Background(), fs, nil)
	if code != exitUsage {
		t.Fatalf("non-loopback without TLS exit = %d, want %d", code, exitUsage)
	}
}

// TestServeEndToEnd boots the serve command on a loopback ephemeral port, completes a
// hello handshake over a real TCP connection, and shuts the server down by canceling the
// context.
func TestServeEndToEnd(t *testing.T) {
	db, err := doc.Open(tmpDoc(t), doc.WithSyncLevel(doc.SyncFull))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	a := &app{cfg: &config{}, db: db}

	ctx, cancel := context.WithCancel(context.Background())
	addrCh := make(chan string, 1)
	done := make(chan int, 1)
	fs := parseFlags([]string{"--bind", "127.0.0.1", "--port", "0"})
	go func() { done <- a.runServe(ctx, fs, func(bound string) { addrCh <- bound }) }()

	var addr string
	select {
	case addr = <-addrCh:
	case <-time.After(5 * time.Second):
		t.Fatal("server never reported ready")
	}

	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = nc.Close() }()

	hello := bson.NewBuilder().
		AppendInt32("hello", 1).
		AppendString("$db", "admin").
		Build()
	if _, err := nc.Write(encodeHello(1, hello)); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	reply := readHelloReply(t, nc)
	if v, ok := reply.Lookup("ok"); !ok || v.Double() != 1 {
		t.Fatalf("hello ok = %v, want 1", v)
	}
	if v, ok := reply.Lookup("isWritablePrimary"); !ok || !v.Boolean() {
		t.Fatalf("isWritablePrimary = %v, want true", v)
	}

	cancel()
	select {
	case code := <-done:
		if code != exitOK {
			t.Fatalf("serve exit = %d, want 0 on cancel", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after cancel")
	}
}

// encodeHello frames a hello command as an OP_MSG with a single kind-0 body section.
func encodeHello(requestID int32, body bson.Raw) []byte {
	const opMsg = 2013
	total := 16 + 4 + 1 + len(body)
	buf := make([]byte, 16+4+1, total)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(total))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(requestID))
	binary.LittleEndian.PutUint32(buf[8:12], 0)
	binary.LittleEndian.PutUint32(buf[12:16], uint32(opMsg))
	binary.LittleEndian.PutUint32(buf[16:20], 0) // flags
	buf[20] = 0                                  // section kind 0
	return append(buf, body...)
}

// readHelloReply reads one OP_MSG reply and returns the body document.
func readHelloReply(t *testing.T, nc net.Conn) bson.Raw {
	t.Helper()
	_ = nc.SetReadDeadline(time.Now().Add(5 * time.Second))
	var hb [16]byte
	if _, err := io.ReadFull(nc, hb[:]); err != nil {
		t.Fatalf("read header: %v", err)
	}
	length := int32(binary.LittleEndian.Uint32(hb[0:4]))
	payload := make([]byte, length-16)
	if _, err := io.ReadFull(nc, payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	// payload: flags(4) + section kind(1) + body doc.
	return bson.Raw(payload[5:])
}
