package wire

import (
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

// TestScramRFC7677Vector pins the SCRAM-SHA-256 primitives against the published RFC 7677
// example (user "user", password "pencil"). A wrong Hi, HMAC, or H would change the proof
// or the server signature, so matching both fixed constants validates the crypto end to
// end.
func TestScramRFC7677Vector(t *testing.T) {
	const (
		password    = "pencil"
		clientNonce = "rOprNGfwEbeRWgbNEkqO"
		serverNonce = "%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0"
		saltB64     = "W22ZaJ0SNY7soEsUEjb6gQ=="
		iter        = 4096

		wantProof     = "dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ="
		wantServerSig = "6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4="
	)
	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil {
		t.Fatalf("decode salt: %v", err)
	}
	combined := clientNonce + serverNonce
	clientFirstBare := "n=user,r=" + clientNonce
	serverFirst := "r=" + combined + ",s=" + saltB64 + ",i=4096"
	clientFinalNoProof := "c=biws,r=" + combined
	authMessage := clientFirstBare + "," + serverFirst + "," + clientFinalNoProof

	salted, _ := pbkdf2.Key(sha256.New, password, salt, iter, sha256.Size)
	clientKey := hmacSHA256(salted, []byte("Client Key"))
	storedKey := sha256Sum(clientKey)
	clientSig := hmacSHA256(storedKey, []byte(authMessage))
	proof := xorBytes(clientKey, clientSig)
	if got := base64.StdEncoding.EncodeToString(proof); got != wantProof {
		t.Fatalf("client proof = %s, want %s", got, wantProof)
	}

	serverKey := hmacSHA256(salted, []byte("Server Key"))
	serverSig := hmacSHA256(serverKey, []byte(authMessage))
	if got := base64.StdEncoding.EncodeToString(serverSig); got != wantServerSig {
		t.Fatalf("server signature = %s, want %s", got, wantServerSig)
	}
}

// TestScramServerRoundTrip drives startScram and finish with a client proof computed the
// way a real driver would, using the spec Appendix C salt and iteration count. It exercises
// the actual server path including the randomly generated server nonce.
func TestScramServerRoundTrip(t *testing.T) {
	const (
		user     = "alice"
		password = "p@ssw0rd"
	)
	salt, _ := base64.StdEncoding.DecodeString("W22ZaJ0SNY7soEsUEjb6gQ==")
	cred := deriveCredential(password, salt, 4096)

	clientNonce := "rOprNGfwEbeRWgbNEkqO"
	clientFirstBare := "n=" + user + ",r=" + clientNonce
	clientFirst := []byte("n,," + clientFirstBare)

	srv, serverFirst, err := startScram(clientFirst, cred, user, clientNonce, clientFirstBare)
	if err != nil {
		t.Fatalf("startScram: %v", err)
	}
	fields := parseScramFields(string(serverFirst))
	combined := fields["r"]
	if !strings.HasPrefix(combined, clientNonce) {
		t.Fatalf("server-first nonce %q does not start with client nonce", combined)
	}

	clientFinal := clientProof(t, cred.salt, cred.iterationCount, password, clientFirstBare, string(serverFirst), combined)
	serverFinal, ok := srv.finish([]byte(clientFinal))
	if !ok {
		t.Fatal("finish rejected a valid proof")
	}
	if !strings.HasPrefix(string(serverFinal), "v=") {
		t.Fatalf("server-final = %q, want a v= signature", serverFinal)
	}
}

// TestScramServerRejectsBadProof confirms a wrong password is refused at the proof step.
func TestScramServerRejectsBadProof(t *testing.T) {
	salt, _ := base64.StdEncoding.DecodeString("W22ZaJ0SNY7soEsUEjb6gQ==")
	cred := deriveCredential("right-password", salt, 4096)

	clientNonce := "abcdef0123456789"
	clientFirstBare := "n=bob,r=" + clientNonce
	srv, serverFirst, err := startScram([]byte("n,,"+clientFirstBare), cred, "bob", clientNonce, clientFirstBare)
	if err != nil {
		t.Fatalf("startScram: %v", err)
	}
	combined := parseScramFields(string(serverFirst))["r"]
	// Proof computed from the wrong password.
	bad := clientProof(t, salt, 4096, "wrong-password", clientFirstBare, string(serverFirst), combined)
	if _, ok := srv.finish([]byte(bad)); ok {
		t.Fatal("finish accepted a proof from the wrong password")
	}
}

// clientProof computes the client-final message a driver would send, given the negotiated
// salt, iteration count, password, and the messages exchanged so far.
func clientProof(t *testing.T, salt []byte, iter int, password, clientFirstBare, serverFirst, combined string) string {
	t.Helper()
	salted, err := pbkdf2.Key(sha256.New, password, salt, iter, sha256.Size)
	if err != nil {
		t.Fatalf("pbkdf2: %v", err)
	}
	clientKey := hmacSHA256(salted, []byte("Client Key"))
	storedKey := sha256Sum(clientKey)
	clientFinalNoProof := "c=biws,r=" + combined
	authMessage := clientFirstBare + "," + serverFirst + "," + clientFinalNoProof
	clientSig := hmacSHA256(storedKey, []byte(authMessage))
	proof := xorBytes(clientKey, clientSig)
	return clientFinalNoProof + ",p=" + base64.StdEncoding.EncodeToString(proof)
}
