package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/tamnd/doc"
	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/extjson"
)

// dotCheck runs a structural and consistency check over the open database and
// prints a report (spec 2061 doc 18 §7.3). With a "full" argument it also verifies
// every page checksum, the slower whole-file pass. It exits with the corruption
// code when the check finds a problem, which is the signal a script keys on.
func (a *app) dotCheck(args []string) error {
	full := false
	for _, arg := range args {
		if strings.EqualFold(arg, "full") {
			full = true
		}
	}
	rep, err := a.db.Check(a.ctx(), full)
	if err != nil {
		return classify(err)
	}
	return a.printCheckReport(rep)
}

// printCheckReport writes a check report as plain lines and returns a corruption
// error when the report is not clean, so the caller exits non-zero.
func (a *app) printCheckReport(rep *doc.CheckReport) error {
	for _, p := range rep.FileProblems {
		if err := a.rend.writeText("file: " + p); err != nil {
			return err
		}
	}
	for _, cc := range rep.Collections {
		head := fmt.Sprintf("%s: %d documents", cc.Namespace, cc.Documents)
		if err := a.rend.writeText(head); err != nil {
			return err
		}
		for _, p := range cc.HeapProblems {
			if err := a.rend.writeText("  heap: " + p); err != nil {
				return err
			}
		}
		for _, ix := range cc.Indexes {
			if len(ix.Problems) == 0 {
				continue
			}
			for _, p := range ix.Problems {
				if err := a.rend.writeText("  index " + ix.Name + ": " + p); err != nil {
					return err
				}
			}
		}
		for _, p := range cc.Problems {
			if err := a.rend.writeText("  " + p); err != nil {
				return err
			}
		}
	}
	if rep.Valid {
		return a.rend.writeText("ok: no corruption found")
	}
	return cliError{code: exitCorruption, msg: "check found corruption"}
}

// dotCompact rewrites the database into a fresh, hole-free file, reclaiming the
// space held by dead slots, superseded cells, and forwarding tombstones (spec 2061
// doc 18 §15.2). It is offline: no other command runs while it proceeds.
func (a *app) dotCompact(_ []string) error {
	if err := a.db.Compact(a.ctx()); err != nil {
		return classify(err)
	}
	return a.rend.writeText("ok: database compacted")
}

// dotCheckpoint folds the WAL into the main file without closing the database (spec
// 2061 doc 18 §15.4). The optional argument is a checkpoint mode (passive, full,
// restart, truncate); doc performs the same full checkpoint for every mode.
func (a *app) dotCheckpoint(args []string) error {
	mode := ""
	if len(args) > 0 {
		mode = args[0]
	}
	if err := a.db.Checkpoint(a.ctx(), mode); err != nil {
		return classify(err)
	}
	return a.rend.writeText("ok: checkpoint complete")
}

// dotVacuum reclaims trailing free pages to the operating system (spec 2061 doc 18
// §15.2). The optional argument bounds how many pages to reclaim; absent, it
// reclaims every trailing free page. It requires auto_vacuum to be enabled, matching
// the engine of PRAGMA incremental_vacuum.
func (a *app) dotVacuum(args []string) error {
	n := 0
	if len(args) > 0 {
		v, err := strconv.Atoi(args[0])
		if err != nil || v < 0 {
			return usageErr(".vacuum [pages]")
		}
		n = v
	}
	reclaimed, err := a.db.IncrementalVacuum(a.ctx(), n)
	if err != nil {
		return classify(err)
	}
	return a.rend.writeText(fmt.Sprintf("ok: reclaimed %d pages", reclaimed))
}

// dotExplain prints the query plan for a find on a collection (spec 2061 doc 11 §9).
// The optional second argument is a filter in extended JSON; the optional third is a
// verbosity ("queryPlanner" or "executionStats"), defaulting to queryPlanner.
func (a *app) dotExplain(args []string) error {
	if len(args) < 1 {
		return usageErr(".explain <coll> [filter] [verbosity]")
	}
	filter := bson.NewBuilder().Build()
	verbosity := "queryPlanner"
	if len(args) > 1 {
		f, err := extjson.Parse([]byte(args[1]))
		if err != nil {
			return queryError(err.Error())
		}
		filter = f
	}
	if len(args) > 2 {
		verbosity = args[2]
	}
	plan, err := a.collection(args[0]).Explain(a.ctx(), filter, verbosity)
	if err != nil {
		return classify(err)
	}
	return a.rend.renderDoc(plan)
}
