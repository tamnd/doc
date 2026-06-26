package wire

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/tamnd/doc/bson"
)

// docOf builds a small BSON document from builder ops, the same shape cmdDoc uses.
func docOf(elems ...func(*bson.Builder)) bson.Raw {
	b := bson.NewBuilder()
	for _, e := range elems {
		e(b)
	}
	return b.Build()
}

// insertDocs sends an insert command carrying docs in the body array and returns the
// reply.
func insertDocs(t *testing.T, nc net.Conn, reqID int32, db, coll string, docs ...bson.Raw) bson.Raw {
	t.Helper()
	body := docOf(
		func(b *bson.Builder) { b.AppendString("insert", coll) },
		func(b *bson.Builder) { b.AppendArray("documents", docArray(docs)) },
		func(b *bson.Builder) { b.AppendString("$db", db) },
	)
	return sendCommand(t, nc, reqID, body)
}

func TestInsertThenFind(t *testing.T) {
	_, nc := testServer(t)

	reply := insertDocs(t, nc, 1, "test", "things",
		docOf(func(b *bson.Builder) { b.AppendInt32("x", 1) }),
		docOf(func(b *bson.Builder) { b.AppendInt32("x", 2) }),
	)
	if v, ok := reply.Lookup("n"); !ok || v.Int32() != 2 {
		t.Fatalf("insert n = %v, want 2", v)
	}

	find := sendCommand(t, nc, 2, docOf(
		func(b *bson.Builder) { b.AppendString("find", "things") },
		func(b *bson.Builder) { b.AppendString("$db", "test") },
	))
	cursor := mustDoc(t, find, "cursor")
	if v, ok := cursor.Lookup("id"); !ok || v.Int64() != 0 {
		t.Fatalf("find cursor id = %v, want 0 (exhausted)", v)
	}
	if got := arrayLen(t, cursor, "firstBatch"); got != 2 {
		t.Fatalf("firstBatch len = %d, want 2", got)
	}
	if v, ok := cursor.Lookup("ns"); !ok || v.StringValue() != "test.things" {
		t.Fatalf("cursor ns = %v, want test.things", v)
	}
}

func TestFindBatchingAndGetMore(t *testing.T) {
	_, nc := testServer(t)

	var docs []bson.Raw
	for i := 0; i < 5; i++ {
		i := i
		docs = append(docs, docOf(func(b *bson.Builder) { b.AppendInt32("x", int32(i)) }))
	}
	insertDocs(t, nc, 1, "test", "nums", docs...)

	find := sendCommand(t, nc, 2, docOf(
		func(b *bson.Builder) { b.AppendString("find", "nums") },
		func(b *bson.Builder) { b.AppendInt32("batchSize", 2) },
		func(b *bson.Builder) { b.AppendString("$db", "test") },
	))
	cursor := mustDoc(t, find, "cursor")
	id, ok := cursor.Lookup("id")
	if !ok || id.Int64() == 0 {
		t.Fatalf("find cursor id = %v, want nonzero", id)
	}
	if got := arrayLen(t, cursor, "firstBatch"); got != 2 {
		t.Fatalf("firstBatch len = %d, want 2", got)
	}

	// Pull the rest with getMore.
	seen := 2
	cursorID := id.Int64()
	for cursorID != 0 {
		gm := sendCommand(t, nc, 3, docOf(
			func(b *bson.Builder) { b.AppendInt64("getMore", cursorID) },
			func(b *bson.Builder) { b.AppendString("collection", "nums") },
			func(b *bson.Builder) { b.AppendInt32("batchSize", 2) },
			func(b *bson.Builder) { b.AppendString("$db", "test") },
		))
		c := mustDoc(t, gm, "cursor")
		seen += arrayLen(t, c, "nextBatch")
		v, _ := c.Lookup("id")
		cursorID = v.Int64()
	}
	if seen != 5 {
		t.Fatalf("getMore streamed %d docs, want 5", seen)
	}
}

func TestGetMoreUnknownCursor(t *testing.T) {
	_, nc := testServer(t)
	reply := sendCommand(t, nc, 1, docOf(
		func(b *bson.Builder) { b.AppendInt64("getMore", 424242) },
		func(b *bson.Builder) { b.AppendString("collection", "x") },
		func(b *bson.Builder) { b.AppendString("$db", "test") },
	))
	if v, ok := reply.Lookup("code"); !ok || v.Int32() != 43 {
		t.Fatalf("getMore unknown cursor code = %v, want 43 CursorNotFound", v)
	}
}

