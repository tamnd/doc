package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tamnd/doc"
	"github.com/tamnd/doc/bson"
)

// app is the running CLI: an open database, the active database name, the output
// renderer, and whatever explicit transaction is in flight. Both the interactive shell
// and the non-interactive paths drive the same app, so a command behaves identically
// whether typed at the prompt or passed with --eval.
type app struct {
	cfg    *config
	db     *doc.DB
	dbName string
	memory bool

	out  io.Writer
	rend *renderer

	sess   doc.Session
	txnCtx context.Context

	// cursor holds an unfinished result set so `it` can advance it (spec §4.4).
	cursor     *doc.Cursor
	batchSize  int
	pending    bson.Raw // a document looked ahead past the last batch, shown next
	pendingSet bool

	interactive bool
	timing      bool
}

// newApp opens the database named by the config and returns a ready app. The caller
// closes it with app.close.
func newApp(cfg *config, out io.Writer) (*app, error) {
	a := &app{
		cfg:       cfg,
		dbName:    cfg.db,
		out:       out,
		batchSize: 20,
	}
	a.rend = newRenderer(out, cfg)
	if err := a.openFile(cfg.file); err != nil {
		return nil, err
	}
	return a, nil
}

// openFile opens path into the app, replacing any database already open. An empty path
// or :memory: opens an in-memory database.
func (a *app) openFile(path string) error {
	if a.db != nil {
		_ = a.db.Close()
		a.db = nil
	}
	a.memory = path == "" || path == ":memory:"
	opts := []doc.Option{
		doc.WithCacheSize(a.cfg.cacheBytes),
		doc.WithSyncLevel(a.cfg.sync),
	}
	if a.cfg.readonly {
		opts = append(opts, doc.WithReadOnly(true))
	}
	if a.cfg.encrypted() && !a.memory {
		opt, err := a.cfg.encryptionOption()
		if err != nil {
			return openError(err.Error())
		}
		opts = append(opts, opt)
	}
	openPath := path
	if a.memory {
		openPath = ":memory:"
	}
	db, err := doc.Open(openPath, opts...)
	if err != nil {
		return openError("cannot open " + displayPath(path) + ": " + err.Error())
	}
	a.db = db
	a.cfg.file = path
	if err := a.applyPragmas(); err != nil {
		_ = a.db.Close()
		a.db = nil
		return err
	}
	return nil
}

// applyPragmas applies every --pragma k=v flag collected at startup to the open
// database (spec 2061 doc 19 §20). A malformed or rejected setting fails the open so
// a typo in a startup flag surfaces immediately rather than being ignored.
func (a *app) applyPragmas() error {
	for _, p := range a.cfg.pragmas {
		name, value, ok := strings.Cut(p, "=")
		if !ok {
			return openError("invalid --pragma " + p + " (want name=value)")
		}
		if _, err := a.db.Pragma(strings.TrimSpace(name), strings.TrimSpace(value)); err != nil {
			return openError(err.Error())
		}
	}
	return nil
}

func (a *app) close() {
	if a.cursor != nil {
		_ = a.cursor.Close(context.Background())
		a.cursor = nil
	}
	if a.sess != nil {
		a.sess.EndSession(context.Background())
		a.sess = nil
	}
	if a.db != nil {
		_ = a.db.Close()
		a.db = nil
	}
}

// ctx returns the context a command runs under: the transaction context when an
// explicit transaction is open, otherwise a fresh background context.
func (a *app) ctx() context.Context {
	if a.txnCtx != nil {
		return a.txnCtx
	}
	return context.Background()
}

func (a *app) collection(name string) *doc.Collection {
	return a.db.Database(a.dbName).Collection(name)
}

// displayPath renders a file path for messages, naming the in-memory case explicitly.
func displayPath(path string) string {
	if path == "" || path == ":memory:" {
		return "[memory]"
	}
	return path
}

// printError writes an error line to stderr (or the output writer in a script), in red
// when color is on. It never returns the error; the caller decides the exit code.
func (a *app) printError(err error) {
	msg := "Error: " + err.Error()
	if f, ok := a.out.(*os.File); ok && f == os.Stdout {
		fmt.Fprintln(os.Stderr, colorError(msg, a.cfg.color))
		return
	}
	fmt.Fprintln(os.Stderr, colorError(msg, a.cfg.color))
}

// ackBuilder starts an acknowledgement document with the conventional first field.
func ackBuilder() *bson.Builder {
	return bson.NewBuilder().AppendBoolean("acknowledged", true)
}

// emptyDoc is the match-everything filter, reused wherever a command needs an empty
// document argument.
func emptyDoc() bson.Raw {
	return bson.NewBuilder().Build()
}

// appendAny encodes an arbitrary Go value (an _id of unknown concrete type) into the
// builder under key, using the public value marshaler so every BSON id type is handled.
func appendAny(b *bson.Builder, key string, val any) {
	t, data, err := doc.MarshalValue(val)
	if err != nil {
		b.AppendNull(key)
		return
	}
	b.AppendValue(key, bson.RawValue{Type: t, Data: data})
}
