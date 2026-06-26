package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tamnd/doc"
	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/extjson"
	"github.com/tamnd/doc/options"
)

// flagSet is the parsed form of a dot-command's flag arguments: the leading
// positional words, the --key value pairs, and the bare boolean switches. The shell
// tokenizer keeps a brace- or bracket-delimited JSON value (a --filter) together as
// one token, so a flag value can be a whole document.
type flagSet struct {
	positional []string
	values     map[string]string
	bools      map[string]bool
}

// boolFlags are the switches that take no value. Everything else consumes the next
// token as its value.
var boolFlags = map[string]bool{
	"drop": true, "pretty": true, "csv-header": true, "no-csv-header": true,
	"header-line": true, "type-coerce": true, "skip-indexes": true,
	"all-databases": true, "no-indexes": true, "restore-indexes": true,
	"stop-on-error": true, "verify": true, "apply-wal": true, "pprof": true,
	"data": true,
	// serve switches
	"tls": true, "auth": true, "readonly": true, "http": true,
}

// parseFlags splits already-tokenized arguments into positionals, --key value pairs,
// and boolean switches. It accepts both --key value and --key=value.
func parseFlags(args []string) flagSet {
	fs := flagSet{values: map[string]string{}, bools: map[string]bool{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			fs.positional = append(fs.positional, a)
			continue
		}
		name := strings.TrimPrefix(a, "--")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			fs.values[name[:eq]] = name[eq+1:]
			continue
		}
		if boolFlags[name] {
			fs.bools[name] = true
			continue
		}
		if i+1 < len(args) {
			fs.values[name] = args[i+1]
			i++
		} else {
			fs.bools[name] = true
		}
	}
	return fs
}

// inferFormat picks a loader format from an explicit --format, falling back to the
// file extension, and finally to jsonl.
func inferFormat(explicit, path string) string {
	if explicit != "" {
		return strings.ToLower(explicit)
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		return "json"
	case ".bson":
		return "bson"
	case ".csv":
		return "csv"
	default:
		return "jsonl"
	}
}

// dotImport loads documents from a file (or stdin) into a collection (spec 2061 doc 15
// §7.2). The format is inferred from the extension unless --format says otherwise.
func (a *app) dotImport(args []string) error {
	fs := parseFlags(args)
	if len(fs.positional) < 1 {
		return usageErr(".import <file> --collection <coll> [--format ...] [--drop]")
	}
	path := fs.positional[0]
	coll := fs.values["collection"]
	if coll == "" {
		return usageErr(".import needs --collection <coll>")
	}
	format := inferFormat(fs.values["format"], path)

	r, closeFn, err := openInput(path)
	if err != nil {
		return cliError{code: exitIOError, msg: err.Error()}
	}
	defer closeFn()

	c := a.collection(coll)
	if fs.bools["drop"] {
		if err := c.Drop(a.ctx()); err != nil {
			return classify(err)
		}
	}

	opt := options.Load()
	if bs := fs.values["batch-size"]; bs != "" {
		if n, err := strconv.Atoi(bs); err == nil {
			opt.SetBatchSize(n)
		}
	}
	if fs.bools["stop-on-error"] {
		opt.SetOrdered(true)
	}

	var res *doc.LoadResult
	switch format {
	case "jsonl":
		res, err = c.LoadJSON(a.ctx(), r, opt)
	case "bson":
		res, err = c.LoadBSON(a.ctx(), r, opt)
	case "json":
		res, err = a.importJSONArray(c, r, opt)
	case "csv":
		res, err = a.importCSV(c, r, fs, opt)
	default:
		return usageErr(".import --format json|jsonl|csv|bson")
	}
	if err != nil {
		return classify(err)
	}
	return a.rend.writeText(fmt.Sprintf("imported %d, skipped %d into %s.%s",
		res.InsertedCount, res.FailedCount, a.dbName, coll))
}

// importJSONArray loads a single JSON array of documents.
func (a *app) importJSONArray(c *doc.Collection, r io.Reader, opt *options.LoadOptions) (*doc.LoadResult, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	docs, err := extjson.ParseArray(data)
	if err != nil {
		return nil, err
	}
	anyDocs := make([]any, len(docs))
	for i, d := range docs {
		anyDocs[i] = d
	}
	return c.Import(a.ctx(), &sliceDocIter{docs: anyDocs}, opt)
}

