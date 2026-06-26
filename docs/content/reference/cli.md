---
title: "CLI"
description: "Every doc command, global flag, subcommand, and interactive shell dot-command."
weight: 10
---

The `doc` binary is three things in one: a one-shot evaluator (`--eval`), an interactive shell, and a set of non-interactive subcommands.
With a file and no subcommand it opens the interactive shell on that file; with no file it opens an in-memory database.
The special path `:memory:` requests an in-memory database explicitly.

```
doc [global-flags] [file] [subcommand [args]]
```

## Global flags

| Flag | Meaning |
| ---- | ------- |
| `-e, --eval <expr>` | Run one expression or dot-command and exit (repeatable). |
| `-f, --file <path>` | Run commands from a script file (`-` for stdin), then exit. |
| `-q, --quiet` | Suppress banners and prompts; only data reaches stdout. |
| `--json` | Pretty extended JSON (default on a terminal). |
| `--jsonl` | One JSON object per line (default in a pipe). |
| `--table` | Aligned ASCII table. |
| `--bson` | Raw BSON binary. |
| `--pretty` / `--no-pretty` | Force pretty or compact JSON. |
| `--canonical` | Canonical extended JSON rather than relaxed. |
| `-r, --readonly` | Open the file read-only. |
| `--pragma <k=v>` | Set a PRAGMA before opening (repeatable). |
| `--cache <bytes>` | Buffer pool size; accepts K, M, G suffixes (default 64M). |
| `--sync <level>` | `OFF`, `NORMAL`, `FULL`, or `EXTRA` (default `NORMAL`). |
| `--db <name>` | Database to use on open (default `default`). |
| `--passphrase <p>` | Open an encrypted file with this passphrase (or `DOC_PASSPHRASE`). |
| `--key-file <path>` | Open an encrypted file with a raw 32-byte key from this file. |
| `--width <n>` | Column width limit in table mode. |
| `--limit <n>` | Truncate query results to at most n documents. |
| `--no-color` | Suppress ANSI colors. |
| `--force` | Proceed past destructive-operation confirmations. |
| `--stop-on-error` | In script mode, exit on the first error. |
| `-v, --version` | Print the version and exit. |
| `-h, --help` | Print help and exit. |

## Subcommands

These run without entering the shell.

| Command | Meaning |
| ------- | ------- |
| `doc version` | Print the version string. |
| `doc info <file>` | Print the file header. |
| `doc validate <file>` | Run the integrity check (exit 4 on failure). |
| `doc stats <file>` | Print database statistics. |
| `doc <file> compact` | Rewrite the file, reclaiming space from deleted documents. |
| `doc <file> checkpoint [mode]` | Fold the WAL into the main file. |
| `doc <file> vacuum [pages]` | Reclaim trailing free pages to the OS. |
| `doc <file> schema <coll> [n]` | Infer a schema from n sample documents. |
| `doc <file> import ...` | Bulk-load a file into a collection. |
| `doc <file> export ...` | Write a query result to a file. |
| `doc <file> dump ...` / `load ...` | Move whole databases with index sidecars. |
| `doc <file> backup ...` / `restore ...` | Online physical backup and restore. |
| `doc <file> reindex` | Rebuild indexes. |
| `doc <file> rekey ...` | Rotate the encryption key (`--new-passphrase` or `--new-key-file`; `--data` re-encrypts every page under a fresh data key). |
| `doc <file> serve ...` | Serve the MongoDB wire protocol (see below). |

### serve

```
doc <file> serve [--bind addr] [--port 27017] [--readonly]
                 [--auth] [--tls --tls-cert f --tls-key f [--tls-ca f]]
                 [--max-conns n] [--max-conn-idle d] [--http]
```

`--bind` defaults to loopback; use `0.0.0.0` to listen on all interfaces.
`--auth` requires SCRAM-SHA-256 authentication with role-based access control.
`--tls` enables TLS, and `--tls-ca` turns on x509 client-certificate verification.
`--http` adds a metrics and health surface.
See the [wire-server guide](/guides/the-wire-server/) for connection examples.

## Shell dot-commands

Inside the interactive shell, regular `mongosh`-style calls (`db.users.find({...})`) run queries, and dot-commands run meta operations.

```
.help [cmd]         show help, or help for one command
.open <file>        close the current file and open another
.close              close the current file (in-memory database)
.databases          list databases
.use <db>           switch the active database
.collections        list collections in the active database
.indexes [coll]     list indexes (all, or for one collection)
.schema <coll> [n]  infer a schema from n sample documents (default 100)
.mode <fmt>         set output mode: json, jsonl, table, bson
.pretty on|off      toggle JSON pretty-printing
.headers on|off     toggle column headers in table mode
.width [n]          set the column width limit (0 = no limit)
.timing on|off      print elapsed time after each command
.read <file>        execute commands from a script file
.output <file>|-    redirect output to a file or back to stdout
.createindex <coll> <spec>  create an index
.dropindex <coll> <name>    drop a named index
.stats [coll]       collStats for a collection, or dbStats for the database
.import <file> --collection <c> [--format json|jsonl|csv|bson] [--drop]
.export <file> --collection <c> [--filter <f>] [--fields a,b] [--format ...]
.dump [dir] [--db <name>] [--collection <c>] [--skip-indexes]
.load <dir> [--db <name>] [--drop] [--no-indexes]
.pragma [name[=value]]      read or write an engine setting, or list all
.check [full]       verify file, heap, and index integrity
.compact            rewrite the file, reclaiming space
.checkpoint [mode]  fold the WAL into the main file without closing
.vacuum [pages]     reclaim trailing free pages (needs auto_vacuum)
.backup --out <file> [--verify]   stream a consistent physical backup
.restore --base <img> --out <file> [--wal-source <dir>]   rebuild from a backup
.explain <coll> [filter] [verbosity]   show the query plan for a find
.begin / .commit / .rollback   explicit transaction control
.quit               close and exit
```

Run `.help <cmd>` inside the shell for the long form of any command.

## Exit codes

`0` is success.
`4` is the integrity or validation failure code, so a script can branch on `doc <file> validate; echo $?`.
Usage errors and query errors carry their own non-zero codes.
