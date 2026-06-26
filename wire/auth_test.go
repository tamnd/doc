package wire

import (
	"context"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/tamnd/doc"
	"github.com/tamnd/doc/bson"
)

// testAuthServer starts a wire server with authentication required and returns a dialed
// loopback connection. Because the dial is over loopback and no users exist yet, the first
// connection enjoys the localhost exception and can create the first user.
func testAuthServer(t *testing.T) (*Server, net.Conn) {
	t.Helper()
	db, err := doc.Open(filepath.Join(t.TempDir(), "auth.doc"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	srv := NewServer(db, Options{AuthRequired: true})

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

// createUser runs createUser over a connection.
func createUser(t *testing.T, nc net.Conn, reqID int32, db, user, pwd string, roles ...string) bson.Raw {
	t.Helper()
	roleVals := make([]bson.RawValue, len(roles))
	for i, r := range roles {
		roleVals[i] = bson.RawValue{Type: bson.TypeString, Data: encodeString(r)}
	}
	body := docOf(
		func(b *bson.Builder) { b.AppendString("createUser", user) },
		func(b *bson.Builder) { b.AppendString("pwd", pwd) },
		func(b *bson.Builder) { b.AppendArray("roles", bson.BuildArray(roleVals...)) },
		func(b *bson.Builder) { b.AppendString("$db", db) },
	)
	return sendCommand(t, nc, reqID, body)
}

// scramAuthenticate runs a full SCRAM-SHA-256 exchange over the connection and returns the
// final saslContinue reply, verifying the server signature.
func scramAuthenticate(t *testing.T, nc net.Conn, reqID int32, db, user, password string) bson.Raw {
	t.Helper()
	clientNonce := "fixedClientNonce0123456789"
	clientFirstBare := "n=" + user + ",r=" + clientNonce
	clientFirst := "n,," + clientFirstBare

	start := sendCommand(t, nc, reqID, docOf(
		func(b *bson.Builder) { b.AppendInt32("saslStart", 1) },
		func(b *bson.Builder) { b.AppendString("mechanism", scramMechName) },
		func(b *bson.Builder) { b.AppendBinary("payload", 0, []byte(clientFirst)) },
		func(b *bson.Builder) { b.AppendString("$db", db) },
	))
	convID, ok := start.Lookup("conversationId")
	if !ok {
		t.Fatalf("saslStart reply has no conversationId: %v", start)
	}
	serverFirst := payloadBytes(t, start)
	fields := parseScramFields(string(serverFirst))
	salt, err := base64.StdEncoding.DecodeString(fields["s"])
	if err != nil {
		t.Fatalf("decode salt: %v", err)
	}
	iter := parseIterationCount(fields["i"])
	combined := fields["r"]

	salted, _ := pbkdf2.Key(sha256.New, password, salt, iter, sha256.Size)
	clientKey := hmacSHA256(salted, []byte("Client Key"))
	storedKey := sha256Sum(clientKey)
	clientFinalNoProof := "c=biws,r=" + combined
	authMessage := clientFirstBare + "," + string(serverFirst) + "," + clientFinalNoProof
	clientSig := hmacSHA256(storedKey, []byte(authMessage))
	proof := xorBytes(clientKey, clientSig)
	clientFinal := clientFinalNoProof + ",p=" + base64.StdEncoding.EncodeToString(proof)

	cont := sendCommand(t, nc, reqID+1, docOf(
		func(b *bson.Builder) { b.AppendInt32("saslContinue", 1) },
		func(b *bson.Builder) { b.AppendInt32("conversationId", convID.Int32()) },
		func(b *bson.Builder) { b.AppendBinary("payload", 0, []byte(clientFinal)) },
		func(b *bson.Builder) { b.AppendString("$db", db) },
	))
	return cont
}

// payloadBytes reads the binary payload field from a SASL reply.
func payloadBytes(t *testing.T, d bson.Raw) []byte {
	t.Helper()
	v, ok := d.Lookup("payload")
	if !ok {
		t.Fatalf("reply has no payload: %v", d)
	}
	_, data, ok := v.Binary()
	if !ok {
		t.Fatalf("payload is not binary: %v", v)
	}
	return data
}

// TestLocalhostExceptionCreatesFirstUser checks that a loopback connection may create the
// first user without authenticating, and that the exception is revoked afterward.
func TestLocalhostExceptionCreatesFirstUser(t *testing.T) {
	srv, nc := testAuthServer(t)

	reply := createUser(t, nc, 1, "admin", "root", "secret", "root")
	if v, ok := reply.Lookup("ok"); !ok || v.Double() != 1 {
		t.Fatalf("createUser ok = %v, want 1: %v", v, reply)
	}
	if !srv.hasUsers(context.Background()) {
		t.Fatal("server reports no users after createUser")
	}

	// With a user now present the exception is gone: an unauthenticated find is refused.
	find := sendCommand(t, nc, 2, docOf(
		func(b *bson.Builder) { b.AppendString("find", "things") },
		func(b *bson.Builder) { b.AppendString("$db", "test") },
	))
	if v, ok := find.Lookup("code"); !ok || v.Int32() != 13 {
		t.Fatalf("unauthenticated find code = %v, want 13 Unauthorized", v)
	}
}

// TestScramAuthenticateThenCommand authenticates a real user and runs an authorized
// command.
func TestScramAuthenticateThenCommand(t *testing.T) {
	_, nc := testAuthServer(t)
	createUser(t, nc, 1, "admin", "alice", "p@ssw0rd", "root")

	cont := scramAuthenticate(t, nc, 2, "admin", "alice", "p@ssw0rd")
	if v, ok := cont.Lookup("done"); !ok || !v.Boolean() {
		t.Fatalf("saslContinue done = %v, want true: %v", v, cont)
	}

	// An authenticated root user can insert and read.
	insertDocs(t, nc, 5, "test", "things",
		docOf(func(b *bson.Builder) { b.AppendInt32("x", 1) }))
	find := sendCommand(t, nc, 6, docOf(
		func(b *bson.Builder) { b.AppendString("find", "things") },
		func(b *bson.Builder) { b.AppendString("$db", "test") },
	))
	if v, ok := find.Lookup("ok"); !ok || v.Double() != 1 {
		t.Fatalf("authenticated find ok = %v, want 1: %v", v, find)
	}
}

// TestScramWrongPassword confirms a bad password is rejected with AuthenticationFailed.
func TestScramWrongPassword(t *testing.T) {
	_, nc := testAuthServer(t)
	createUser(t, nc, 1, "admin", "bob", "correct-horse", "root")

	cont := scramAuthenticate(t, nc, 2, "admin", "bob", "wrong-horse")
	if v, ok := cont.Lookup("code"); !ok || v.Int32() != 18 {
		t.Fatalf("wrong password code = %v, want 18 AuthenticationFailed: %v", v, cont)
	}
}

// TestRoleEnforcement gives a user read-only on one database and checks a write is refused
// there while a read is allowed.
func TestRoleEnforcement(t *testing.T) {
	_, admin := testAuthServer(t)
	// Bootstrap an admin via the localhost exception, then authenticate as it so the
	// connection can create more users once the exception is revoked.
	createUser(t, admin, 1, "admin", "root", "secret", "root")
	if v, ok := scramAuthenticate(t, admin, 2, "admin", "root", "secret").Lookup("done"); !ok || !v.Boolean() {
		t.Fatal("admin auth failed")
	}
	createReadUser(t, admin, 4, "test", "reader", "pw")

	// A separate connection authenticates as the read-only user.
	_, nc := dialSame(t, admin)
	cont := scramAuthenticate(t, nc, 1, "test", "reader", "pw")
	if v, ok := cont.Lookup("done"); !ok || !v.Boolean() {
		t.Fatalf("reader auth failed: %v", cont)
	}

	// A read is allowed.
	find := sendCommand(t, nc, 3, docOf(
		func(b *bson.Builder) { b.AppendString("find", "c") },
		func(b *bson.Builder) { b.AppendString("$db", "test") },
	))
	if v, ok := find.Lookup("ok"); !ok || v.Double() != 1 {
		t.Fatalf("reader find ok = %v, want 1: %v", v, find)
	}

	// A write is refused with Unauthorized.
	ins := insertDocs(t, nc, 5, "test", "c",
		docOf(func(b *bson.Builder) { b.AppendInt32("x", 1) }))
	if v, ok := ins.Lookup("code"); !ok || v.Int32() != 13 {
		t.Fatalf("reader insert code = %v, want 13 Unauthorized: %v", v, ins)
	}
}

// createReadUser creates a user with a single {role:"read", db} grant.
func createReadUser(t *testing.T, nc net.Conn, reqID int32, db, user, pwd string) {
	t.Helper()
	roleDoc := bson.NewBuilder().
		AppendString("role", "read").
		AppendString("db", db).
		Build()
	body := docOf(
		func(b *bson.Builder) { b.AppendString("createUser", user) },
		func(b *bson.Builder) { b.AppendString("pwd", pwd) },
		func(b *bson.Builder) {
			b.AppendArray("roles", bson.BuildArray(bson.RawValue{Type: bson.TypeDocument, Data: roleDoc}))
		},
		func(b *bson.Builder) { b.AppendString("$db", db) },
	)
	reply := sendCommand(t, nc, reqID, body)
	if v, ok := reply.Lookup("ok"); !ok || v.Double() != 1 {
		t.Fatalf("createReadUser ok = %v: %v", v, reply)
	}
}

// dialSame opens a second connection to the same server as an existing connection.
func dialSame(t *testing.T, existing net.Conn) (*Server, net.Conn) {
	t.Helper()
	nc, err := net.Dial("tcp", existing.RemoteAddr().String())
	if err != nil {
		t.Fatalf("dial second conn: %v", err)
	}
	t.Cleanup(func() { _ = nc.Close() })
	return nil, nc
}

// TestUsersInfo round-trips createUser then usersInfo and checks the credential is not
// disclosed.
func TestUsersInfo(t *testing.T) {
	_, nc := testAuthServer(t)
	// Bootstrap root via the exception, authenticate, then create carol and read her back.
	createUser(t, nc, 1, "admin", "root", "secret", "root")
	if v, ok := scramAuthenticate(t, nc, 2, "admin", "root", "secret").Lookup("done"); !ok || !v.Boolean() {
		t.Fatal("admin auth failed")
	}
	createUser(t, nc, 4, "admin", "carol", "pw", "root")

	info := sendCommand(t, nc, 5, docOf(
		func(b *bson.Builder) { b.AppendString("usersInfo", "carol") },
		func(b *bson.Builder) { b.AppendString("$db", "admin") },
	))
	if arrayLen(t, info, "users") != 1 {
		t.Fatalf("usersInfo users len = %d, want 1", arrayLen(t, info, "users"))
	}
	users, _ := info.Lookup("users")
	elems, _ := users.Document().Elements()
	first := elems[0].Value.Document()
	if _, ok := first.Lookup("credentials"); ok {
		t.Fatal("usersInfo leaked credentials")
	}
	if lookupString(first, "user") != "carol" {
		t.Fatalf("usersInfo user = %q, want carol", lookupString(first, "user"))
	}
}
