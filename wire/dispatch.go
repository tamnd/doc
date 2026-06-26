package wire

import (
	"context"
	"strings"
	"time"

	"github.com/tamnd/doc/bson"
)

// maxBsonObjectSize and maxMessageSizeBytes are advertised in the hello response so a
// driver sizes its batches to match (spec 2061 doc 16 §5.2).
const (
	maxBsonObjectSize   = 16 * 1024 * 1024
	maxMessageSizeBytes = 48*1024*1024 + 1024
	maxWriteBatchSize   = 100000
	maxWireVersion      = 21 // MongoDB 8.0
)

// dispatch routes one OP_MSG command to its handler and returns the reply body. The
// handshake and a handful of server-side commands are answered here; everything else
// flows through the library's RunCommand surface, which already implements the
// diagnostic, configuration, and DDL command set (spec 2061 doc 16 §4.2).
func (c *conn) dispatch(ctx context.Context, in *opMsgIn) bson.Raw {
	name := strings.ToLower(firstKey(in.body))
	if name == "" {
		return errorDoc(9, "FailedToParse", "empty command document")
	}

	// Log a command that runs past the slow-op threshold, regardless of which return path
	// it takes (spec 2061 doc 16 §2.2).
	if c.srv.opts.SlowOpThreshold > 0 {
		start := time.Now()
		defer func() {
			if d := time.Since(start); d >= c.srv.opts.SlowOpThreshold {
				c.srv.opts.Logger.Info("slow command",
					"connectionId", c.id, "command", name, "durationMs", d.Milliseconds())
			}
		}()
	}

	dbName := lookupString(in.body, "$db")
	if dbName == "" {
		dbName = "admin"
	}

	switch name {
	case "hello", "ismaster":
		return c.buildHello(in.body)
	case "ping":
		return okReply()
	case "saslstart":
		if !c.srv.authConfigured() {
			return errorDoc(59, "CommandNotFound", "authentication is not configured on this server")
		}
		return c.handleSaslStart(ctx, dbName, in.body)
	case "saslcontinue":
		if !c.srv.authConfigured() {
			return errorDoc(59, "CommandNotFound", "authentication is not configured on this server")
		}
		return c.handleSaslContinue(ctx, in.body)
	case "logout":
		return c.handleLogout()
	}

	ctx, cancel := c.commandContext(ctx, in.body)
	defer cancel()

	// Every remaining command passes the authorization gate before it is routed (spec
	// 2061 doc 16 §19).
	if deny := c.authorize(ctx, dbName, name); deny != nil {
		return deny
	}

	// Session and transaction control commands run before the data path; they manage the
	// session table rather than a collection (spec 2061 doc 16 §10).
	if reply, handled := c.dispatchSession(ctx, name, in.body); handled {
		return reply
	}

	// A command that carries lsid/txnNumber runs on its session's transaction; one that
	// carries only an lsid, or none, runs auto-commit. Binding happens here so the data
	// path and RunCommand both see the transaction's snapshot (spec 2061 doc 16 §10.2).
	sctx, deny := c.txnContext(ctx, in.body)
	if deny != nil {
		return deny
	}
	ctx = sctx

	if reply, handled := c.dispatchUsers(ctx, dbName, name, in.body); handled {
		return reply
	}

	if reply, handled := c.dispatchData(ctx, dbName, name, in); handled {
		return reply
	}

	// Route the remaining commands through the in-process command surface.
	res := c.srv.db.Database(dbName).RunCommand(ctx, in.body)
	raw, err := res.Raw()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return errorDoc(50, "MaxTimeMSExpired", "operation exceeded time limit")
		}
		return errorReplyFrom(err)
	}
	return raw
}

// commandContext derives the per-command context, honoring a maxTimeMS deadline if the
// command carries one (spec 2061 doc 16 §4.4).
func (c *conn) commandContext(ctx context.Context, body bson.Raw) (context.Context, context.CancelFunc) {
	if v, ok := body.Lookup("maxTimeMS"); ok {
		if ms, ok := v.AsFloat64(); ok && ms > 0 {
			return context.WithTimeout(ctx, time.Duration(ms)*time.Millisecond)
		}
	}
	return context.WithCancel(ctx)
}