func TestKillCursors(t *testing.T) {
	srv, nc := testServer(t)

	var docs []bson.Raw
	for i := 0; i < 6; i++ {
		i := i
		docs = append(docs, docOf(func(b *bson.Builder) { b.AppendInt32("x", int32(i)) }))
	}
	insertDocs(t, nc, 1, "test", "kc", docs...)

	find := sendCommand(t, nc, 2, docOf(
		func(b *bson.Builder) { b.AppendString("find", "kc") },
		func(b *bson.Builder) { b.AppendInt32("batchSize", 2) },
		func(b *bson.Builder) { b.AppendString("$db", "test") },
	))
	id, _ := mustDoc(t, find, "cursor").Lookup("id")
	if srv.cursors.count() != 1 {
		t.Fatalf("open cursors = %d, want 1", srv.cursors.count())
	}

	kill := sendCommand(t, nc, 3, docOf(
		func(b *bson.Builder) { b.AppendString("killCursors", "kc") },
		func(b *bson.Builder) { b.AppendArray("cursors", int64Array([]int64{id.Int64()})) },
		func(b *bson.Builder) { b.AppendString("$db", "test") },
	))
	if got := arrayLen(t, kill, "cursorsKilled"); got != 1 {
		t.Fatalf("cursorsKilled = %d, want 1", got)
	}
	if srv.cursors.count() != 0 {
		t.Fatalf("open cursors after kill = %d, want 0", srv.cursors.count())
	}
}

func TestUpdateAndUpsert(t *testing.T) {
	_, nc := testServer(t)
	insertDocs(t, nc, 1, "test", "u",
		docOf(func(b *bson.Builder) { b.AppendString("k", "a"); b.AppendInt32("n", 1) }),
	)

	// Update the existing doc.
	upd := sendCommand(t, nc, 2, docOf(
		func(b *bson.Builder) { b.AppendString("update", "u") },
		func(b *bson.Builder) {
			b.AppendArray("updates", docArray([]bson.Raw{docOf(
				func(s *bson.Builder) {
					s.AppendDocument("q", docOf(func(q *bson.Builder) { q.AppendString("k", "a") }))
					s.AppendDocument("u", docOf(func(u *bson.Builder) {
						u.AppendDocument("$set", docOf(func(set *bson.Builder) { set.AppendInt32("n", 9) }))
					}))
				},
			)}))
		},
		func(b *bson.Builder) { b.AppendString("$db", "test") },
	))
	if v, ok := upd.Lookup("n"); !ok || v.Int32() != 1 {
		t.Fatalf("update n = %v, want 1", v)
	}
	if v, ok := upd.Lookup("nModified"); !ok || v.Int32() != 1 {
		t.Fatalf("update nModified = %v, want 1", v)
	}

	// Upsert a new doc.
	ups := sendCommand(t, nc, 3, docOf(
		func(b *bson.Builder) { b.AppendString("update", "u") },
		func(b *bson.Builder) {
			b.AppendArray("updates", docArray([]bson.Raw{docOf(
				func(s *bson.Builder) {
					s.AppendDocument("q", docOf(func(q *bson.Builder) { q.AppendString("k", "b") }))
					s.AppendDocument("u", docOf(func(u *bson.Builder) {
						u.AppendDocument("$set", docOf(func(set *bson.Builder) { set.AppendInt32("n", 5) }))
					}))
					s.AppendBoolean("upsert", true)
				},
			)}))
		},
		func(b *bson.Builder) { b.AppendString("$db", "test") },
	))
	if arrayLen(t, ups, "upserted") != 1 {
		t.Fatalf("upserted array len = %d, want 1", arrayLen(t, ups, "upserted"))
	}
}

