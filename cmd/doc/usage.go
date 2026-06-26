package main

import "github.com/tamnd/doc"

// versionLine is what --version and `doc version` print.
func versionLine() string {
	return "doc " + doc.Version
}

// versionLineValue is the bare version string without the leading "doc ".
func versionLineValue() string {
	return doc.Version
}

// subcommands are the non-interactive top-level verbs (spec 2061 doc 15 appendix A).
// Several are wired to the engine now; the rest report that they arrive with a later
// milestone rather than silently doing nothing.
var subcommands = map[string]bool{
	"version":  true,
	"info":     true,
	"stats":    true,
	"validate": true,
	"schema":   true,
	"import":   true,
	"export":   true,
	"dump":     true,
	"load":     true,
	"backup":   true,
	"restore":  true,
	"compact":  true,
	"reindex":  true,
	"serve":    true,
	"rekey":    true,
}

func isSubcommand(s string) bool { return subcommands[s] }

const usageText = `doc - embedded MongoDB-compatible document database

Usage:
  doc [global-flags] [file] [subcommand [args]]

With a file and no subcommand, the interactive shell opens on that file.
With no file and no subcommand, the shell opens on an in-memory database.
The special file :memory: requests an in-memory database explicitly.

Global flags:
  -e, --eval <expr>       run one expression or dot-command and exit (repeatable)
  -f, --file <path>       run commands from a script file ("-" for stdin), then exit
  -q, --quiet             suppress banners and prompts; only data reaches stdout
      --json              output pretty extended JSON (default on a TTY)
      --jsonl             output one JSON object per line (default in a pipe)
      --table             output an aligned ASCII table
      --bson              output raw BSON binary
      --pretty            pretty-print JSON (default on a TTY)
      --no-pretty         force compact JSON
      --canonical         use canonical extended JSON rather than relaxed
  -r, --readonly          open the file read-only
      --pragma <k=v>      set a PRAGMA before opening (repeatable)
      --cache <bytes>     buffer pool size; accepts K, M, G suffixes (default 64M)
      --sync <level>      OFF, NORMAL, FULL, or EXTRA (default NORMAL)
      --db <name>         database to use on open (default "default")
      --passphrase <p>    open an encrypted file with this passphrase (or DOC_PASSPHRASE)
      --key-file <path>   open an encrypted file with a raw 32-byte key from this file
      --width <n>         column width limit in table mode
      --limit <n>         truncate query results to at most n documents
      --no-color          suppress ANSI colors
      --force             proceed past destructive-operation confirmations
      --stop-on-error     in script mode, exit on the first error
  -v, --version           print the version and exit
  -h, --help              print this help and exit

Subcommands:
  doc version             print the version string
  doc info <file>         print the file header
  doc validate <file>     run the integrity check (exit 4 on failure)
  doc stats <file>        print database statistics
  doc <file> rekey ...    rotate the encryption key (--new-passphrase or --new-key-file;
                          --data re-encrypts every page under a fresh data key)

Run "doc <file>" then ".help" inside the shell for the full command reference.
`
