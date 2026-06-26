package wire

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/tamnd/doc"
	"github.com/tamnd/doc/bson"
)

// testServer starts a wire server over a fresh database on a loopback port and returns
// a dialed connection plus a cleanup. The context is canceled by cleanup so the accept
// loop and connection goroutines drain.
func testServer(t *testing.T) (*Server, net.Conn) {
	t.Helper()
	db, err := doc.Open(filepath.Join(t.TempDir(), "wire.doc"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	srv := NewServer(db, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	addrCh := make(chan string, 1)
	go func() { _ = srv.ListenAndServe(ctx, "localhost:0", func(a string) { addrCh <- a }) }()

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
	t.Cleanup(func() {
		_ = nc.Close()
		cancel()
		_ = db.Close()
	})
	return srv, nc
}

// sendCommand frames body as an OP_MSG, writes it, and reads back the reply body.
func sendCommand(t *testing.T, nc net.Conn, requestID int32, body bson.Raw) bson.Raw {
	t.Helper()
	req := encodeRequestOpMsg(requestID, body)
	if _, err := nc.Write(req); err != nil {
		t.Fatalf("write: %v", err)
	}
	return readReplyOpMsg(t, nc, requestID)
}

// encodeRequestOpMsg frames a client OP_MSG with a single kind-0 body, no checksum.
func encodeRequestOpMsg(requestID int32, body bson.Raw) []byte {
	total := headerLen + 4 + 1 + len(body)
	buf := make([]byte, headerLen+4+1, total)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(total))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(requestID))
	binary.LittleEndian.PutUint32(buf[8:12], 0)
	binary.LittleEndian.PutUint32(buf[12:16], uint32(opMsg))
	binary.LittleEndian.PutUint32(buf[16:20], 0)
	buf[20] = 0
	return append(buf, body...)
}

// readReplyOpMsg reads one OP_MSG reply and returns its body, checking the responseTo
// echoes the request id.
func readReplyOpMsg(t *testing.T, nc net.Conn, requestID int32) bson.Raw {
	t.Helper()
	_ = nc.SetReadDeadline(time.Now().Add(5 * time.Second))
	var hb [headerLen]byte
	if _, err := io.ReadFull(nc, hb[:]); err != nil {
		t.Fatalf("read header: %v", err)
	}
	length := int32(binary.LittleEndian.Uint32(hb[0:4]))
	responseTo := int32(binary.LittleEndian.Uint32(hb[8:12]))
	opcode := int32(binary.LittleEndian.Uint32(hb[12:16]))
	if responseTo != requestID {
		t.Fatalf("responseTo = %d, want %d", responseTo, requestID)
	}
	if opcode != opMsg {
		t.Fatalf("reply opcode = %d, want OP_MSG", opcode)
	}
	payload := make([]byte, length-headerLen)
	if _, err := io.ReadFull(nc, payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	in, err := parseOpMsg(payload)
	if err != nil {
		t.Fatalf("parse reply: %v", err)
	}
	return in.body
}

func cmdDoc(elems ...func(*bson.Builder)) bson.Raw {
	b := bson.NewBuilder()
	for _, e := range elems {
		e(b)
	}
	return b.Build()
}

func TestHelloOverOpMsg(t *testing.T) {
	_, nc := testServer(t)
	body := cmdDoc(
		func(b *bson.Builder) { b.AppendInt32("hello", 1) },
		func(b *bson.Builder) { b.AppendString("$db", "admin") },
	)
	reply := sendCommand(t, nc, 1, body)

	if v, ok := reply.Lookup("ok"); !ok || v.Double() != 1 {
		t.Fatalf("hello ok = %v, want 1", v)
	}
	for _, field := range []string{"isWritablePrimary", "helloOk", "maxBsonObjectSize", "maxWireVersion", "topologyVersion", "connectionId"} {
		if _, ok := reply.Lookup(field); !ok {
			t.Fatalf("hello reply missing %q:\n%v", field, reply)
		}
	}
	if v, ok := reply.Lookup("maxWireVersion"); !ok || v.Int32() != maxWireVersion {
		t.Fatalf("maxWireVersion = %v, want %d", v, maxWireVersion)
	}
	if v, ok := reply.Lookup("isWritablePrimary"); !ok || !v.Boolean() {
		t.Fatalf("isWritablePrimary = %v, want true", v)
	}
}

func TestPingAndBuildInfo(t *testing.T) {
	_, nc := testServer(t)

	ping := sendCommand(t, nc, 2, cmdDoc(
		func(b *bson.Builder) { b.AppendInt32("ping", 1) },
		func(b *bson.Builder) { b.AppendString("$db", "admin") },
	))
	if v, ok := ping.Lookup("ok"); !ok || v.Double() != 1 {
		t.Fatalf("ping ok = %v", v)
	}

	bi := sendCommand(t, nc, 3, cmdDoc(
		func(b *bson.Builder) { b.AppendInt32("buildInfo", 1) },
		func(b *bson.Builder) { b.AppendString("$db", "admin") },
	))
	if v, ok := bi.Lookup("version"); !ok || v.StringValue() == "" {
		t.Fatalf("buildInfo version = %v", v)
	}
}

func TestUnknownCommandReply(t *testing.T) {
	_, nc := testServer(t)
	reply := sendCommand(t, nc, 4, cmdDoc(
		func(b *bson.Builder) { b.AppendInt32("nosuchcommand", 1) },
		func(b *bson.Builder) { b.AppendString("$db", "admin") },
	))
	if v, ok := reply.Lookup("ok"); !ok || v.Double() != 0 {
		t.Fatalf("unknown command ok = %v, want 0", v)
	}
	if v, ok := reply.Lookup("code"); !ok || v.Int32() != 59 {
		t.Fatalf("unknown command code = %v, want 59 CommandNotFound", v)
	}
}

// TestLegacyHelloOverOpQuery checks the OP_QUERY handshake path, which old drivers use
// for the first isMaster.
func TestLegacyHelloOverOpQuery(t *testing.T) {
	_, nc := testServer(t)

	query := cmdDoc(
		func(b *bson.Builder) { b.AppendInt32("isMaster", 1) },
	)
	req := encodeRequestOpQuery(10, "admin.$cmd", query)
	if _, err := nc.Write(req); err != nil {
		t.Fatalf("write: %v", err)
	}

	_ = nc.SetReadDeadline(time.Now().Add(5 * time.Second))
	var hb [headerLen]byte
	if _, err := io.ReadFull(nc, hb[:]); err != nil {
		t.Fatalf("read header: %v", err)
	}
	length := int32(binary.LittleEndian.Uint32(hb[0:4]))
	opcode := int32(binary.LittleEndian.Uint32(hb[12:16]))
	if opcode != opReply {
		t.Fatalf("legacy handshake opcode = %d, want OP_REPLY", opcode)
	}
	payload := make([]byte, length-headerLen)
	if _, err := io.ReadFull(nc, payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	// Skip responseFlags(4) cursorID(8) startingFrom(4) numberReturned(4) = 20 bytes.
	doc := bson.Raw(payload[20:])
	if v, ok := doc.Lookup("isWritablePrimary"); !ok || !v.Boolean() {
		t.Fatalf("legacy hello isWritablePrimary = %v, want true", v)
	}
}

// encodeRequestOpQuery frames an OP_QUERY for the handshake test.
func encodeRequestOpQuery(requestID int32, coll string, query bson.Raw) []byte {
	body := make([]byte, 0, 64)
	var f [4]byte
	body = append(body, f[:]...) // flags 0
	body = append(body, []byte(coll)...)
	body = append(body, 0)       // cstring null
	body = append(body, f[:]...) // numberToSkip 0
	binary.LittleEndian.PutUint32(f[:], 1)
	body = append(body, f[:]...) // numberToReturn 1
	body = append(body, query...)

	total := headerLen + len(body)
	buf := make([]byte, headerLen, total)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(total))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(requestID))
	binary.LittleEndian.PutUint32(buf[8:12], 0)
	binary.LittleEndian.PutUint32(buf[12:16], uint32(opQuery))
	return append(buf, body...)
}

func TestGracefulShutdownClosesConns(t *testing.T) {
	db, err := doc.Open(filepath.Join(t.TempDir(), "wire.doc"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	srv := NewServer(db, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	addrCh := make(chan string, 1)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe(ctx, "localhost:0", func(a string) { addrCh <- a }) }()
	addr := <-addrCh

	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = nc.Close() }()
	// Handshake so the connection is registered.
	sendCommand(t, nc, 1, cmdDoc(
		func(b *bson.Builder) { b.AppendInt32("hello", 1) },
		func(b *bson.Builder) { b.AppendString("$db", "admin") },
	))
	if srv.ConnCount() != 1 {
		t.Fatalf("ConnCount = %d, want 1", srv.ConnCount())
	}

	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve returned %v, want nil on cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
	if srv.ConnCount() != 0 {
		t.Fatalf("ConnCount after shutdown = %d, want 0", srv.ConnCount())
	}
}

func TestRejectsOversizeMessage(t *testing.T) {
	db, err := doc.Open(filepath.Join(t.TempDir(), "wire.doc"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	srv := NewServer(db, Options{MaxMessageBytes: 256})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrCh := make(chan string, 1)
	go func() { _ = srv.ListenAndServe(ctx, "localhost:0", func(a string) { addrCh <- a }) }()
	addr := <-addrCh

	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = nc.Close() }()

	// Claim a 4 KiB message, over the 256-byte cap. The server should drop the
	// connection without reading the (never sent) body.
	var hb [headerLen]byte
	binary.LittleEndian.PutUint32(hb[0:4], 4096)
	binary.LittleEndian.PutUint32(hb[12:16], uint32(opMsg))
	if _, err := nc.Write(hb[:]); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = nc.SetReadDeadline(time.Now().Add(5 * time.Second))
	var one [1]byte
	if _, err := nc.Read(one[:]); err != io.EOF {
		t.Fatalf("expected EOF after oversize header, got %v", err)
	}
}
