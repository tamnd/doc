package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tamnd/doc"
)

// runShell runs the interactive read-eval-print loop. It prints the banner, then reads
// commands, accumulating multi-line input until braces balance, and dispatches each
// complete command. EOF (Ctrl-D) on an empty prompt exits cleanly (spec 2061 doc 15
// §3).
func (a *app) runShell() error {
	a.interactive = true
	if !a.cfg.quiet {
		a.printBanner()
	}
	hist := newHistory()
	in := bufio.NewReader(os.Stdin)

	var acc strings.Builder
	for {
		if !a.cfg.quiet {
			_, _ = fmt.Fprint(os.Stdout, a.prompt(acc.Len() > 0))
		}
		line, err := in.ReadString('\n')
		if err == io.EOF {
			if acc.Len() == 0 {
				_, _ = fmt.Fprintln(os.Stdout)
				break
			}
			// EOF mid-command: run what we have, then exit.
			line = strings.TrimRight(line, "\n")
		} else if err != nil {
			return cliError{code: exitIOError, msg: err.Error()}
		}
		line = strings.TrimRight(line, "\r\n")

		// A dot-command on a fresh prompt is always single-line.
		if acc.Len() == 0 && strings.HasPrefix(strings.TrimSpace(line), ".") {
			hist.add(line)
			if a.runOne(line) == errQuit {
				break
			}
			if err == io.EOF {
				break
			}
			continue
		}

		if acc.Len() > 0 {
			acc.WriteByte('\n')
		}
		acc.WriteString(line)
		if err != io.EOF && !balanced(acc.String()) {
			continue
		}
		cmd := strings.TrimSpace(acc.String())
		acc.Reset()
		if cmd != "" {
			hist.add(cmd)
			if a.runOne(cmd) == errQuit {
				break
			}
		}
		if err == io.EOF {
			break
		}
	}
	hist.flush()
	return nil
}

// runOne dispatches a command in interactive context and prints any error without
// aborting the loop. It returns errQuit when the user asked to leave.
func (a *app) runOne(line string) error {
	err := a.dispatch(line)
	if err == errQuit {
		return errQuit
	}
	if err != nil {
		a.printError(err)
	}
	return nil
}

func (a *app) printBanner() {
	target := displayPath(a.cfg.file)
	_, _ = fmt.Fprintf(os.Stdout, "doc %s  engine=btree  %s\n", doc.Version, target)
	_, _ = fmt.Fprintln(os.Stdout, "Connected. Type .help for help.")
}

// prompt is the line shown before input: the active database, a [session] tag when an
// explicit transaction is open, and a continuation form for multi-line input.
func (a *app) prompt(continuation bool) string {
	if continuation {
		return "... "
	}
	tag := ""
	if a.sess != nil {
		tag = " [session]"
	}
	return a.dbName + tag + "> "
}

// history appends accepted commands to the history file, capped at 2000 lines
// (spec §3.4). Without a raw-TTY line reader the shell does not navigate history
// interactively, but it still records a session for later inspection.
type history struct {
	path  string
	lines []string
}

func newHistory() *history {
	path := os.Getenv("DOC_HISTFILE")
	if path == "" {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, ".doc_history")
		}
	}
	h := &history{path: path}
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			h.lines = strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		}
	}
	return h
}

func (h *history) add(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	h.lines = append(h.lines, line)
}

func (h *history) flush() {
	if h.path == "" {
		return
	}
	if len(h.lines) > 2000 {
		h.lines = h.lines[len(h.lines)-2000:]
	}
	_ = os.WriteFile(h.path, []byte(strings.Join(h.lines, "\n")+"\n"), 0o600)
}
