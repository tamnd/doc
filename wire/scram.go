package wire

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// SCRAM-SHA-256 (RFC 5802) is doc's primary wire authentication mechanism (spec 2061
// doc 16 §8). The server stores a salted credential, never the password, and proves the
// password over the wire without ever seeing it. This file is the crypto and the message
// grammar; the conversation state machine lives on the connection in auth.go.

// defaultIterationCount is the PBKDF2 work factor for a freshly created credential,
// matching MongoDB's default (spec 2061 doc 16 §8.2).
const defaultIterationCount = 15000

// scramCredential is the stored form of a password: the salt, the iteration count, and
// the two derived keys. H(ClientKey) and ServerKey are stored, not the password or the
// salted password, so a credential-store breach does not reveal the password (RFC 5802
// §5.4).
type scramCredential struct {
	iterationCount int
	salt           []byte
	storedKey      []byte
	serverKey      []byte
}

// deriveCredential computes the stored credential from a password, salt, and iteration
// count. It is the derivation both newCredential (at user creation) and the test vector
// use.
func deriveCredential(password string, salt []byte, iter int) scramCredential {
	salted, _ := pbkdf2.Key(sha256.New, password, salt, iter, sha256.Size)
	clientKey := hmacSHA256(salted, []byte("Client Key"))
	storedKey := sha256Sum(clientKey)
	serverKey := hmacSHA256(salted, []byte("Server Key"))
	return scramCredential{
		iterationCount: iter,
		salt:           salt,
		storedKey:      storedKey,
		serverKey:      serverKey,
	}
}

// newCredential builds a credential for a new user with a random 16-byte salt and the
// default iteration count.
func newCredential(password string) (scramCredential, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return scramCredential{}, err
	}
	return deriveCredential(password, salt, defaultIterationCount), nil
}

// scramServer drives the server side of one SCRAM conversation. It is created from the
// client-first message and the looked-up credential, emits the server-first challenge,
// and verifies the client-final proof.
type scramServer struct {
	username        string
	clientFirstBare string
	serverFirst     string
	combinedNonce   string
	cred            scramCredential
}

// startScram parses a client-first message, generates the server nonce, and returns the
// server with its server-first challenge bytes. The credential is supplied by the caller
// after looking the user up; an unknown user is still run through the motions with a
// decoy credential by the caller to avoid leaking which usernames exist.
func startScram(clientFirst []byte, cred scramCredential, username, clientNonce, clientFirstBare string) (*scramServer, []byte, error) {
	serverNonce, err := randomNonce()
	if err != nil {
		return nil, nil, err
	}
	combined := clientNonce + serverNonce
	serverFirst := fmt.Sprintf("r=%s,s=%s,i=%d", combined,
		base64.StdEncoding.EncodeToString(cred.salt), cred.iterationCount)

	s := &scramServer{
		username:        username,
		clientFirstBare: clientFirstBare,
		serverFirst:     serverFirst,
		combinedNonce:   combined,
		cred:            cred,
	}
	return s, []byte(serverFirst), nil
}

// finish verifies the client-final proof and, on success, returns the server-final
// message carrying the server signature for mutual authentication. A bad proof or a
// mismatched nonce returns ok=false.
func (s *scramServer) finish(clientFinal []byte) (serverFinal []byte, ok bool) {
	fields := parseScramFields(string(clientFinal))
	if fields["r"] != s.combinedNonce {
		return nil, false
	}
	proof, err := base64.StdEncoding.DecodeString(fields["p"])
	if err != nil || len(proof) != sha256.Size {
		return nil, false
	}
	clientFinalNoProof := "c=" + fields["c"] + ",r=" + fields["r"]
	authMessage := s.clientFirstBare + "," + s.serverFirst + "," + clientFinalNoProof

	clientSig := hmacSHA256(s.cred.storedKey, []byte(authMessage))
	clientKey := xorBytes(proof, clientSig)
	if subtle.ConstantTimeCompare(sha256Sum(clientKey), s.cred.storedKey) != 1 {
		return nil, false
	}
	serverSig := hmacSHA256(s.cred.serverKey, []byte(authMessage))
	return []byte("v=" + base64.StdEncoding.EncodeToString(serverSig)), true
}

// parseClientFirst extracts the username, client nonce, and client-first-bare (everything
// after the gs2 header) from a client-first message of the form "n,,n=user,r=nonce".
func parseClientFirst(payload []byte) (username, clientNonce, bare string, err error) {
	s := string(payload)
	// The gs2 header is the first two comma-separated fields (cbind flag, authzid). The
	// bare message is everything after them.
	first := strings.IndexByte(s, ',')
	if first < 0 {
		return "", "", "", fmt.Errorf("%w: malformed client-first", errProtocol)
	}
	second := strings.IndexByte(s[first+1:], ',')
	if second < 0 {
		return "", "", "", fmt.Errorf("%w: malformed client-first gs2 header", errProtocol)
	}
	bare = s[first+1+second+1:]
	fields := parseScramFields(bare)
	username = decodeSaslName(fields["n"])
	clientNonce = fields["r"]
	if username == "" || clientNonce == "" {
		return "", "", "", fmt.Errorf("%w: client-first missing username or nonce", errProtocol)
	}
	return username, clientNonce, bare, nil
}

// parseScramFields splits a SCRAM message into its comma-separated key=value attributes.
// A value may itself contain '=' (a base64 proof), so only the first '=' separates.
func parseScramFields(s string) map[string]string {
	out := make(map[string]string)
	for _, part := range strings.Split(s, ",") {
		if eq := strings.IndexByte(part, '='); eq > 0 {
			out[part[:eq]] = part[eq+1:]
		}
	}
	return out
}

// decodeSaslName reverses the SCRAM username escaping: =2C is a comma, =3D an equals.
func decodeSaslName(name string) string {
	name = strings.ReplaceAll(name, "=3D", "=")
	name = strings.ReplaceAll(name, "=2C", ",")
	return name
}

// randomNonce returns a base64 server nonce over 24 random bytes.
func randomNonce() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

func hmacSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

func sha256Sum(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

func xorBytes(a, b []byte) []byte {
	out := make([]byte, len(a))
	for i := range a {
		out[i] = a[i] ^ b[i]
	}
	return out
}

// parseIterationCount is a small helper for reading the stored iteration count when it
// arrives as a string.
func parseIterationCount(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return defaultIterationCount
	}
	return n
}
