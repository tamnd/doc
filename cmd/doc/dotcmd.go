package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/tamnd/doc"
	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/extjson"
	"github.com/tamnd/doc/options"
)

// runDot handles a meta dot-command. Names are case-insensitive after the dot
// (spec 2061 doc 15 §6). Commands that depend on a later milestone report that plainly
// rather than appearing to succeed.
func (a *app) runDot(line string) error {
	fields := splitFields(line[1:])
	if len(fields) == 0 {
		return usageErr(".<command>")
	}
	cmd := strings.ToLower(fields[0])
	args := fields[1:]

	switch cmd {
	case "help":
		return a.dotHelp(args)
	case "quit", "exit":
		return errQuit
	case "open":
		if len(args) < 1 {
			return usageErr(".open <file>")
		}
		return a.openFile(args[0])
	case "close":
		return a.openFile(":memory:")
	case "databases":
		return a.dotDatabases()
	case "use":
		if len(args) < 1 {
			return usageErr(".use <db>")
		}
		a.dbName = args[0]
		return a.rend.writeText("switched to db " + a.dbName)
	case "collections":
		return a.dotCollections()
	case "indexes":
		return a.dotIndexes(args)
	case "schema":
		return a.dotSchema(args)
	case "mode":
		return a.dotMode(args)
	case "pretty":
		return a.toggle(args, &a.rend.pretty, "pretty")
	case "headers":
		return a.toggle(args, &a.rend.headers, "headers")
	case "timing":
		return a.toggle(args, &a.timing, "timing")
	case "width":
		return a.dotWidth(args)
	case "read":
		if len(args) < 1 {
			return usageErr(".read <file>")
		}
		return a.runScriptFile(args[0])
	case "output":
		return a.dotOutput(args)
	case "createindex":
		return a.dotCreateIndex(args)
	case "dropindex":
		return a.dotDropIndex(args)
	case "begin":
		return a.dotBegin()
	case "commit":
		return a.dotCommit()
	case "rollback":
		return a.dotRollback()
	case "stats":
		return a.dotStats(args)
	case "dbstats":
		return a.dotDBStats()
	case "import", "export", "dump", "load", "backup",
		"restore", "explain", "profile", "pragma", "validate", "compact",
		"reindex", "vacuum", "pager":
		return a.dotDeferred(cmd)
	default:
		return usageErr("unknown dot-command: ." + cmd)
	}
}

func (a *app) dotHelp(args []string) error {
	if len(args) > 0 {
		if d, ok := dotHelpDetail[strings.ToLower(args[0])]; ok {
			return a.rend.writeText(d)
		}
		return a.rend.writeText("no help for ." + args[0])
	}
	return a.rend.writeText(dotHelpText)
}