// importCSV loads a CSV file, one document per row. Column names come from --fields or
// the header line; cells are type-coerced unless --type-coerce is off.
func (a *app) importCSV(c *doc.Collection, r io.Reader, fs flagSet, opt *options.LoadOptions) (*doc.LoadResult, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	var header []string
	if f := fs.values["fields"]; f != "" {
		header = strings.Split(f, ",")
	} else {
		row, err := cr.Read()
		if err != nil {
			return nil, err
		}
		header = row
	}
	coerce := !fs.bools["no-type-coerce"]
	return c.Import(a.ctx(), &csvDocIter{r: cr, header: header, coerce: coerce}, opt)
}

// dotExport writes a collection (or a filtered query result) to a file or stdout in
// the chosen format (spec 2061 doc 15 §7.4).
func (a *app) dotExport(args []string) error {
	fs := parseFlags(args)
	if len(fs.positional) < 1 {
		return usageErr(".export <file> --collection <coll> [--filter {...}] [--format ...]")
	}
	path := fs.positional[0]
	coll := fs.values["collection"]
	if coll == "" {
		return usageErr(".export needs --collection <coll>")
	}
	format := inferFormat(fs.values["format"], path)

	opt := options.Find()
	if s := fs.values["sort"]; s != "" {
		sd, err := extjson.Parse([]byte(s))
		if err != nil {
			return queryError(err.Error())
		}
		opt.SetSort(sd)
	}
	if v := fs.values["limit"]; v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			opt.SetLimit(n)
		}
	}
	if v := fs.values["skip"]; v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			opt.SetSkip(n)
		}
	}
	filter, err := parseFilter(fs.values["filter"])
	if err != nil {
		return queryError(err.Error())
	}

	cur, err := a.collection(coll).Find(a.ctx(), filter, opt)
	if err != nil {
		return classify(err)
	}
	defer func() { _ = cur.Close(a.ctx()) }()

	w, closeFn, err := openOutput(path)
	if err != nil {
		return cliError{code: exitIOError, msg: err.Error()}
	}
	defer closeFn()

	var fields []string
	if f := fs.values["fields"]; f != "" {
		fields = strings.Split(f, ",")
	}
	n, err := writeExport(w, cur, a.ctx(), format, fields, !fs.bools["no-csv-header"], fs.bools["pretty"])
	if err != nil {
		return classify(err)
	}
	return a.rend.writeText(fmt.Sprintf("exported %d documents from %s.%s", n, a.dbName, coll))
}

// parseFilter turns a --filter value into a document, defaulting to match-all.
func parseFilter(s string) (bson.Raw, error) {
	if s == "" {
		return bson.NewBuilder().Build(), nil
	}
	return extjson.Parse([]byte(s))
}

