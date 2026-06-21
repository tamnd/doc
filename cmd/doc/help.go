package main

// dotHelpText is the concise reference .help prints (spec 2061 doc 15 §3.6).
const dotHelpText = `Dot-commands (meta-operations):
  .help [cmd]         show this help or help for cmd
  .open <file>        close current file and open another
  .close              close current file (in-memory database)
  .databases          list databases
  .use <db>           switch active database
  .collections        list collections in active db
  .indexes [coll]     list indexes (all, or for one collection)
  .schema <coll> [n]  infer schema from n sample docs (default 100)
  .mode <fmt>         set output mode: json, jsonl, table, bson
  .pretty on|off      toggle JSON pretty-printing
  .headers on|off     toggle column headers in table mode
  .width [n]          set column width limit (0 = no limit)
  .timing on|off      print elapsed time after each command
  .read <file>        execute commands from a script file
  .output <file>|-    redirect output to a file or back to stdout
  .createindex <coll> <spec>  create an index
  .dropindex <coll> <name>    drop a named index
  .begin              begin an explicit transaction
  .commit             commit the current transaction
  .rollback           roll back the current transaction
  .quit               close and exit
Some commands (.stats, .pragma, .import, .export, .backup, .validate, .compact)
arrive with a later milestone and report so when called.
Type .help <cmd> for detail on any command.`

// dotHelpDetail holds the long form for individual commands.
var dotHelpDetail = map[string]string{
	"find":        "db.<coll>.find(filter, projection).sort(s).skip(n).limit(n) - query documents",
	"insertone":   "db.<coll>.insertOne(doc) - insert one document, generating _id when absent",
	"use":         ".use <db> - switch the active database; created on first write",
	"begin":       ".begin - open an explicit multi-document transaction; the prompt shows [session]",
	"createindex": ".createindex <coll> <spec> - e.g. .createindex users {\"email\":1}",
	"mode":        ".mode json|jsonl|table|bson - set the output format for later commands",
}
