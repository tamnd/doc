package wire

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tamnd/doc"
	"github.com/tamnd/doc/bson"
)

// testCA is a self-signed certificate authority used to sign the server and client
// certificates a TLS test needs.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	der  []byte
}

// newTestCA builds a throwaway CA.
func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "doc test CA"},
		NotBefore:             time.Unix(1600000000, 0),
		NotAfter:              time.Unix(1900000000, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("ca cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}
	return &testCA{cert: cert, key: key, der: der}
}

// issue signs a leaf certificate. serverName, when set, becomes a DNS SAN so a TLS client
// verifying the host accepts it; otherwise the leaf is a client certificate.
func (ca *testCA) issue(t *testing.T, subject pkix.Name, serverName string) (certPEM, keyPEM []byte, leaf *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano() & 0x7fffffff),
		Subject:      subject,
		NotBefore:    time.Unix(1600000000, 0),
		NotAfter:     time.Unix(1900000000, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if serverName != "" {
		tmpl.DNSNames = []string{serverName}
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("sign leaf: %v", err)
	}
	leaf, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, leaf
}

// writeFile writes bytes to a temp file and returns its path.
func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

// tlsServerFiles writes the CA bundle, server cert, and server key to dir and returns their
// paths.
func tlsServerFiles(t *testing.T, ca *testCA) (caFile, certFile, keyFile string) {
	t.Helper()
	dir := t.TempDir()
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.der})
	serverCert, serverKey, _ := ca.issue(t, pkix.Name{CommonName: "localhost"}, "localhost")
	caFile = writeFile(t, dir, "ca.pem", caPEM)
	certFile = writeFile(t, dir, "server.pem", serverCert)
	keyFile = writeFile(t, dir, "server.key", serverKey)
	return caFile, certFile, keyFile
}

// startTLSServer starts a wire server with the given TLS options and returns the bound
// address and a client tls.Config that trusts the CA.
func startTLSServer(t *testing.T, ca *testCA, opts Options) (string, *tls.Config) {
	t.Helper()
	db, err := doc.Open(filepath.Join(t.TempDir(), "tls.doc"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	srv := NewServer(db, opts)
	ctx, cancel := context.WithCancel(context.Background())
	addrCh := make(chan string, 1)
	go func() { _ = srv.ListenAndServe(ctx, "localhost:0", func(a string) { addrCh <- a }) }()

	var addr string
	select {
	case addr = <-addrCh:
	case <-time.After(5 * time.Second):
		t.Fatal("server never reported ready")
	}
	t.Cleanup(func() {
		cancel()
		_ = db.Close()
	})

	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	return addr, &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12}
}

// TestTLSScramOverTLS runs the full SCRAM flow over a requireTLS listener, proving the
// handshake, the localhost exception, and authentication all work through the TLS wrapper.
func TestTLSScramOverTLS(t *testing.T) {
	ca := newTestCA(t)
	caFile, certFile, keyFile := tlsServerFiles(t, ca)
	addr, clientCfg := startTLSServer(t, ca, Options{
		AuthRequired: true,
		TLS:          TLSOptions{Mode: TLSRequire, CertFile: certFile, KeyFile: keyFile, CAFile: caFile},
	})

	nc, err := tls.Dial("tcp", addr, clientCfg)
	if err != nil {
		t.Fatalf("tls dial: %v", err)
	}
	defer func() { _ = nc.Close() }()

	// The localhost exception still applies over TLS: create the first user, then
	// authenticate as it.
	createUser(t, nc, 1, "admin", "root", "secret", "root")
	cont := scramAuthenticate(t, nc, 2, "admin", "root", "secret")
	if v, ok := cont.Lookup("done"); !ok || !v.Boolean() {
		t.Fatalf("scram over tls done = %v, want true: %v", v, cont)
	}
}

// TestRequireTLSRejectsPlaintext checks that a plaintext connection to a requireTLS listener
// cannot run a command: the TLS server rejects the non-handshake bytes.
func TestRequireTLSRejectsPlaintext(t *testing.T) {
	ca := newTestCA(t)
	caFile, certFile, keyFile := tlsServerFiles(t, ca)
	addr, _ := startTLSServer(t, ca, Options{
		TLS: TLSOptions{Mode: TLSRequire, CertFile: certFile, KeyFile: keyFile, CAFile: caFile},
	})

	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = nc.Close() }()
	_ = nc.SetDeadline(time.Now().Add(2 * time.Second))

	// A plaintext hello is read by the TLS server as a malformed record, the handshake
	// fails, and the connection is dropped, so the read returns an error instead of a reply.
	body := docOf(
		func(b *bson.Builder) { b.AppendInt32("ping", 1) },
		func(b *bson.Builder) { b.AppendString("$db", "admin") },
	)
	msg := encodeRequestOpMsg(1, body)
	if _, err := nc.Write(msg); err != nil {
		return // a write failure is an acceptable rejection too
	}
	buf := make([]byte, 16)
	if _, err := nc.Read(buf); err == nil {
		t.Fatal("requireTLS listener answered a plaintext command")
	}
}

