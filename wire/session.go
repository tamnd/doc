package wire

import (
	"context"
	"crypto/rand"
	"strconv"

	"github.com/tamnd/doc"
	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/options"
)

// logicalSessionTimeoutMinutes is the idle window a logical session survives without
// use, advertised in hello and echoed by startSession (spec 2061 doc 16 §5.2, §10.1).
const logicalSessionTimeoutMinutes = 30

// binarySubtypeUUID is the BSON binary subtype a logical session id uses (spec 2061
// doc 16 §10.1, BSON binary subtype 0x04).
const binarySubtypeUUID = 0x04

// txnState tracks where a session's single transaction is in its lifecycle. A session
// drives at most one transaction at a time, mirroring the MongoDB ClientSession
// contract (spec 2061 doc 16 §10.3).
type txnState int

const (
	txnNone       txnState = iota // no transaction has been started under txnNum
	txnInProgress                 // a transaction is open and accepting commands
	txnDone                       // the transaction committed or aborted
)

// wireSession is one logical session on a connection: a client lsid bound to a library
// doc.Session plus the bookkeeping for the transaction that session may have open. The
// connection runs a serial request loop, so a session is only ever touched by its own
// connection goroutine and needs no locking (spec 2061 doc 16 §10.1, §15.2).
type wireSession struct {
	sess   doc.Session
	txnNum int64    // the txnNumber the current or last transaction ran under
	state  txnState // lifecycle of the transaction under txnNum
}

// session returns the wireSession for an lsid key, creating it (and its library
// session) on first use. Sessions are created implicitly on first reference, matching
// the driver convention that an explicit startSession is optional (spec 2061 doc 16
// §10.1).
func (c *conn) session(key string) (*wireSession, error) {
	if c.sessions == nil {
		c.sessions = make(map[string]*wireSession)
	}
	if ws, ok := c.sessions[key]; ok {
		return ws, nil
	}
	sess, err := c.srv.db.StartSession()
	if err != nil {
		return nil, err
	}
	ws := &wireSession{sess: sess}
	c.sessions[key] = ws
	return ws, nil
}

// endAllSessions aborts any open transaction and releases every session on the
// connection. It runs when the connection closes so a client that drops mid-transaction
// does not leave a write transaction pinned (spec 2061 doc 16 §7.3, §15.5).
func (c *conn) endAllSessions(ctx context.Context) {
	for k, ws := range c.sessions {
		ws.sess.EndSession(ctx)
		delete(c.sessions, k)
	}
}

// sessionKeyOf reads the lsid sub-document off a command and returns a stable map key.
// The key is the raw bytes of the lsid document, which a driver sends identically on
// every command of the same session, so there is no need to decode the UUID binary
// (spec 2061 doc 16 §10.1).
func sessionKeyOf(body bson.Raw) (string, bool) {
	v, ok := body.Lookup("lsid")
	if !ok || v.Type != bson.TypeDocument {
		return "", false
	}
	d := v.Document()
	if _, ok := d.Lookup("id"); !ok {
		return "", false
	}
	return string(d), true
}

// dispatchSession routes the session and transaction control commands. The data and
// CRUD commands carry their session through the bound context instead and are handled
// by txnContext (spec 2061 doc 16 §4.2, §10).
func (c *conn) dispatchSession(ctx context.Context, name string, body bson.Raw) (bson.Raw, bool) {
	switch name {
	case "startsession":
		return c.handleStartSession(), true
	case "endsessions":
		return c.handleEndSessions(ctx, body), true
	case "committransaction":
		return c.handleCommitTransaction(ctx, body), true
	case "aborttransaction":
		return c.handleAbortTransaction(ctx, body), true
	default:
		return nil, false
	}
}

// handleStartSession answers an explicit startSession by minting a fresh logical
// session id and returning it, the shape a driver stores and replays as lsid on later
// commands (spec 2061 doc 16 §10.1). The session itself is created lazily when the id
// is first used.
func (c *conn) handleStartSession() bson.Raw {
	uuid := newUUID()
	id := bson.NewBuilder().
		AppendBinary("id", binarySubtypeUUID, uuid).
		Build()
	return bson.NewBuilder().
		AppendDocument("id", id).
		AppendInt32("timeoutMinutes", logicalSessionTimeoutMinutes).
		AppendDouble("ok", 1).
		Build()
}

// handleEndSessions releases the named sessions, aborting any transaction still open on
// them. Unknown ids are ignored, matching MongoDB's best-effort semantics (spec 2061
// doc 16 §10.1).
func (c *conn) handleEndSessions(ctx context.Context, body bson.Raw) bson.Raw {
	if v, ok := body.Lookup("endSessions"); ok && v.Type == bson.TypeArray {
		for _, e := range arrayElements(v) {
			if e.Type != bson.TypeDocument {
				continue
			}
			key := string(e.Document())
			if ws, ok := c.sessions[key]; ok {
				ws.sess.EndSession(ctx)
				delete(c.sessions, key)
			}
		}
	}
	return okReply()
}