// writeExport streams a cursor to w in the chosen format and returns the count.
func writeExport(w io.Writer, cur *doc.Cursor, ctx context.Context, format string, fields []string, csvHeader, pretty bool) (int64, error) {
	bw := bufio.NewWriter(w)
	defer func() { _ = bw.Flush() }()
	var n int64
	switch format {
	case "bson":
		for cur.Next(ctx) {
			if _, err := bw.Write(cur.Current()); err != nil {
				return n, err
			}
			n++
		}
	case "csv":
		cw := csv.NewWriter(bw)
		wroteHeader := false
		for cur.Next(ctx) {
			doc := cur.Current()
			cols := fields
			if cols == nil {
				cols = topKeys(doc)
			}
			if csvHeader && !wroteHeader {
				if err := cw.Write(cols); err != nil {
					return n, err
				}
				wroteHeader = true
			}
			if err := cw.Write(csvRow(doc, cols)); err != nil {
				return n, err
			}
			n++
		}
		cw.Flush()
		return n, cw.Error()
	case "json":
		if _, err := bw.WriteString("[\n"); err != nil {
			return n, err
		}
		for cur.Next(ctx) {
			if n > 0 {
				if _, err := bw.WriteString(",\n"); err != nil {
					return n, err
				}
			}
			out, err := extjson.MarshalRelaxed(cur.Current())
			if err != nil {
				return n, err
			}
			if _, err := bw.Write(out); err != nil {
				return n, err
			}
			n++
		}
		if _, err := bw.WriteString("\n]\n"); err != nil {
			return n, err
		}
	default: // jsonl
		for cur.Next(ctx) {
			out, err := extjson.MarshalRelaxed(cur.Current())
			if err != nil {
				return n, err
			}
			if _, err := bw.Write(out); err != nil {
				return n, err
			}
			if err := bw.WriteByte('\n'); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, cur.Err()
}

// topKeys returns the top-level field names of a document in stored order.
func topKeys(d bson.Raw) []string {
	elems, err := d.Elements()
	if err != nil {
		return nil
	}
	keys := make([]string, 0, len(elems))
	for _, e := range elems {
		keys = append(keys, e.Key)
	}
	return keys
}

// csvRow renders the named columns of a document as CSV cells. A field absent from
// the document is an empty cell; a nested value is its relaxed JSON form.
func csvRow(d bson.Raw, cols []string) []string {
	row := make([]string, len(cols))
	for i, c := range cols {
		v, ok := d.Lookup(c)
		if !ok {
			continue
		}
		row[i] = cellString(v)
	}
	return row
}

// cellString renders one value as a CSV cell. Scalars print bare; an ObjectId prints
// as its 24-hex form and a date as ISO-8601, matching mongoexport; a nested document
// or array prints as compact relaxed JSON (spec 2061 doc 15 §7.5).
func cellString(v bson.RawValue) string {
	switch v.Type {
	case bson.TypeString:
		return v.StringValue()
	case bson.TypeInt32:
		return strconv.FormatInt(int64(v.Int32()), 10)
	case bson.TypeInt64:
		return strconv.FormatInt(v.Int64(), 10)
	case bson.TypeDouble:
		return strconv.FormatFloat(v.Double(), 'g', -1, 64)
	case bson.TypeBoolean:
		return strconv.FormatBool(v.Boolean())
	case bson.TypeObjectID:
		return v.ObjectID().Hex()
	case bson.TypeNull:
		return ""
	}
	// Anything richer (document, array, date, binary) goes through the relaxed
	// encoder by wrapping it in a one-field document and stripping the envelope.
	wrapped := bson.NewBuilder().AppendValue("v", v).Build()
	out, err := extjson.MarshalRelaxed(wrapped)
	if err != nil {
		return ""
	}
	s := string(out)
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = strings.TrimSpace(s[i+1:])
		s = strings.TrimSuffix(s, "}")
		s = strings.TrimSpace(s)
	}
	return strings.Trim(s, "\"")
}

// openInput opens a path for reading, treating "-" as stdin.
func openInput(path string) (io.Reader, func(), error) {
	if path == "-" {
		return os.Stdin, func() {}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	return f, func() { _ = f.Close() }, nil
}

// openOutput opens a path for writing, treating "-" as stdout.
func openOutput(path string) (io.Writer, func(), error) {
	if path == "-" {
		return os.Stdout, func() {}, nil
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, nil, err
	}
	return f, func() { _ = f.Close() }, nil
}

// sliceDocIter is a DocumentIterator over an in-memory slice, used for a JSON-array
// import.
type sliceDocIter struct {
	docs []any
	i    int
}

func (s *sliceDocIter) Next(context.Context) bool {
	if s.i >= len(s.docs) {
		return false
	}
	s.i++
	return true
}
func (s *sliceDocIter) Document() (any, error) { return s.docs[s.i-1], nil }
func (s *sliceDocIter) Err() error             { return nil }
func (s *sliceDocIter) Close() error           { return nil }

// csvDocIter turns CSV rows into documents on demand, one per Next.
type csvDocIter struct {
	r      *csv.Reader
	header []string
	coerce bool
	cur    []string
	err    error
}

func (it *csvDocIter) Next(context.Context) bool {
	row, err := it.r.Read()
	if err == io.EOF {
		return false
	}
	if err != nil {
		it.err = err
		return false
	}
	it.cur = row
	return true
}

func (it *csvDocIter) Document() (any, error) {
	b := bson.NewBuilder()
	for i, name := range it.header {
		if i >= len(it.cur) {
			break
		}
		appendCell(b, name, it.cur[i], it.coerce)
	}
	return b.Build(), nil
}

func (it *csvDocIter) Err() error   { return it.err }
func (it *csvDocIter) Close() error { return nil }

// appendCell adds one CSV cell to a document builder, inferring an int, a float, or a
// bool when --type-coerce is on, otherwise storing the raw string.
func appendCell(b *bson.Builder, name, cell string, coerce bool) {
	if !coerce {
		b.AppendString(name, cell)
		return
	}
	if n, err := strconv.ParseInt(cell, 10, 64); err == nil {
		b.AppendInt64(name, n)
		return
	}
	if f, err := strconv.ParseFloat(cell, 64); err == nil {
		b.AppendDouble(name, f)
		return
	}
	switch strings.ToLower(cell) {
	case "true":
		b.AppendBoolean(name, true)
		return
	case "false":
		b.AppendBoolean(name, false)
		return
	}
	b.AppendString(name, cell)
}