func TestDeleteOverWire(t *testing.T) {
	_, nc := testServer(t)
	insertDocs(t, nc, 1, "test", "d",
		docOf(func(b *bson.Builder) { b.AppendInt32("x", 1) }),
		docOf(func(b *bson.Builder) { b.AppendInt32("x", 1) }),
		docOf(func(b *bson.Builder) { b.AppendInt32("x", 2) }),
	)
	del := sendCommand(t, nc, 2, docOf(
		func(b *bson.Builder) { b.AppendString("delete", "d") },
		func(b *bson.Builder) {
			b.AppendArray("deletes", docArray([]bson.Raw{docOf(
				func(s *bson.Builder) {
					s.AppendDocument("q", docOf(func(q *bson.Builder) { q.AppendInt32("x", 1) }))
					s.AppendInt32("limit", 0)
				},
			)}))
		},
		func(b *bson.Builder) { b.AppendString("$db", "test") },
	))
	if v, ok := del.Lookup("n"); !ok || v.Int32() != 2 {
		t.Fatalf("delete n = %v, want 2", v)
	}
}

func TestFindAndModify(t *testing.T) {
	_, nc := testServer(t)
	insertDocs(t, nc, 1, "test", "fam",
		docOf(func(b *bson.Builder) { b.AppendString("k", "a"); b.AppendInt32("n", 1) }),
	)
	reply := sendCommand(t, nc, 2, docOf(
		func(b *bson.Builder) { b.AppendString("findAndModify", "fam") },
		func(b *bson.Builder) {
			b.AppendDocument("query", docOf(func(q *bson.Builder) { q.AppendString("k", "a") }))
		},
		func(b *bson.Builder) {
			b.AppendDocument("update", docOf(func(u *bson.Builder) {
				u.AppendDocument("$set", docOf(func(set *bson.Builder) { set.AppendInt32("n", 42) }))
			}))
		},
		func(b *bson.Builder) { b.AppendBoolean("new", true) },
		func(b *bson.Builder) { b.AppendString("$db", "test") },
	))
	value := mustDoc(t, reply, "value")
	if v, ok := value.Lookup("n"); !ok || v.Int32() != 42 {
		t.Fatalf("findAndModify returned n = %v, want 42 (after image)", v)
	}
}

// TestInsertDocumentSequence sends the documents as a kind-1 sequence, the framing a
// driver uses for a large bulk write, and checks the server merges it.
func TestInsertDocumentSequence(t *testing.T) {
	_, nc := testServer(t)
	body := docOf(
		func(b *bson.Builder) { b.AppendString("insert", "seq") },
		func(b *bson.Builder) { b.AppendString("$db", "test") },
	)
	seqDocs := []bson.Raw{
		docOf(func(b *bson.Builder) { b.AppendInt32("x", 1) }),
		docOf(func(b *bson.Builder) { b.AppendInt32("x", 2) }),
		docOf(func(b *bson.Builder) { b.AppendInt32("x", 3) }),
	}
	req := encodeRequestOpMsgSeq(1, body, "documents", seqDocs)
	if _, err := nc.Write(req); err != nil {
		t.Fatalf("write: %v", err)
	}
	reply := readReplyOpMsg(t, nc, 1)
	if v, ok := reply.Lookup("n"); !ok || v.Int32() != 3 {
		t.Fatalf("insert sequence n = %v, want 3", v)
	}
}

// TestCompressionRoundTrip negotiates zlib, sends a compressed request, and reads the
// reply, exercising both decompression of the request and compression of a large reply.
func TestCompressionRoundTrip(t *testing.T) {
	_, nc := testServer(t)

	hello := sendCommand(t, nc, 1, docOf(
		func(b *bson.Builder) { b.AppendInt32("hello", 1) },
		func(b *bson.Builder) {
			b.AppendArray("compression", bson.BuildArray(bson.RawValue{Type: bson.TypeString, Data: encodeString("zlib")}))
		},
		func(b *bson.Builder) { b.AppendString("$db", "admin") },
	))
	if arrayLen(t, hello, "compression") != 1 {
		t.Fatalf("hello advertised compression len = %d, want 1", arrayLen(t, hello, "compression"))
	}

	// Insert enough documents that the find reply exceeds the compression threshold.
	var docs []bson.Raw
	for i := 0; i < 60; i++ {
		i := i
		docs = append(docs, docOf(func(b *bson.Builder) {
			b.AppendInt32("x", int32(i))
			b.AppendString("pad", "the quick brown fox jumps over the lazy dog")
		}))
	}
	insertDocs(t, nc, 2, "test", "cz", docs...)

	// Send the find inside an OP_COMPRESSED envelope.
	findBody := docOf(
		func(b *bson.Builder) { b.AppendString("find", "cz") },
		func(b *bson.Builder) { b.AppendInt32("batchSize", 200) },
		func(b *bson.Builder) { b.AppendString("$db", "test") },
	)
	findMsg := encodeRequestOpMsg(3, findBody)
	if _, err := nc.Write(wrapCompressed(findMsg, compressorZlib)); err != nil {
		t.Fatalf("write compressed: %v", err)
	}

	reply := readAnyReply(t, nc, 3)
	cursor := mustDoc(t, reply, "cursor")
	if got := arrayLen(t, cursor, "firstBatch"); got != 60 {
		t.Fatalf("compressed find firstBatch len = %d, want 60", got)
	}
}

