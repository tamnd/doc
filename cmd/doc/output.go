package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/extjson"
)

// renderer writes command results to an io.Writer in the configured output mode. It
// carries the display switches from config so a dot-command that flips .mode or
// .pretty is reflected on the next render.
type renderer struct {
	w         io.Writer
	mode      outputMode
	pretty    bool
	canonical bool
	headers   bool
	width     int
	color     bool
}

func newRenderer(w io.Writer, c *config) *renderer {
	return &renderer{
		w:         w,
		mode:      c.mode,
		pretty:    c.pretty,
		canonical: c.canonical,
		headers:   c.headers,
		width:     c.width,
		color:     c.color,
	}
}

// renderDocs writes a slice of documents. JSON mode prints each document on its own,
// JSONL one per line, table mode aligns columns inferred from the first document, and
// BSON mode writes each document's length-prefixed bytes.
func (r *renderer) renderDocs(docs []bson.Raw) error {
	switch r.mode {
	case modeBSON:
		for _, d := range docs {
			if err := r.writeBSON(d); err != nil {
				return err
			}
		}
		return nil
	case modeTable:
		return r.writeTable(docs)
	case modeJSONL:
		for _, d := range docs {
			if err := r.writeJSONLine(d); err != nil {
				return err
			}
		}
		return nil
	default:
		for _, d := range docs {
			if err := r.writeJSON(d); err != nil {
				return err
			}
		}
		return nil
	}
}

// renderDoc writes a single result document (findOne, an ack, a command reply).
func (r *renderer) renderDoc(d bson.Raw) error {
	switch r.mode {
	case modeBSON:
		return r.writeBSON(d)
	case modeJSONL:
		return r.writeJSONLine(d)
	default:
		return r.writeJSON(d)
	}
}

func (r *renderer) opts() extjson.Options {
	return extjson.Options{Canonical: r.canonical, Indent: r.pretty}
}

func (r *renderer) writeJSON(d bson.Raw) error {
	out, err := extjson.Marshal(d, r.opts())
	if err != nil {
		return err
	}
	s := string(out)
	if r.color {
		s = colorizeJSON(s)
	}
	_, err = fmt.Fprintln(r.w, s)
	return err
}

func (r *renderer) writeJSONLine(d bson.Raw) error {
	out, err := extjson.Marshal(d, extjson.Options{Canonical: r.canonical})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(r.w, string(out))
	return err
}

// writeBSON emits the document's bytes prefixed by their 4-byte little-endian length,
// which is the BSON wire framing (spec §14.4).
func (r *renderer) writeBSON(d bson.Raw) error {
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(d)))
	if _, err := r.w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := r.w.Write(d)
	return err
}

// writeText prints a plain line that is not a document (a scalar count, a notice). It
// honors --quiet only at the call site; here it just writes.
func (r *renderer) writeText(s string) error {
	_, err := fmt.Fprintln(r.w, s)
	return err
}

// writeTable renders documents as aligned columns. Column names come from the first
// document's top-level keys; later documents that lack a column show an empty cell,
// and nested documents and arrays collapse to compact JSON (spec §14.3).
func (r *renderer) writeTable(docs []bson.Raw) error {
	if len(docs) == 0 {
		return nil
	}
	cols := topLevelKeys(docs[0])
	rows := make([][]string, 0, len(docs))
	for _, d := range docs {
		row := make([]string, len(cols))
		for i, col := range cols {
			if v, ok := d.Lookup(col); ok {
				row[i] = r.cell(v)
			}
		}
		rows = append(rows, row)
	}

	widths := make([]int, len(cols))
	for i, col := range cols {
		widths[i] = displayLen(col)
	}
	for _, row := range rows {
		for i, cell := range row {
			if l := displayLen(cell); l > widths[i] {
				widths[i] = l
			}
		}
	}
	if r.width > 0 {
		for i := range widths {
			if widths[i] > r.width {
				widths[i] = r.width
			}
		}
	}

	var sb strings.Builder
	if r.headers {
		for i, col := range cols {
			if i > 0 {
				sb.WriteString("  ")
			}
			sb.WriteString(pad(truncate(col, widths[i]), widths[i]))
		}
		sb.WriteByte('\n')
		for i := range cols {
			if i > 0 {
				sb.WriteString("  ")
			}
			sb.WriteString(strings.Repeat("-", widths[i]))
		}
		sb.WriteByte('\n')
	}
	for _, row := range rows {
		for i, cell := range row {
			if i > 0 {
				sb.WriteString("  ")
			}
			sb.WriteString(pad(truncate(cell, widths[i]), widths[i]))
		}
		sb.WriteByte('\n')
	}
	_, err := io.WriteString(r.w, sb.String())
	return err
}

// cell renders a single value for a table cell: a string bare, everything else as its
// relaxed extended JSON so an ObjectId or date reads naturally.
func (r *renderer) cell(v bson.RawValue) string {
	if v.Type == bson.TypeString {
		return v.StringValue()
	}
	single := bson.NewBuilder().AppendValue("v", v).Build()
	out, err := extjson.MarshalRelaxed(single)
	if err != nil {
		return "?"
	}
	// Strip the {"v":...} wrapper the single-field document carries.
	s := string(out)
	const prefix = `{"v":`
	if strings.HasPrefix(s, prefix) && strings.HasSuffix(s, "}") {
		return s[len(prefix) : len(s)-1]
	}
	return s
}

func topLevelKeys(d bson.Raw) []string {
	elems, err := d.Elements()
	if err != nil {
		return nil
	}
	keys := make([]string, len(elems))
	for i, e := range elems {
		keys[i] = e.Key
	}
	return keys
}

func displayLen(s string) int { return utf8.RuneCountInString(s) }

func pad(s string, w int) string {
	n := w - displayLen(s)
	if n <= 0 {
		return s
	}
	return s + strings.Repeat(" ", n)
}

func truncate(s string, w int) string {
	if w <= 0 || displayLen(s) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	r := []rune(s)
	return string(r[:w-1]) + "…"
}
