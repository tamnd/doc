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
  .stats [coll]       print collStats for a collection, or dbStats for the db
  .import <file> --collection <c> [--format json|jsonl|csv|bson] [--drop]
  .export <file> --collection <c> [--filter <f>] [--fields a,b] [--format ...]
  .dump [dir] [--db <name>] [--collection <c>] [--skip-indexes]
  .load <dir> [--db <name>] [--drop] [--no-indexes]
  .pragma [name[=value]]      read or write an engine setting, or list all
  .check [full]       verify file, heap, and index integrity (full adds checksums)
  .compact            rewrite the file, reclaiming space from deleted documents
  .checkpoint [mode]  fold the WAL into the main file without closing
  .vacuum [pages]     reclaim trailing free pages to the OS (needs auto_vacuum)
  .explain <coll> [filter] [verbosity]   show the query plan for a find
  .begin              begin an explicit transaction
  .commit             commit the current transaction
  .rollback           roll back the current transaction
  .quit               close and exit
Some commands (.backup, .restore) arrive with a later milestone and
report so when called.
Type .help <cmd> for detail on any command.`

// dotHelpDetail holds the long form for individual commands.
var dotHelpDetail = map[string]string{
	"find":        "db.<coll>.find(filter, projection).sort(s).skip(n).limit(n) - query documents",
	"insertone":   "db.<coll>.insertOne(doc) - insert one document, generating _id when absent",
	"use":         ".use <db> - switch the active database; created on first write",
	"begin":       ".begin - open an explicit multi-document transaction; the prompt shows [session]",
	"createindex": ".createindex <coll> <spec> - e.g. .createindex users {\"email\":1}",
	"mode":        ".mode json|jsonl|table|bson - set the output format for later commands",
	"import":      ".import <file> --collection <c> [--format json|jsonl|csv|bson] [--fields a,b] [--drop] [--batch-size n] [--stop-on-error] - bulk load a file into a collection; - reads stdin",
	"export":      ".export <file> --collection <c> [--filter <json>] [--fields a,b] [--sort <json>] [--skip n] [--limit n] [--format json|jsonl|csv|bson] - write a query result to a file; - writes stdout",
	"dump":        ".dump [dir] [--db <name>] [--collection <c>] [--skip-indexes] - write each collection as a bson stream plus an index sidecar under dir/<db>; - streams jsonl to stdout",
	"load":        ".load <dir> [--db <name>] [--drop] [--no-indexes] - read a dump directory back, recreating indexes from the sidecars",
	"pragma":      ".pragma [name[=value]] - with no argument list every engine setting; with a name read it; with name=value write it. Writable: synchronous, default_isolation",
	"check":       ".check [full] - walk the freelist, heap, and every index, reporting any corruption; full also re-reads every page to verify its checksum. Exits non-zero when a problem is found",
	"compact":     ".compact - rebuild the file into a fresh, hole-free copy, reclaiming the space held by deleted documents, superseded versions, and forwarding tombstones. Offline: nothing else runs during it",
	"checkpoint":  ".checkpoint [mode] - fold the write-ahead log into the main file and start a fresh WAL, online, without closing the database. The mode (passive, full, restart, truncate) is accepted for compatibility; doc runs the same full checkpoint for each",
	"vacuum":      ".vacuum [pages] - reclaim trailing free pages to the operating system, shrinking the file. With a page count it reclaims at most that many; without one it reclaims every trailing free page. Requires PRAGMA auto_vacuum to be incremental or full",
	"explain":     ".explain <coll> [filter] [verbosity] - show how the planner would run a find: the chosen plan and the index it picks. Verbosity is queryPlanner (default) or executionStats",
}
