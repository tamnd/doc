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
	case "check", "validate":
		return a.subCheck()
	case "compact":
		if err := a.dotCompact(a.cfg.subArgs); err != nil {
			return reportTop(err)
		}
		return exitOK
	case "checkpoint":
		if err := a.dotCheckpoint(a.cfg.subArgs); err != nil {
			return reportTop(err)
		}
		return exitOK
	case "vacuum":
		if err := a.dotVacuum(a.cfg.subArgs); err != nil {
			return reportTop(err)
		}
		return exitOK
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
	case "import":
		return reportTop(a.dotImport(a.cfg.subArgs))
	case "export":
		return reportTop(a.dotExport(a.cfg.subArgs))
	case "dump":
		return reportTop(a.dotDump(a.cfg.subArgs))
	case "load":
		return reportTop(a.dotLoad(a.cfg.subArgs))
	case "backup":
		if err := a.dotBackup(a.cfg.subArgs); err != nil {
			return reportTop(err)
		}
		return exitOK
	case "restore":
		if err := a.dotRestore(a.cfg.subArgs); err != nil {
			return reportTop(err)
		}
		return exitOK
	case "reindex":
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

// subCheck runs the deep structural and consistency check over the file and prints
// its report (spec 2061 doc 18 §7.3). It exits 4 when the check finds corruption, the
// code CI keys on (spec §17). A "full" argument adds the whole-file checksum pass.
func (a *app) subCheck() int {
	if err := a.dotCheck(a.cfg.subArgs); err != nil {
		return reportTop(err)
	}
	return exitOK
}
