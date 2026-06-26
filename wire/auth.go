package wire

import "github.com/tamnd/doc/bson"

// authConfigured reports whether the server requires authentication. M8-a never
// configures auth, so this is always false; M8-c wires it to the credential store and
// grows the per-connection auth conversation state.
func (s *Server) authConfigured() bool { return false }

// appendAuthMechs advertises the supported SASL mechanisms in the hello response, but
// only when auth is configured. With no auth a driver connecting without credentials
// proceeds straight to commands, which is the M8-a behavior.
func (c *conn) appendAuthMechs(b *bson.Builder) {
	if !c.srv.authConfigured() {
		return
	}
	mechs := bson.BuildArray(bson.RawValue{Type: bson.TypeString, Data: encodeString("SCRAM-SHA-256")})
	b.AppendArray("saslSupportedMechs", mechs)
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