func (a *app) dotDatabases() error {
	names, err := a.db.ListDatabaseNames(a.ctx(), bson.NewBuilder().Build())
	if err != nil {
		return classify(err)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := a.rend.writeText(n); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) dotCollections() error {
	names, err := a.db.Database(a.dbName).ListCollectionNames(a.ctx(), bson.NewBuilder().Build())
	if err != nil {
		return classify(err)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := a.rend.writeText(n); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) dotIndexes(args []string) error {
	var colls []string
	if len(args) > 0 {
		colls = []string{args[0]}
	} else {
		names, err := a.db.Database(a.dbName).ListCollectionNames(a.ctx(), bson.NewBuilder().Build())
		if err != nil {
			return classify(err)
		}
		sort.Strings(names)
		colls = names
	}
	for _, cn := range colls {
		specs, err := a.collection(cn).Indexes().ListSpecifications(a.ctx())
		if err != nil {
			return classify(err)
		}
		for _, s := range specs {
			line := cn + ": " + s.Name
			if s.Unique {
				line += " (unique)"
			}
			if s.ExpireAfterSeconds != nil {
				line += " (ttl " + strconv.FormatInt(int64(*s.ExpireAfterSeconds), 10) + "s)"
			}
			if err := a.rend.writeText(line); err != nil {
				return err
			}
		}
	}
	return nil
}

// dotStats prints collStats for a named collection, or dbStats for the current
// database when no collection is given (spec 2061 doc 15 §6). The reply is the same
// document RunCommand returns to a driver.
func (a *app) dotStats(args []string) error {
	if len(args) > 0 {
		cmd := bson.NewBuilder().AppendString("collStats", args[0]).Build()
		return a.renderCommand(cmd)
	}
	return a.dotDBStats()
}

// dotDBStats prints dbStats for the current database.
func (a *app) dotDBStats() error {
	return a.renderCommand(bson.NewBuilder().AppendInt32("dbStats", 1).Build())
}

// renderCommand runs cmd through the database dispatcher and prints its reply.
func (a *app) renderCommand(cmd bson.Raw) error {
	res := a.db.Database(a.dbName).RunCommand(a.ctx(), cmd)
	raw, err := res.Raw()
	if err != nil {
		return classify(err)
	}
	return a.rend.renderDoc(bson.Raw(raw))
}

// dotSchema infers a flat field-frequency summary from up to n sample documents. It is
// a sampling walk over the collection, not a stored schema (spec §11.4).
func (a *app) dotSchema(args []string) error {
	if len(args) < 1 {
		return usageErr(".schema <coll> [n]")
	}
	n := 100
	if len(args) > 1 {
		if v, err := strconv.Atoi(args[1]); err == nil {
			n = v
		}
	}
	opt := options.Find().SetLimit(int64(n))
	cur, err := a.collection(args[0]).Find(a.ctx(), bson.NewBuilder().Build(), opt)
	if err != nil {
		return classify(err)
	}
	defer func() { _ = cur.Close(context.Background()) }()
	counts := map[string]int{}
	types := map[string]string{}
	total := 0
	for cur.Next(a.ctx()) {
		total++
		elems, err := cur.Current().Elements()
		if err != nil {
			continue
		}
		for _, e := range elems {
			counts[e.Key]++
			types[e.Key] = e.Value.Type.String()
		}
	}
	if err := cur.Err(); err != nil {
		return classify(err)
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if err := a.rend.writeText(fmt.Sprintf("%s: %d documents sampled", args[0], total)); err != nil {
		return err
	}
	for _, k := range keys {
		pct := 0
		if total > 0 {
			pct = counts[k] * 100 / total
		}
		if err := a.rend.writeText(fmt.Sprintf("  %-24s %-10s %3d%%", k, types[k], pct)); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) dotMode(args []string) error {
	if len(args) < 1 {
		return usageErr(".mode json|jsonl|table|bson")
	}
	m, ok := parseMode(args[0])
	if !ok {
		return usageErr(".mode json|jsonl|table|bson")
	}
	a.rend.mode = m
	a.cfg.modeSet = true
	if m == modeBSON {
		a.rend.canonical = true
		a.rend.pretty = false
	}
	return nil
}

func (a *app) dotWidth(args []string) error {
	if len(args) < 1 {
		return a.rend.writeText(strconv.Itoa(a.rend.width))
	}
	n, err := strconv.Atoi(args[0])
	if err != nil {
		return usageErr(".width <n>")
	}
	a.rend.width = n
	return nil
}

func (a *app) dotOutput(args []string) error {
	if len(args) < 1 || args[0] == "-" {
		a.setOutput(os.Stdout)
		return nil
	}
	f, err := os.Create(args[0])
	if err != nil {
		return cliError{code: exitIOError, msg: err.Error()}
	}
	a.setOutput(f)
	return nil
}

// setOutput repoints the renderer at a new writer, closing any previous file target.
func (a *app) setOutput(w *os.File) {
	if cur, ok := a.out.(*os.File); ok && cur != os.Stdout && cur != os.Stderr {
		_ = cur.Close()
	}
	a.out = w
	a.rend.w = w
}

func (a *app) dotCreateIndex(args []string) error {
	if len(args) < 2 {
		return usageErr(".createindex <coll> <spec>")
	}
	keys, err := extjson.Parse([]byte(args[1]))
	if err != nil {
		return queryError(err.Error())
	}
	name, err := a.collection(args[0]).Indexes().CreateOne(a.ctx(), doc.IndexModel{Keys: keys})
	if err != nil {
		return classify(err)
	}
	return a.rend.writeText("created index " + name)
}

func (a *app) dotDropIndex(args []string) error {
	if len(args) < 2 {
		return usageErr(".dropindex <coll> <name>")
	}
	if _, err := a.collection(args[0]).Indexes().DropOne(a.ctx(), args[1]); err != nil {
		return classify(err)
	}
	return a.rend.writeText("dropped index " + args[1])
}

func (a *app) dotBegin() error {
	if a.sess != nil {
		return queryError("a transaction is already open")
	}
	sess, err := a.db.StartSession()
	if err != nil {
		return classify(err)
	}
	if err := sess.StartTransaction(); err != nil {
		sess.EndSession(context.Background())
		return classify(err)
	}
	a.sess = sess
	a.txnCtx = doc.NewSessionContext(context.Background(), sess)
	return a.rend.writeText("transaction started")
}

func (a *app) dotCommit() error {
	if a.sess == nil {
		return queryError("no transaction is open")
	}
	err := a.sess.CommitTransaction(context.Background())
	a.endSession()
	if err != nil {
		return classify(err)
	}
	return a.rend.writeText("transaction committed")
}

func (a *app) dotRollback() error {
	if a.sess == nil {
		return queryError("no transaction is open")
	}
	err := a.sess.AbortTransaction(context.Background())
	a.endSession()
	if err != nil {
		return classify(err)
	}
	return a.rend.writeText("transaction rolled back")
}

func (a *app) endSession() {
	if a.sess != nil {
		a.sess.EndSession(context.Background())
		a.sess = nil
	}
	a.txnCtx = nil
}

// dotDeferred reports a command whose backing feature lands in a later milestone.
func (a *app) dotDeferred(cmd string) error {
	return cliError{
		code: exitUsage,
		msg:  "." + cmd + " is not available in this build yet (it lands with a later milestone: stats/pragma/validate/compact with M6-e, import/export/dump/load with the loaders, backup/restore with M7)",
	}
}

// toggle sets a bool flag from an on/off argument, or flips it when no argument given.
func (a *app) toggle(args []string, flag *bool, name string) error {
	if len(args) == 0 {
		*flag = !*flag
	} else {
		switch strings.ToLower(args[0]) {
		case "on", "true", "1", "yes":
			*flag = true
		case "off", "false", "0", "no":
			*flag = false
		default:
			return usageErr("." + name + " on|off")
		}
	}
	return nil
}

// splitFields splits a dot-command into whitespace-separated fields while keeping a
// brace- or bracket-delimited JSON argument (an index spec) together as one field.
func splitFields(s string) []string {
	var fields []string
	depth := 0
	inStr := false
	start := -1
	flush := func(end int) {
		if start >= 0 {
			fields = append(fields, s[start:end])
			start = -1
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch c {
			case '\\':
				i++
			case '"':
				inStr = false
			}
			continue
		}
		switch {
		case c == '"':
			inStr = true
			if start < 0 {
				start = i
			}
		case c == '{' || c == '[':
			if start < 0 {
				start = i
			}
			depth++
		case c == '}' || c == ']':
			depth--
		case (c == ' ' || c == '\t') && depth == 0:
			flush(i)
		default:
			if start < 0 {
				start = i
			}
		}
	}
	flush(len(s))
	return fields
}
