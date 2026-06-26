package wire

import (
	"context"
	"crypto/x509"

	"github.com/tamnd/doc/bson"
)

// MONGODB-X509 authenticates a connection as the subject of its verified TLS client
// certificate (spec 2061 doc 16 §8.5). There is no password exchange: the TLS handshake
// already proved the client holds the private key for a certificate the configured CA
// signed, so authentication reduces to reading the certificate subject and matching it to a
// user record in $external.system.users. The mechanism is a single SASL step, so saslStart
// returns done immediately.

// x509MechName is the SASL mechanism name and the credential family for external,
// certificate-authenticated users.
const x509MechName = "MONGODB-X509"

// externalDB is the database external (non-password) users live under, the convention
// MongoDB uses for X.509 and other external mechanisms.
const externalDB = "$external"

// x509Available reports whether the server can offer MONGODB-X509: auth is on and the
// listener verifies client certificates against a CA. Without a CA a presented certificate
// is never checked, so trusting its subject would be meaningless.
func (c *conn) x509Available() bool {
	return c.srv.authConfigured() && c.srv.opts.TLS.CAFile != ""
}

// handleX509 runs the one-step MONGODB-X509 exchange. It requires a verified peer
// certificate from the TLS handshake, derives the subject name, optionally checks it against
// the username the client supplied, looks the user up in $external.system.users, and on a
// match adopts that user's roles.
func (c *conn) handleX509(ctx context.Context, body bson.Raw) bson.Raw {
	subject, ok := c.verifiedClientSubject()
	if !ok {
		return errorDoc(18, "AuthenticationFailed",
			"MONGODB-X509 requires a verified client certificate")
	}
	// The payload, when present, names the subject the client expects to authenticate as.
	// It must match the certificate, so a client cannot ask to be someone else.
	if requested := string(binaryPayload(body, "payload")); requested != "" && requested != subject {
		return errorDoc(18, "AuthenticationFailed",
			"certificate subject does not match the requested user")
	}
	roles, found := c.lookupExternalUser(ctx, subject)
	if !found {
		return errorDoc(11, "UserNotFound",
			"could not find user \""+subject+"\" for db \""+externalDB+"\"")
	}
	c.auth = &identity{user: subject, db: externalDB, roles: roles}
	return saslReply(c.nextConvID(), true, nil)
}

// verifiedClientSubject returns the RFC 2253 subject name of the verified peer certificate.
// VerifiedChains is non-empty only when crypto/tls validated the certificate against the
// configured CA, so reading the leaf from it never trusts an unverified certificate.
func (c *conn) verifiedClientSubject() (string, bool) {
	state, ok := c.tlsState()
	if !ok || len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) == 0 {
		return "", false
	}
	leaf := state.VerifiedChains[0][0]
	return certificateSubject(leaf), true
}

// certificateSubject renders a certificate subject the way MongoDB keys an external user:
// the RFC 2253 string form of the distinguished name.
func certificateSubject(cert *x509.Certificate) string {
	return cert.Subject.String()
}

// lookupExternalUser loads the role grants for an X.509 user stored under
// $external.system.users, keyed by the certificate subject. found is false when no such
// user exists, which the caller reports as UserNotFound.
func (c *conn) lookupExternalUser(ctx context.Context, subject string) (roles []roleRef, found bool) {
	filter := bson.NewBuilder().AppendString("_id", userID(externalDB, subject)).Build()
	raw, err := c.usersCollection().FindOne(ctx, filter).Raw()
	if err != nil {
		return nil, false
	}
	if v, ok := raw.Lookup("roles"); ok {
		roles = parseRoles(v, externalDB)
	}
	return roles, true
}
