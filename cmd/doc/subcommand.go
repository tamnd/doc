package main

import (
	"fmt"
	"os"
)

// runSubcommand executes a non-interactive subcommand (spec 2061 doc 15 appendix A).
// The handful that the engine supports today run against the open database; the rest
// report that they arrive with a later milestone, with a usage exit code so a script
// notices.
func (a *app) runSubcommand() int {
	switch a.cfg.subcommand {
	case "info":
		return a.subInfo()
	case "validate":
		return a.subValidate()
	case "stats":
		if err := a.dotStats(a.cfg.subArgs); err != nil {
			return reportTop(err)
		}
		return exitOK
	case "schema":
		if len(a.cfg.subArgs) == 0 {
			return reportTop(usageErr("doc schema <file> <coll> [n]"))
		}
		if err := a.dotSchema(a.cfg.subArgs); err != nil {
			return reportTop(err)
		}
		return exitOK
	case "import", "export", "dump", "load", "backup", "restore", "compact", "reindex":
		return reportTop(a.dotDeferred(a.cfg.subcommand))
	case "serve":
		return reportTop(cliError{code: exitUsage, msg: "doc serve arrives with the wire server in M8"})
	default:
		return reportTop(usageErr("unknown subcommand: " + a.cfg.subcommand))
	}
}

// subInfo prints the basic file identity. A deep header dump (page size, free list,
// counts) lands with the stats work in M6-e; this reports what is known without it.
func (a *app) subInfo() int {
	path := displayPath(a.cfg.file)
	_, _ = fmt.Fprintf(os.Stdout, "file:    %s\n", path)
	_, _ = fmt.Fprintf(os.Stdout, "version: %s\n", versionLineValue())
	names, err := a.db.ListDatabaseNames(a.ctx(), emptyDoc())
	if err != nil {
		return reportTop(classify(err))
	}
	_, _ = fmt.Fprintf(os.Stdout, "databases: %d\n", len(names))
	return exitOK
}

// subValidate runs an integrity check. The deep page-and-index checker is M6-e/M7
// work; until then this confirms the file opened and every catalogued collection can
// be scanned, exiting 4 if any scan fails (the corruption code CI keys on, spec §17).
func (a *app) subValidate() int {
	dbs, err := a.db.ListDatabaseNames(a.ctx(), emptyDoc())
	if err != nil {
		return reportTop(cliError{code: exitCorruption, msg: err.Error()})
	}
	scanned := 0
	for _, dbName := range dbs {
		colls, err := a.db.Database(dbName).ListCollectionNames(a.ctx(), emptyDoc())
		if err != nil {
			return reportTop(cliError{code: exitCorruption, msg: err.Error()})
		}
		for _, cn := range colls {
			cur, err := a.db.Database(dbName).Collection(cn).Find(a.ctx(), emptyDoc())
			if err != nil {
				return reportTop(cliError{code: exitCorruption, msg: dbName + "." + cn + ": " + err.Error()})
			}
			for cur.Next(a.ctx()) {
				_ = cur.Current()
			}
			if err := cur.Err(); err != nil {
				_ = cur.Close(a.ctx())
				return reportTop(cliError{code: exitCorruption, msg: dbName + "." + cn + ": " + err.Error()})
			}
			_ = cur.Close(a.ctx())
			scanned++
		}
	}
	_, _ = fmt.Fprintf(os.Stdout, "ok: %d collections scanned, no corruption found\n", scanned)
	return exitOK
}
