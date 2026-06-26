package wire

import (
	"bytes"
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

// makeLsid builds an lsid sub-document carrying a 16-byte UUID, the shape a driver sends
// to group commands into a logical session.
func makeLsid(seed byte) bson.Raw {
	uuid := bytes.Repeat([]byte{seed}, 16)
	return bson.NewBuilder().AppendBinary("id", binarySubtypeUUID, uuid).Build()
}

// withTxn appends the session and transaction fields a command carries inside an explicit
// transaction: lsid, txnNumber, startTransaction on the opening command, and autocommit.
func withTxn(lsid bson.Raw, txnNum int64, start bool) func(*bson.Builder) {
	return func(b *bson.Builder) {
		b.AppendDocument("lsid", lsid)
		b.AppendInt64("txnNumber", txnNum)
		if start {
			b.AppendBoolean("startTransaction", true)
		}
		b.AppendBoolean("autocommit", false)
	}
}

// txnInsert opens or continues a transaction with one inserted document.
func txnInsert(b *bson.Builder, coll string, lsid bson.Raw, txnNum int64, start bool, doc bson.Raw) {
	b.AppendString("insert", coll)
	b.AppendArray("documents", docArray([]bson.Raw{doc}))
	withTxn(lsid, txnNum, start)(b)
}

// findCount runs a plain auto-commit find and returns the firstBatch length the snapshot
// sees.
func findCount(t *testing.T, nc net.Conn, reqID int32, db, coll string) int {
	t.Helper()
	reply := sendCommand(t, nc, reqID, docOf(
		func(b *bson.Builder) { b.AppendString("find", coll) },
		func(b *bson.Builder) { b.AppendString("$db", db) },
	))
	return arrayLen(t, mustDoc(t, reply, "cursor"), "firstBatch")
}

// commitTxn sends a commitTransaction for the (lsid, txnNumber) pair and returns the reply.
func commitTxn(t *testing.T, nc net.Conn, reqID int32, lsid bson.Raw, txnNum int64) bson.Raw {
	t.Helper()
	return sendCommand(t, nc, reqID, docOf(
		func(b *bson.Builder) { b.AppendInt32("commitTransaction", 1) },
		func(b *bson.Builder) { b.AppendDocument("lsid", lsid) },
		func(b *bson.Builder) { b.AppendInt64("txnNumber", txnNum) },
		func(b *bson.Builder) { b.AppendBoolean("autocommit", false) },
		func(b *bson.Builder) { b.AppendString("$db", "admin") },
	))
}

// hasLabel reports whether a reply carries label in its errorLabels array.
func hasLabel(reply bson.Raw, label string) bool {
	v, ok := reply.Lookup("errorLabels")
	if !ok || v.Type != bson.TypeArray {
		return false
	}
	for _, e := range arrayElements(v) {
		if e.Type == bson.TypeString && e.StringValue() == label {
			return true
		}
	}
	return false
}

func TestTransactionCommitMakesWritesVisible(t *testing.T) {
	_, nc := testServer(t)
	lsid := makeLsid(1)

	// Open a transaction and insert one document into it.
	ins := sendCommand(t, nc, 1, docOf(
		func(b *bson.Builder) {
			txnInsert(b, "txc", lsid, 1, true, docOf(func(d *bson.Builder) { d.AppendInt32("x", 1) }))
		},
		func(b *bson.Builder) { b.AppendString("$db", "test") },
	))
	if v, ok := ins.Lookup("n"); !ok || v.Int32() != 1 {
		t.Fatalf("txn insert n = %v, want 1", v)
	}

	// An auto-commit read on the same connection must not see the uncommitted write.
	if before := findCount(t, nc, 2, "test", "txc"); before != 0 {
		t.Fatalf("auto-commit find saw %d docs before commit, want 0", before)
	}

	// The transaction reads its own write (read-your-writes).
	ryw := sendCommand(t, nc, 3, docOf(
		func(b *bson.Builder) { b.AppendString("find", "txc") },
		withTxn(lsid, 1, false),
		func(b *bson.Builder) { b.AppendString("$db", "test") },
	))
	if got := arrayLen(t, mustDoc(t, ryw, "cursor"), "firstBatch"); got != 1 {
		t.Fatalf("read-your-writes find saw %d, want 1", got)
	}

	// Commit, then the auto-commit read sees the write.
	if commit := commitTxn(t, nc, 4, lsid, 1); !ok1(commit) {
		t.Fatalf("commit ok = false:\n%v", commit)
	}
	if after := findCount(t, nc, 5, "test", "txc"); after != 1 {
		t.Fatalf("auto-commit find saw %d docs after commit, want 1", after)
	}
}

func TestTransactionAbortDiscardsWrites(t *testing.T) {
	_, nc := testServer(t)
	lsid := makeLsid(2)

	sendCommand(t, nc, 1, docOf(
		func(b *bson.Builder) {
			txnInsert(b, "txa", lsid, 1, true, docOf(func(d *bson.Builder) { d.AppendInt32("x", 9) }))
		},
		func(b *bson.Builder) { b.AppendString("$db", "test") },
	))

	abort := sendCommand(t, nc, 2, docOf(
		func(b *bson.Builder) { b.AppendInt32("abortTransaction", 1) },
		func(b *bson.Builder) { b.AppendDocument("lsid", lsid) },
		func(b *bson.Builder) { b.AppendInt64("txnNumber", 1) },
		func(b *bson.Builder) { b.AppendBoolean("autocommit", false) },
		func(b *bson.Builder) { b.AppendString("$db", "admin") },
	))
	if !ok1(abort) {
		t.Fatalf("abort ok = false:\n%v", abort)
	}
	if n := findCount(t, nc, 3, "test", "txa"); n != 0 {
		t.Fatalf("find after abort saw %d docs, want 0", n)
	}
}

func TestContinueCommittedTransactionFails(t *testing.T) {
	_, nc := testServer(t)
	lsid := makeLsid(3)

	sendCommand(t, nc, 1, docOf(
		func(b *bson.Builder) {
			txnInsert(b, "txn", lsid, 1, true, docOf(func(d *bson.Builder) { d.AppendInt32("x", 1) }))
		},
		func(b *bson.Builder) { b.AppendString("$db", "test") },
	))
	commitTxn(t, nc, 2, lsid, 1)

	// Continuing the committed transaction must fail with NoSuchTransaction (251) and the
	// TransientTransactionError label.
	cont := sendCommand(t, nc, 3, docOf(
		func(b *bson.Builder) { b.AppendString("find", "txn") },
		withTxn(lsid, 1, false),
		func(b *bson.Builder) { b.AppendString("$db", "test") },
	))
	if v, ok := cont.Lookup("code"); !ok || v.Int32() != 251 {
		t.Fatalf("continue committed txn code = %v, want 251", v)
	}
	if !hasLabel(cont, "TransientTransactionError") {
		t.Fatalf("NoSuchTransaction reply missing TransientTransactionError label:\n%v", cont)
	}
}

func TestStartSessionReturnsID(t *testing.T) {
	_, nc := testServer(t)
	reply := sendCommand(t, nc, 1, docOf(
		func(b *bson.Builder) { b.AppendInt32("startSession", 1) },
		func(b *bson.Builder) { b.AppendString("$db", "admin") },
	))
	id := mustDoc(t, reply, "id")
	if v, ok := id.Lookup("id"); !ok || v.Type != bson.TypeBinary {
		t.Fatalf("startSession id.id = %v, want binary UUID", v)
	}
	if v, ok := reply.Lookup("timeoutMinutes"); !ok || v.Int32() != logicalSessionTimeoutMinutes {
		t.Fatalf("startSession timeoutMinutes = %v, want %d", v, logicalSessionTimeoutMinutes)
	}
}

func TestEndSessionsAbortsOpenTransaction(t *testing.T) {
	_, nc := testServer(t)
	lsid := makeLsid(4)

	sendCommand(t, nc, 1, docOf(
		func(b *bson.Builder) {
			txnInsert(b, "txe", lsid, 1, true, docOf(func(d *bson.Builder) { d.AppendInt32("x", 1) }))
		},
		func(b *bson.Builder) { b.AppendString("$db", "test") },
	))

	end := sendCommand(t, nc, 2, docOf(
		func(b *bson.Builder) {
			b.AppendArray("endSessions", bson.BuildArray(bson.RawValue{Type: bson.TypeDocument, Data: lsid}))
		},
		func(b *bson.Builder) { b.AppendString("$db", "admin") },
	))
	if !ok1(end) {
		t.Fatalf("endSessions ok = false:\n%v", end)
	}
	// The open transaction was rolled back when the session ended, so nothing is visible.
	if n := findCount(t, nc, 3, "test", "txe"); n != 0 {
		t.Fatalf("find after endSessions saw %d docs, want 0", n)
	}
}

// ok1 reports whether a reply carries ok:1.
func ok1(reply bson.Raw) bool {
	v, ok := reply.Lookup("ok")
	return ok && v.Double() == 1
}

// benchServer starts a wire server over a fresh database for a benchmark and returns a
// dialed connection plus a stop function. It mirrors testServer without the *testing.T
// cleanup hooks.
func benchServer(b *testing.B) (func(), net.Conn) {
	b.Helper()
	db, err := doc.Open(filepath.Join(b.TempDir(), "wire.doc"))
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	srv := NewServer(db, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	addrCh := make(chan string, 1)
	go func() { _ = srv.ListenAndServe(ctx, "localhost:0", func(a string) { addrCh <- a }) }()
	addr := <-addrCh

	nc, err := net.Dial("tcp", addr)
	if err != nil {
		b.Fatalf("dial: %v", err)
	}
	stop := func() {
		_ = nc.Close()
		cancel()
		_ = db.Close()
	}
	return stop, nc
}

// drainReply reads one OP_MSG reply and discards it, failing the benchmark on a read or
// frame error.
func drainReply(b *testing.B, nc net.Conn, requestID int32) {
	b.Helper()
	_ = nc.SetReadDeadline(time.Now().Add(5 * time.Second))
	var hb [headerLen]byte
	if _, err := io.ReadFull(nc, hb[:]); err != nil {
		b.Fatalf("read header: %v", err)
	}
	length := int32(binary.LittleEndian.Uint32(hb[0:4]))
	responseTo := int32(binary.LittleEndian.Uint32(hb[8:12]))
	if responseTo != requestID {
		b.Fatalf("responseTo = %d, want %d", responseTo, requestID)
	}
	payload := make([]byte, length-headerLen)
	if _, err := io.ReadFull(nc, payload); err != nil {
		b.Fatalf("read payload: %v", err)
	}
}

// BenchmarkTransactionCommit measures the cost of an open-insert-commit round trip over
// the wire, the hot path a driver drives for every short transaction.
func BenchmarkTransactionCommit(b *testing.B) {
	srv, nc := benchServer(b)
	defer srv()
	lsid := makeLsid(7)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		txn := int64(i + 1)
		reqID := int32(i*2 + 1)
		req := encodeRequestOpMsg(reqID, docOf(
			func(bd *bson.Builder) {
				txnInsert(bd, "bench", lsid, txn, true, docOf(func(d *bson.Builder) { d.AppendInt32("x", int32(i)) }))
			},
			func(bd *bson.Builder) { bd.AppendString("$db", "test") },
		))
		if _, err := nc.Write(req); err != nil {
			b.Fatalf("write insert: %v", err)
		}
		drainReply(b, nc, reqID)

		commitID := reqID + 1
		creq := encodeRequestOpMsg(commitID, docOf(
			func(bd *bson.Builder) { bd.AppendInt32("commitTransaction", 1) },
			func(bd *bson.Builder) { bd.AppendDocument("lsid", lsid) },
			func(bd *bson.Builder) { bd.AppendInt64("txnNumber", txn) },
			func(bd *bson.Builder) { bd.AppendBoolean("autocommit", false) },
			func(bd *bson.Builder) { bd.AppendString("$db", "admin") },
		))
		if _, err := nc.Write(creq); err != nil {
			b.Fatalf("write commit: %v", err)
		}
		drainReply(b, nc, commitID)
	}
}
