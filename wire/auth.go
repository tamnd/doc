package wire

import (
	"context"

	"github.com/tamnd/doc/bson"
)

// auth.go owns the authentication conversation: advertising the supported mechanisms,
// running the SCRAM-SHA-256 exchange that produces an identity, and clearing it on logout
// (spec 2061 doc 16 §8). The crypto and message grammar live in scram.go; the role checks
// an identity is put through live in authz.go.

// authConfigured reports whether the server requires authentication. It is the single
// switch the whole authorization path keys off (spec 2061 doc 16 §19.1).
func (s *Server) authConfigured() bool { return s.opts.AuthRequired }

// appendAuthMechs advertises the supported SASL mechanisms in the hello response, but only
// when auth is configured. A driver reads this to pick a mechanism before it sends
// credentials. X.509 is advertised alongside SCRAM once a TLS client CA is configured, so a
// driver connecting with a client certificate knows it may authenticate without a password.
func (c *conn) appendAuthMechs(b *bson.Builder) {
	if !c.srv.authConfigured() {
		return
	}
	mechVals := []bson.RawValue{{Type: bson.TypeString, Data: encodeString(scramMechName)}}
	if c.x509Available() {
		mechVals = append(mechVals, bson.RawValue{Type: bson.TypeString, Data: encodeString(x509MechName)})
	}
	b.AppendArray("saslSupportedMechs", bson.BuildArray(mechVals...))
}

// appendSpeculative answers an embedded speculativeAuthenticate in hello: the driver folds
// its SCRAM client-first into the handshake to save a round trip, and the server folds the
// server-first back into the hello reply (spec 2061 doc 16 §8.5). A later saslContinue
// finishes the same conversation.
func (c *conn) appendSpeculative(b *bson.Builder, req bson.Raw) {
	if !c.srv.authConfigured() {
		return
	}
	v, ok := req.Lookup("speculativeAuthenticate")
	if !ok || v.Type != bson.TypeDocument {
		return
	}
	d := v.Document()
	if lookupString(d, "mechanism") != scramMechName {
		return
	}
	db := lookupString(d, "db")
	if db == "" {
		db = "admin"
	}
	reply, ok := c.scramStep1(context.Background(), db, binaryPayload(d, "payload"))
	if !ok {
		return
	}
	b.AppendDocument("speculativeAuthenticate", reply)
}

// handleSaslStart begins a SASL conversation. SCRAM-SHA-256 runs its challenge exchange;
// MONGODB-X509 finishes in this single step off the TLS certificate. Any other mechanism is
// rejected before the credential store is touched.
func (c *conn) handleSaslStart(ctx context.Context, db string, body bson.Raw) bson.Raw {
	switch lookupString(body, "mechanism") {
	case scramMechName:
		reply, _ := c.scramStep1(ctx, db, binaryPayload(body, "payload"))
		return reply
	case x509MechName:
		return c.handleX509(ctx, body)
	default:
		return errorDoc(2, "BadValue",
			"unsupported authentication mechanism "+lookupString(body, "mechanism"))
	}
}

// scramStep1 parses the client-first message, looks up the user, and emits the server-first
// challenge. An unknown user is run against a decoy credential so the reply shape and
// timing do not reveal whether the user exists; the conversation then fails at the proof
// step like any wrong password (spec 2061 doc 16 §8.4).
func (c *conn) scramStep1(ctx context.Context, db string, payload []byte) (bson.Raw, bool) {
	user, clientNonce, bare, err := parseClientFirst(payload)
	if err != nil {
		return errorDoc(17, "ProtocolError", err.Error()), false
	}
	cred, roles, found := c.lookupCredential(ctx, db, user)
	if !found {
		cred = decoyCredential()
		roles = nil
	}
	srv, serverFirst, err := startScram(payload, cred, user, clientNonce, bare)
	if err != nil {
		return errorDoc(1, "InternalError", err.Error()), false
	}
	c.scram = srv
	if found {
		c.scramIdentity = &identity{user: user, db: db, roles: roles}
	} else {
		c.scramIdentity = nil
	}
	c.scramConvID = c.nextConvID()
	return saslReply(c.scramConvID, false, serverFirst), true
}

// handleSaslContinue verifies the client-final proof. On success the connection adopts the
// pending identity and the server-final signature goes back for mutual authentication; on
// failure the conversation is dropped and the connection stays anonymous.
func (c *conn) handleSaslContinue(_ context.Context, body bson.Raw) bson.Raw {
	if c.scram == nil {
		return errorDoc(17, "ProtocolError", "no SASL conversation in progress")
	}
	serverFinal, ok := c.scram.finish(binaryPayload(body, "payload"))
	convID := c.scramConvID
	ident := c.scramIdentity
	c.scram = nil
	c.scramIdentity = nil
	if !ok || ident == nil {
		return errorDoc(18, "AuthenticationFailed", "Authentication failed.")
	}
	c.auth = ident
	return saslReply(convID, true, serverFinal)
}

// handleLogout clears the connection's identity and any in-flight conversation. The
// connection reverts to anonymous, subject to the auth gate again (spec 2061 doc 16 §19.1).
func (c *conn) handleLogout() bson.Raw {
	c.auth = nil
	c.scram = nil
	c.scramIdentity = nil
	return okReply()
}

// saslReply frames a saslStart/saslContinue response: the conversation id, the done flag,
// and the binary SASL payload.
func saslReply(convID int32, done bool, payload []byte) bson.Raw {
	return bson.NewBuilder().
		AppendInt32("conversationId", convID).
		AppendBoolean("done", done).
		AppendBinary("payload", 0, payload).
		AppendDouble("ok", 1).
		Build()
}

// binaryPayload reads a SASL payload field, which a driver sends as BSON binary but some
// send as a string.
func binaryPayload(d bson.Raw, key string) []byte {
	v, ok := d.Lookup(key)
	if !ok {
		return nil
	}
	if _, data, ok := v.Binary(); ok {
		return data
	}
	if v.Type == bson.TypeString {
		return []byte(v.StringValue())
	}
	return nil
}

// decoyCredential is a throwaway credential used when the named user does not exist, so the
// server-first reply looks identical to a real one and the proof step fails like a wrong
// password rather than disclosing that the user is unknown.
func decoyCredential() scramCredential {
	cred, err := newCredential("\x00not-a-real-password")
	if err != nil {
		return scramCredential{iterationCount: defaultIterationCount, salt: make([]byte, 16)}
	}
	return cred
}

// encodeString renders a Go string as a BSON string value payload (int32 length prefix,
// UTF-8 bytes, null terminator), used when building array elements by hand.
func encodeString(s string) []byte {
	n := len(s) + 1
	out := make([]byte, 4+n)
	out[0] = byte(n)
	out[1] = byte(n >> 8)
	out[2] = byte(n >> 16)
	out[3] = byte(n >> 24)
	copy(out[4:], s)
	return out
}
