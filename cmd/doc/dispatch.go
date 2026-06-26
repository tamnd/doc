package main

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/tamnd/doc/extjson"
)

// errQuit unwinds the shell loop when the user types .quit, exit, or sends EOF.
var errQuit = errors.New("quit")

// dispatch routes one complete command line to the right handler: a dot-command, an
// alias such as "use" or "show", the `it` cursor advance, a raw JSON command document,
// or a mongosh-style helper. It is shared by the shell and the script runner so both
// accept exactly the same syntax.
func (a *app) dispatch(line string) error {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "//") || strings.HasPrefix(line, "#") {
		return nil
	}

	if a.timing {
		// time.Now is fine here: this is the running CLI, not a workflow script.
		start := time.Now()
		defer func() { a.printTiming(time.Since(start)) }()
	}

	switch {
	case strings.HasPrefix(line, "."):
		return a.runDot(line)
	case line == "it":
		return a.advanceCursor()
	case isAlias(line):
		return a.runAlias(line)
	case strings.HasPrefix(line, "{"):
		cmd, err := extjson.Parse([]byte(line))
		if err != nil {
			return queryError(err.Error())
		}
		return a.runCommand(cmd)
	case strings.HasPrefix(line, "db."):
		hc, ok, err := parseHelper(line)
		if err != nil {
			return queryError(err.Error())
		}
		if !ok {
			return queryError("unrecognized command")
		}
		return a.runHelper(hc)
	default:
		return queryError("unrecognized command: " + line)
	}
}

// isAlias reports whether the line is one of the bare-word aliases (spec §4.5).
func isAlias(line string) bool {
	w := firstWord(line)
	switch w {
	case "show", "use", "exit", "quit", "help":
		return true
	}
	return false
}

// runAlias expands a bare-word alias to its dot-command equivalent.
func (a *app) runAlias(line string) error {
	fields := strings.Fields(line)
	switch fields[0] {
	case "show":
		if len(fields) < 2 {
			return usageErr("show dbs|collections|indexes")
		}
		switch fields[1] {
		case "dbs", "databases":
			return a.runDot(".databases")
		case "collections", "tables":
			return a.runDot(".collections")
		case "indexes":
			return a.runDot(".indexes")
		default:
			return usageErr("show dbs|collections|indexes")
		}
	case "use":
		if len(fields) < 2 {
			return usageErr("use <db>")
		}
		return a.runDot(".use " + fields[1])
	case "help":
		return a.runDot(".help")
	case "exit", "quit":
		return errQuit
	}
	return nil
}

func firstWord(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " \t("); i >= 0 {
		return s[:i]
	}
	return s
}

func (a *app) printTiming(d time.Duration) {
	_ = a.rend.writeText("Elapsed: " + formatDuration(d))
}

func formatDuration(d time.Duration) string {
	ms := float64(d.Microseconds()) / 1000.0
	return strconv.FormatFloat(ms, 'f', 1, 64) + " ms"
}