// encodeRequestOpMsgSeq frames an OP_MSG with a kind-0 body and one kind-1 document
// sequence under identifier id.
func encodeRequestOpMsgSeq(requestID int32, body bson.Raw, id string, docs []bson.Raw) []byte {
	// Build the kind-1 section payload: size(4) + cstring id + docs.
	seq := make([]byte, 4)
	seq = append(seq, []byte(id)...)
	seq = append(seq, 0)
	for _, d := range docs {
		seq = append(seq, d...)
	}
	binary.LittleEndian.PutUint32(seq[0:4], uint32(len(seq)))

	total := headerLen + 4 + 1 + len(body) + 1 + len(seq)
	buf := make([]byte, headerLen+4, total)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(total))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(requestID))
	binary.LittleEndian.PutUint32(buf[8:12], 0)
	binary.LittleEndian.PutUint32(buf[12:16], uint32(opMsg))
	binary.LittleEndian.PutUint32(buf[16:20], 0) // flags
	buf = append(buf, 0)                         // kind 0
	buf = append(buf, body...)                   // body doc
	buf = append(buf, 1)                         // kind 1
	buf = append(buf, seq...)                    // sequence section
	return buf
}

// readAnyReply reads a reply that may be OP_MSG or OP_COMPRESSED and returns the body.
func readAnyReply(t *testing.T, nc net.Conn, requestID int32) bson.Raw {
	t.Helper()
	_ = nc.SetReadDeadline(time.Now().Add(5 * time.Second))
	var hb [headerLen]byte
	if _, err := io.ReadFull(nc, hb[:]); err != nil {
		t.Fatalf("read header: %v", err)
	}
	length := int32(binary.LittleEndian.Uint32(hb[0:4]))
	opcode := int32(binary.LittleEndian.Uint32(hb[12:16]))
	payload := make([]byte, length-headerLen)
	if _, err := io.ReadFull(nc, payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	switch opcode {
	case opMsg:
		in, err := parseOpMsg(payload)
		if err != nil {
			t.Fatalf("parse reply: %v", err)
		}
		return in.body
	case opCompressed:
		origOp, inner, err := parseOpCompressed(payload, DefaultMaxMessageBytes)
		if err != nil {
			t.Fatalf("parse compressed: %v", err)
		}
		if origOp != opMsg {
			t.Fatalf("compressed inner opcode = %d, want OP_MSG", origOp)
		}
		in, err := parseOpMsg(inner)
		if err != nil {
			t.Fatalf("parse inner: %v", err)
		}
		return in.body
	default:
		t.Fatalf("reply opcode = %d, want OP_MSG or OP_COMPRESSED", opcode)
		return nil
	}
}

// mustDoc returns a sub-document field or fails.
func mustDoc(t *testing.T, d bson.Raw, key string) bson.Raw {
	t.Helper()
	v, ok := d.Lookup(key)
	if !ok || v.Type != bson.TypeDocument {
		t.Fatalf("missing document field %q in %v", key, d)
	}
	return v.Document()
}

// arrayLen returns the element count of an array field.
func arrayLen(t *testing.T, d bson.Raw, key string) int {
	t.Helper()
	v, ok := d.Lookup(key)
	if !ok || v.Type != bson.TypeArray {
		t.Fatalf("missing array field %q in %v", key, d)
	}
	elems, err := v.Document().Elements()
	if err != nil {
		t.Fatalf("array %q elements: %v", key, err)
	}
	return len(elems)
}