// buildHello answers the handshake (spec 2061 doc 16 §5.2). The response classifies the
// server as a standalone node so drivers route every read and write to it.
func (c *conn) buildHello(req bson.Raw) bson.Raw {
	c.recordClientMeta(req)

	topo := bson.NewBuilder().
		AppendObjectID("processId", c.srv.processID).
		AppendInt64("counter", 0).
		Build()

	b := bson.NewBuilder().
		AppendBoolean("ismaster", true).
		AppendBoolean("isWritablePrimary", true).
		AppendBoolean("helloOk", true).
		AppendDocument("topologyVersion", topo).
		AppendInt32("maxBsonObjectSize", maxBsonObjectSize).
		AppendInt32("maxMessageSizeBytes", maxMessageSizeBytes).
		AppendInt32("maxWriteBatchSize", maxWriteBatchSize).
		AppendDateTime("localTime", time.Now().UnixMilli()).
		AppendInt32("logicalSessionTimeoutMinutes", 30).
		AppendInt32("connectionId", c.id).
		AppendInt32("minWireVersion", 0).
		AppendInt32("maxWireVersion", maxWireVersion).
		AppendBoolean("readOnly", c.srv.opts.ReadOnly).
		AppendArray("compression", c.negotiateCompression(req))

	c.appendAuthMechs(b)
	c.appendSpeculative(b, req)
	b.AppendDouble("ok", 1)
	return b.Build()
}

// negotiateCompression reads the client's offered compressor names from hello, picks the
// one doc supports, and returns the array to advertise back. The chosen compressor is
// staged as pending so it activates only after this hello reply is written (spec 2061
// doc 16 §11.2). Re-running hello on the same connection does not downgrade an already
// active compressor.
func (c *conn) negotiateCompression(req bson.Raw) bson.Raw {
	var offered []string
	if v, ok := req.Lookup("compression"); ok && v.Type == bson.TypeArray {
		for _, e := range arrayElements(v) {
			if e.Type == bson.TypeString {
				offered = append(offered, e.StringValue())
			}
		}
	}
	id, names := negotiateCompressor(offered)
	if id != compressorNoop && c.compressor == compressorNoop {
		c.pendingCompressor = id
	}
	vals := make([]bson.RawValue, len(names))
	for i, n := range names {
		vals[i] = bson.RawValue{Type: bson.TypeString, Data: encodeString(n)}
	}
	return bson.BuildArray(vals...)
}

// recordClientMeta captures the driver-supplied client document once, for logging.
func (c *conn) recordClientMeta(req bson.Raw) {
	if c.clientMeta != "" {
		return
	}
	if v, ok := req.Lookup("client"); ok && v.Type == bson.TypeDocument {
		c.clientMeta = describeClient(v.Document())
		c.srv.opts.Logger.Info("wire client connected",
			"connectionId", c.id, "client", c.clientMeta)
	}
}

// describeClient renders driver and application names from the hello client document
// for a one-line log entry.
func describeClient(client bson.Raw) string {
	var parts []string
	if v, ok := client.Lookup("driver"); ok && v.Type == bson.TypeDocument {
		d := v.Document()
		name := lookupString(d, "name")
		version := lookupString(d, "version")
		if name != "" {
			parts = append(parts, name+"/"+version)
		}
	}
	if v, ok := client.Lookup("application"); ok && v.Type == bson.TypeDocument {
		if app := lookupString(v.Document(), "name"); app != "" {
			parts = append(parts, "app="+app)
		}
	}
	return strings.Join(parts, " ")
}

// lookupString reads a string field, returning "" when absent or not a string.
func lookupString(d bson.Raw, key string) string {
	if v, ok := d.Lookup(key); ok && v.Type == bson.TypeString {
		return v.StringValue()
	}
	return ""
}

// okReply is the canonical {ok: 1} success document.
func okReply() bson.Raw {
	return bson.NewBuilder().AppendDouble("ok", 1).Build()
}