// TestX509Authentication drives the MONGODB-X509 mechanism end to end: a client presents a
// CA-signed certificate, the server matches its subject to an $external user, and an
// authorized command runs.
func TestX509Authentication(t *testing.T) {
	ca := newTestCA(t)
	caFile, certFile, keyFile := tlsServerFiles(t, ca)
	clientCert, clientKey, clientLeaf := ca.issue(t,
		pkix.Name{CommonName: "alice", Organization: []string{"doc"}}, "")
	subject := clientLeaf.Subject.String()

	addr, clientCfg := startTLSServer(t, ca, Options{
		AuthRequired: true,
		TLS:          TLSOptions{Mode: TLSRequire, CertFile: certFile, KeyFile: keyFile, CAFile: caFile},
	})

	// Bootstrap over the localhost exception (no client cert needed) and register the
	// external user keyed by the certificate subject.
	boot, err := tls.Dial("tcp", addr, clientCfg)
	if err != nil {
		t.Fatalf("bootstrap dial: %v", err)
	}
	createExternalUser(t, boot, 1, subject, "root")
	_ = boot.Close()

	// Reconnect presenting the client certificate and authenticate with MONGODB-X509.
	pair, err := tls.X509KeyPair(clientCert, clientKey)
	if err != nil {
		t.Fatalf("client keypair: %v", err)
	}
	authCfg := clientCfg.Clone()
	authCfg.Certificates = []tls.Certificate{pair}
	nc, err := tls.Dial("tcp", addr, authCfg)
	if err != nil {
		t.Fatalf("auth dial: %v", err)
	}
	defer func() { _ = nc.Close() }()

	start := sendCommand(t, nc, 2, docOf(
		func(b *bson.Builder) { b.AppendInt32("saslStart", 1) },
		func(b *bson.Builder) { b.AppendString("mechanism", x509MechName) },
		func(b *bson.Builder) { b.AppendBinary("payload", 0, nil) },
		func(b *bson.Builder) { b.AppendString("$db", externalDB) },
	))
	if v, ok := start.Lookup("done"); !ok || !v.Boolean() {
		t.Fatalf("x509 saslStart done = %v, want true: %v", v, start)
	}

	// The authenticated root identity may read.
	find := sendCommand(t, nc, 3, docOf(
		func(b *bson.Builder) { b.AppendString("find", "things") },
		func(b *bson.Builder) { b.AppendString("$db", "test") },
	))
	if v, ok := find.Lookup("ok"); !ok || v.Double() != 1 {
		t.Fatalf("x509 authenticated find ok = %v, want 1: %v", v, find)
	}
}

// TestX509AdvertisedInHello checks that a server with a client CA advertises MONGODB-X509
// alongside SCRAM-SHA-256.
func TestX509AdvertisedInHello(t *testing.T) {
	ca := newTestCA(t)
	caFile, certFile, keyFile := tlsServerFiles(t, ca)
	addr, clientCfg := startTLSServer(t, ca, Options{
		AuthRequired: true,
		TLS:          TLSOptions{Mode: TLSRequire, CertFile: certFile, KeyFile: keyFile, CAFile: caFile},
	})
	nc, err := tls.Dial("tcp", addr, clientCfg)
	if err != nil {
		t.Fatalf("tls dial: %v", err)
	}
	defer func() { _ = nc.Close() }()

	hello := sendCommand(t, nc, 1, docOf(
		func(b *bson.Builder) { b.AppendInt32("hello", 1) },
		func(b *bson.Builder) { b.AppendString("$db", "admin") },
	))
	v, ok := hello.Lookup("saslSupportedMechs")
	if !ok || v.Type != bson.TypeArray {
		t.Fatalf("hello has no saslSupportedMechs array: %v", hello)
	}
	var mechs []string
	for _, e := range arrayElements(v) {
		if e.Type == bson.TypeString {
			mechs = append(mechs, e.StringValue())
		}
	}
	if !contains(mechs, scramMechName) || !contains(mechs, x509MechName) {
		t.Fatalf("advertised mechs = %v, want both SCRAM and X509", mechs)
	}
}

// createExternalUser creates an X.509 user under $external keyed by its certificate subject.
func createExternalUser(t *testing.T, nc net.Conn, reqID int32, subject string, roles ...string) {
	t.Helper()
	roleVals := make([]bson.RawValue, len(roles))
	for i, r := range roles {
		roleVals[i] = bson.RawValue{Type: bson.TypeString, Data: encodeString(r)}
	}
	body := docOf(
		func(b *bson.Builder) { b.AppendString("createUser", subject) },
		func(b *bson.Builder) { b.AppendArray("roles", bson.BuildArray(roleVals...)) },
		func(b *bson.Builder) { b.AppendString("$db", externalDB) },
	)
	reply := sendCommand(t, nc, reqID, body)
	if v, ok := reply.Lookup("ok"); !ok || v.Double() != 1 {
		t.Fatalf("createExternalUser ok = %v: %v", v, reply)
	}
}

func contains(set []string, want string) bool {
	for _, s := range set {
		if s == want {
			return true
		}
	}
	return false
}
