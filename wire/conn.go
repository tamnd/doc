package wire

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"

	"github.com/tamnd/doc/bson"
)

// conn is one client connection. It owns the read and write side of a single TCP
// stream and runs a serial request loop: the single-writer engine behind it means
// there is no benefit to pipelining within one connection.
type conn struct {
	srv *conn0Server
	nc  net.Conn
	id  int32

	// clientMeta is the driver-supplied client document from hello, logged once.
	clientMeta string

	// compressor is the active outbound compressor id (compressorNoop when none was
	// negotiated). pendingCompressor holds the negotiated id until the hello reply that
	// negotiated it has been written, so that reply itself goes out uncompressed.
	compressor        byte
	pendingCompressor byte

	// auth is the authenticated identity, nil until a SCRAM conversation succeeds. scram
	// holds the in-flight conversation and scramIdentity the identity it will adopt on a
	// valid proof; scramConvID is the conversation id echoed back to the driver. convSeq
	// numbers conversations on this connection.
	auth          *identity
	scram         *scramServer
	scramIdentity *identity
	scramConvID   int32
	convSeq       int32

	// sessions holds the logical sessions opened on this connection, keyed by lsid. The
	// serial request loop is the only writer, so the map needs no lock (spec 2061 doc 16
	// §10.1, §15.2).
	sessions map[string]*wireSession
}

// conn0Server is the slice of Server a conn needs. It is an alias so server.go and
// conn.go can evolve independently; Server satisfies it directly.
type conn0Server = Server

// serve runs the request loop until the peer closes, the context is canceled, or a
// framing error makes the stream unrecoverable.
func (c *conn) serve(ctx context.Context) {
	defer func() { _ = c.nc.Close() }()
	defer c.endAllSessions(context.Background())
	br := bufio.NewReaderSize(c.nc, 32*1024)
	bw := bufio.NewWriterSize(c.nc, 32*1024)

	c.srv.opts.Logger.Debug("wire connection opened", "connectionId", c.id, "remote", c.nc.RemoteAddr().String())
	defer c.srv.opts.Logger.Debug("wire connection closed", "connectionId", c.id)

	for {
		if ctx.Err() != nil {
			return
		}
		msg, err := readMessage(br, c.srv.opts.MaxMessageBytes)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && !isClosedConnErr(err) {
				c.srv.opts.Logger.Debug("wire read ended", "connectionId", c.id, "err", err)
			}
			return
		}
		reply, keepGoing := c.handle(ctx, msg)
		if reply != nil {
			if c.compressor != compressorNoop && len(reply) > compressThreshold {
				reply = wrapCompressed(reply, c.compressor)
			}
			if _, err := bw.Write(reply); err != nil {
				return
			}
			if err := bw.Flush(); err != nil {
				return
			}
		}
		// A hello that negotiated compression activates it only now, after its own reply
		// has gone out uncompressed.
		if c.pendingCompressor != compressorNoop {
			c.compressor = c.pendingCompressor
			c.pendingCompressor = compressorNoop
		}
		if !keepGoing {
			return
		}
	}
}

// handle dispatches one framed message and returns the bytes to write back (nil for a
// fire-and-forget) and whether to keep serving the connection.
func (c *conn) handle(ctx context.Context, msg *rawMessage) (reply []byte, keepGoing bool) {
	switch msg.header.OpCode {
	case opMsg:
		in, err := parseOpMsg(msg.payload)
		if err != nil {
			return c.protocolErrorReply(msg.header.RequestID, err), false
		}
		body := c.dispatch(ctx, in)
		if in.flags&flagMoreToCome != 0 {
			// Fire-and-forget write: the client does not expect a reply.
			return nil, true
		}
		return encodeOpMsgReply(c.srv.nextRequestID(), msg.header.RequestID, body, in.checksumRequested()), true
	case opQuery:
		return c.handleLegacyQuery(msg)
	case opCompressed:
		// Unwrap the compressed envelope and dispatch the inner message. The reply is
		// compressed back on the way out by the serve loop when a compressor is active.
		origOp, inner, err := parseOpCompressed(msg.payload, c.srv.opts.MaxMessageBytes)
		if err != nil {
			return c.protocolErrorReply(msg.header.RequestID, err), false
		}
		innerMsg := &rawMessage{
			header:  header{RequestID: msg.header.RequestID, ResponseTo: msg.header.ResponseTo, OpCode: origOp},
			payload: inner,
		}
		return c.handle(ctx, innerMsg)
	default:
		// Legacy opcodes (OP_INSERT, OP_UPDATE, OP_GET_MORE, ...) are not supported.
		return c.protocolErrorReply(msg.header.RequestID, errors.New("unexpected legacy opcode")), false
	}
}

// handleLegacyQuery answers a legacy OP_QUERY, which the server accepts only for the
// hello/isMaster handshake on admin.$cmd.
func (c *conn) handleLegacyQuery(msg *rawMessage) (reply []byte, keepGoing bool) {
	coll, query, err := parseOpQuery(msg.payload)
	if err != nil {
		return c.protocolErrorReply(msg.header.RequestID, err), false
	}
	name := firstKey(query)
	if !strings.HasSuffix(coll, "$cmd") || !isHandshake(name) {
		// The server declines OP_QUERY for anything but the handshake.
		doc := errorDoc(17, "UnsupportedOpQueryNonCmd", "OP_QUERY is only supported for the handshake")
		return encodeOpReply(c.srv.nextRequestID(), msg.header.RequestID, doc), false
	}
	resp := c.buildHello(query)
	return encodeOpReply(c.srv.nextRequestID(), msg.header.RequestID, resp), true
}

// isLoopback reports whether the peer connected over a loopback address, which the
// localhost exception keys off (spec 2061 doc 16 §8.6).
func (c *conn) isLoopback() bool {
	host, _, err := net.SplitHostPort(c.nc.RemoteAddr().String())
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// tlsState returns the completed TLS handshake state when the connection runs over TLS.
// X.509 authentication reads the verified peer certificate from it. By the time a SASL
// command arrives the handshake has already run, so the state is populated.
func (c *conn) tlsState() (tls.ConnectionState, bool) {
	if tc, ok := c.nc.(*tls.Conn); ok {
		return tc.ConnectionState(), true
	}
	return tls.ConnectionState{}, false
}

// nextConvID hands out the next SASL conversation id for this connection.
func (c *conn) nextConvID() int32 {
	c.convSeq++
	return c.convSeq
}

func (c *conn) protocolErrorReply(responseTo int32, err error) []byte {
	doc := errorDoc(17, "ProtocolError", err.Error())
	return encodeOpMsgReply(c.srv.nextRequestID(), responseTo, doc, false)
}

// firstKey returns the first element key of a BSON document, the command name.
func firstKey(d bson.Raw) string {
	elems, err := d.Elements()
	if err != nil || len(elems) == 0 {
		return ""
	}
	return elems[0].Key
}

func isHandshake(name string) bool {
	switch strings.ToLower(name) {
	case "hello", "ismaster":
		return true
	}
	return false
}

// isClosedConnErr reports whether err is the "use of closed network connection" that a
// graceful shutdown produces, which is not worth logging as a failure.
func isClosedConnErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "use of closed network connection")
}