// handleCommitTransaction commits the transaction the command's (lsid, txnNumber)
// names. A commit on an already-committed transaction is idempotent and returns ok; a
// stale or unknown transaction returns NoSuchTransaction (spec 2061 doc 16 §10.3). A
// write conflict surfaces with the TransientTransactionError label so the driver can
// retry the whole transaction.
func (c *conn) handleCommitTransaction(ctx context.Context, body bson.Raw) bson.Raw {
	ws, deny := c.txnTarget(body)
	if deny != nil {
		return deny
	}
	if ws.state == txnDone {
		// Committing a transaction that already finished is a no-op the driver may
		// issue on retry; acknowledge it.
		return okReply()
	}
	err := ws.sess.CommitTransaction(ctx)
	ws.state = txnDone
	if err != nil {
		return errorReplyFrom(err)
	}
	return okReply()
}

// handleAbortTransaction discards the transaction the command names. Abort is idempotent
// and abort of an already-finished transaction returns ok (spec 2061 doc 16 §10.3).
func (c *conn) handleAbortTransaction(ctx context.Context, body bson.Raw) bson.Raw {
	ws, deny := c.txnTarget(body)
	if deny != nil {
		return deny
	}
	if ws.state == txnDone {
		return okReply()
	}
	err := ws.sess.AbortTransaction(ctx)
	ws.state = txnDone
	if err != nil {
		return errorReplyFrom(err)
	}
	return okReply()
}

// txnTarget resolves the session a commit or abort command refers to and checks the
// txnNumber against the session's current transaction. It returns an error reply when
// the session is unknown or the txnNumber names an older transaction (spec 2061 doc 16
// §10.3).
func (c *conn) txnTarget(body bson.Raw) (*wireSession, bson.Raw) {
	key, ok := sessionKeyOf(body)
	if !ok {
		return nil, errorDoc(50000, "InvalidOptions", "commitTransaction requires an lsid")
	}
	ws := c.sessions[key]
	if ws == nil {
		return nil, noSuchTransaction(body)
	}
	txnNum, _ := lookupInt64(body, "txnNumber")
	if ws.state == txnNone || txnNum != ws.txnNum {
		return nil, noSuchTransaction(body)
	}
	return ws, nil
}

// txnContext binds a data or CRUD command to its logical session and transaction. A
// command that opens a transaction (startTransaction:true) begins one on the session; a
// command that continues a transaction runs on the session's open transaction; a
// command that only carries an lsid runs auto-commit with no binding. It returns an
// error reply when the transaction reference is stale (spec 2061 doc 16 §10.2, §10.3).
func (c *conn) txnContext(ctx context.Context, body bson.Raw) (context.Context, bson.Raw) {
	key, ok := sessionKeyOf(body)
	if !ok {
		return ctx, nil
	}
	txnNum, hasTxn := lookupInt64(body, "txnNumber")
	if !hasTxn {
		// A bare lsid groups commands for the driver but does not open a transaction;
		// the command runs auto-commit.
		return ctx, nil
	}

	ws, err := c.session(key)
	if err != nil {
		return ctx, errorReplyFrom(err)
	}

	if lookupBool(body, "startTransaction") {
		// Opening a new transaction. Any transaction still open under an earlier number
		// is implicitly aborted, as MongoDB does when a higher txnNumber arrives.
		if ws.state == txnInProgress {
			_ = ws.sess.AbortTransaction(ctx)
		}
		if err := ws.sess.StartTransaction(txnOptionsFrom(body)...); err != nil {
			return ctx, errorReplyFrom(err)
		}
		ws.txnNum = txnNum
		ws.state = txnInProgress
		return doc.NewSessionContext(ctx, ws.sess), nil
	}

	// Continuing a transaction: the number must match the open one.
	if ws.state != txnInProgress || txnNum != ws.txnNum {
		return ctx, noSuchTransaction(body)
	}
	return doc.NewSessionContext(ctx, ws.sess), nil
}

// noSuchTransaction builds the NoSuchTransaction (251) reply for a stale txnNumber. It
// carries the TransientTransactionError label so a driver retries the transaction from
// the top rather than failing the operation (spec 2061 doc 16 §10.3, §13.4).
func noSuchTransaction(body bson.Raw) bson.Raw {
	txnNum, _ := lookupInt64(body, "txnNumber")
	msg := "Transaction " + strconv.FormatInt(txnNum, 10) + " has been committed or aborted"
	return errorDocLabeled(251, "NoSuchTransaction", msg, "TransientTransactionError")
}

// txnOptionsFrom reads the read concern carried on the first command of a transaction
// and turns it into the library transaction options. A "snapshot" read concern selects
// the serializable isolation the session pins for the transaction's lifetime; the other
// levels map to the default snapshot isolation, which on a single node already gives a
// stable read view (spec 2061 doc 16 §10.2, §12.3).
func txnOptionsFrom(body bson.Raw) []*options.TransactionOptions {
	rc := readConcernLevel(body)
	if rc == "" {
		return nil
	}
	return []*options.TransactionOptions{
		options.Transaction().SetReadConcern(rc),
	}
}

// readConcernLevel reads the readConcern.level string off a command, or "" when absent.
func readConcernLevel(body bson.Raw) string {
	v, ok := body.Lookup("readConcern")
	if !ok || v.Type != bson.TypeDocument {
		return ""
	}
	return lookupString(v.Document(), "level")
}

// newUUID returns 16 random bytes for a version-4 logical session id. The variant and
// version bits are set per RFC 4122 so a driver that inspects them is satisfied.
func newUUID() []byte {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return b[:]
}
