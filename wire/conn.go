package wire

import (
	"bufio"
	"context"
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
}

// conn0Server is the slice of Server a conn needs. It is an alias so server.go and
// conn.go can evolve independently; Server satisfies it directly.
type conn0Server = Server

// serve runs the request loop until the peer closes, the context is canceled, or a
// framing error makes the stream unrecoverable.
func (c *conn) serve(ctx context.Context) {
	defer func() { _ = c.nc.Close() }()
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
			if _, err := bw.Write(reply); err != nil {
				return
			}
			if err := bw.Flush(); err != nil {
				return
			}
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
		// Compression is negotiated through hello; the server advertises none in M8-a,
		// so a well-behaved driver never sends OP_COMPRESSED. Decline it clearly.
		return c.protocolErrorReply(msg.header.RequestID, errors.New("OP_COMPRESSED not negotiated")), false
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
