package main

import (
	"strconv"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/extjson"
)

// runCommand executes a raw JSON command document, the form a driver sends over the
// wire (spec 2061 doc 15 §4.2). The command name is the first key. Until the wire
// RunCommand dispatcher lands in M6-e, the shell translates the common commands into
// the same collection methods the mongosh helpers use, which is observably identical
// for these cases.
func (a *app) runCommand(cmd bson.Raw) error {
	elems, err := cmd.Elements()
	if err != nil || len(elems) == 0 {
		return queryError("empty command document")
	}
	name := elems[0].Key
	target := ""
	if elems[0].Value.Type == bson.TypeString {
		target = elems[0].Value.StringValue()
	}

	switch name {
	case "find":
		return a.cmdFind(target, cmd)
	case "count":
		return a.cmdCount(target, cmd)
	case "distinct":
		return a.cmdDistinct(target, cmd)
	case "aggregate":
		return a.cmdAggregate(target, cmd)
	case "insert":
		return a.cmdInsert(target, cmd)
	case "delete":
		return a.cmdDelete(target, cmd)
	case "update":
		return a.cmdUpdate(target, cmd)
	default:
		return cliError{code: exitQueryError, msg: "command not supported in this build: " + name + " (raw command dispatch arrives with the wire server in M8; use the db." + target + " helper)"}
	}
}

// subDoc pulls a nested document field out of a command, encoding it back to JSON text
// so it can flow through the same helper argument path.
func subDoc(cmd bson.Raw, key string) (string, bool) {
	v, ok := cmd.Lookup(key)
	if !ok {
		return "", false
	}
	wrapped := bson.NewBuilder().AppendValue("v", v).Build()
	out, err := extjson.Marshal(wrapped, extjson.Options{Canonical: true})
	if err != nil {
		return "", false
	}
	s := string(out)
	const prefix = `{"v":`
	if len(s) > len(prefix)+1 {
		return s[len(prefix) : len(s)-1], true
	}
	return "", false
}

func (a *app) cmdFind(target string, cmd bson.Raw) error {
	hc := helperCall{coll: target, method: "find"}
	if f, ok := subDoc(cmd, "filter"); ok {
		hc.args = append(hc.args, f)
	} else {
		hc.args = append(hc.args, "{}")
	}
	if p, ok := subDoc(cmd, "projection"); ok {
		hc.args = append(hc.args, p)
	}
	if s, ok := subDoc(cmd, "sort"); ok {
		hc.chain = append(hc.chain, chainCall{name: "sort", arg: s})
	}
	if v, ok := cmd.Lookup("skip"); ok {
		if n, ok := v.AsFloat64(); ok {
			hc.chain = append(hc.chain, chainCall{name: "skip", arg: strconv.FormatInt(int64(n), 10)})
		}
	}
	if v, ok := cmd.Lookup("limit"); ok {
		if n, ok := v.AsFloat64(); ok {
			hc.chain = append(hc.chain, chainCall{name: "limit", arg: strconv.FormatInt(int64(n), 10)})
		}
	}
	return a.runHelper(hc)
}

func (a *app) cmdCount(target string, cmd bson.Raw) error {
	hc := helperCall{coll: target, method: "countDocuments"}
	if q, ok := subDoc(cmd, "query"); ok {
		hc.args = append(hc.args, q)
	} else if f, ok := subDoc(cmd, "filter"); ok {
		hc.args = append(hc.args, f)
	}
	return a.runHelper(hc)
}

func (a *app) cmdDistinct(target string, cmd bson.Raw) error {
	key, ok := cmd.Lookup("key")
	if !ok || key.Type != bson.TypeString {
		return queryError("distinct command needs a key string")
	}
	hc := helperCall{coll: target, method: "distinct", args: []string{strconv.Quote(key.StringValue())}}
	if q, ok := subDoc(cmd, "query"); ok {
		hc.args = append(hc.args, q)
	}
	return a.runHelper(hc)
}

func (a *app) cmdAggregate(target string, cmd bson.Raw) error {
	p, ok := subDoc(cmd, "pipeline")
	if !ok {
		return queryError("aggregate command needs a pipeline")
	}
	return a.runHelper(helperCall{coll: target, method: "aggregate", args: []string{p}})
}

func (a *app) cmdInsert(target string, cmd bson.Raw) error {
	d, ok := subDoc(cmd, "documents")
	if !ok {
		return queryError("insert command needs documents")
	}
	return a.runHelper(helperCall{coll: target, method: "insertMany", args: []string{d}})
}

func (a *app) cmdDelete(target string, cmd bson.Raw) error {
	// The delete command carries an array of {q, limit} statements; run each.
	v, ok := cmd.Lookup("deletes")
	if !ok || v.Type != bson.TypeArray {
		return queryError("delete command needs a deletes array")
	}
	stmts, _ := v.Document().Elements()
	for _, st := range stmts {
		if st.Value.Type != bson.TypeDocument {
			continue
		}
		stmt := st.Value.Document()
		q, _ := subDoc(stmt, "q")
		method := "deleteMany"
		if lv, ok := stmt.Lookup("limit"); ok {
			if n, ok := lv.AsFloat64(); ok && n == 1 {
				method = "deleteOne"
			}
		}
		if err := a.runHelper(helperCall{coll: target, method: method, args: []string{q}}); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) cmdUpdate(target string, cmd bson.Raw) error {
	v, ok := cmd.Lookup("updates")
	if !ok || v.Type != bson.TypeArray {
		return queryError("update command needs an updates array")
	}
	stmts, _ := v.Document().Elements()
	for _, st := range stmts {
		if st.Value.Type != bson.TypeDocument {
			continue
		}
		stmt := st.Value.Document()
		q, _ := subDoc(stmt, "q")
		u, _ := subDoc(stmt, "u")
		method := "updateOne"
		if lookupBool(stmt, "multi") {
			method = "updateMany"
		}
		args := []string{q, u}
		if lookupBool(stmt, "upsert") {
			args = append(args, `{"upsert":true}`)
		}
		if err := a.runHelper(helperCall{coll: target, method: method, args: args}); err != nil {
			return err
		}
	}
	return nil
}
