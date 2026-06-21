package main

import (
	"context"

	"github.com/tamnd/doc"
	"github.com/tamnd/doc/bson"
)

// streamCursor renders a result cursor. In the interactive shell with the default JSON
// display it shows one batch (batchSize documents) and parks the cursor so `it`
// advances it; in every other case (an explicit format flag, a script, a pipe) it
// streams the whole cursor to completion (spec 2061 doc 15 §4.4).
func (a *app) streamCursor(cur *doc.Cursor) error {
	if a.batched() {
		a.cursor = cur
		return a.advanceCursor()
	}
	defer func() { _ = cur.Close(context.Background()) }()
	docs := make([]bson.Raw, 0, 64)
	for cur.Next(a.ctx()) {
		docs = append(docs, cloneRaw(cur.Current()))
	}
	if err := cur.Err(); err != nil {
		return classify(err)
	}
	return a.rend.renderDocs(docs)
}

// advanceCursor prints the next batch from the parked cursor and, if more documents
// remain, prints the "Type it for more" hint. It is what both the first render and the
// `it` alias call.
func (a *app) advanceCursor() error {
	if a.cursor == nil {
		return a.rend.writeText("no cursor")
	}
	docs := make([]bson.Raw, 0, a.batchSize)
	// A document held back from the previous batch's look-ahead leads this batch.
	if a.pendingSet {
		docs = append(docs, a.pending)
		a.pendingSet = false
	}
	for len(docs) < a.batchSize {
		if !a.cursor.Next(a.ctx()) {
			break
		}
		docs = append(docs, cloneRaw(a.cursor.Current()))
	}
	// Look one past the batch to learn whether to print the "more" hint, holding that
	// document over for the next batch rather than dropping it.
	more := false
	if len(docs) == a.batchSize && a.cursor.Next(a.ctx()) {
		a.pending = cloneRaw(a.cursor.Current())
		a.pendingSet = true
		more = true
	}
	if err := a.cursor.Err(); err != nil {
		a.closeCursor()
		return classify(err)
	}
	if err := a.rend.renderDocs(docs); err != nil {
		return err
	}
	if more {
		return a.rend.writeText(`Type "it" for more`)
	}
	a.closeCursor()
	return nil
}

func (a *app) closeCursor() {
	if a.cursor != nil {
		_ = a.cursor.Close(context.Background())
		a.cursor = nil
	}
	a.pendingSet = false
}

// batched reports whether results should be shown one batch at a time: only in the
// interactive shell when no explicit output format was requested.
func (a *app) batched() bool {
	return a.interactive && !a.cfg.modeSet && a.rend.mode == modeJSON
}

// cloneRaw copies a cursor's current bytes, which are only valid until the next Next.
func cloneRaw(r bson.Raw) bson.Raw {
	out := make(bson.Raw, len(r))
	copy(out, r)
	return out
}
