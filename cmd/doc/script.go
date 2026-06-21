package main

import (
	"bufio"
	"io"
	"os"
	"strings"
)

// runScript reads commands from r and executes them in sequence, accumulating
// multi-line input the same way the interactive shell does. On an error it prints the
// message; with stopOnError it returns the error (and its exit code) immediately,
// otherwise it continues to the next command (spec 2061 doc 15 §13.2).
func (a *app) runScript(r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var acc strings.Builder
	var firstErr error
	for sc.Scan() {
		line := sc.Text()
		if acc.Len() == 0 && strings.HasPrefix(strings.TrimSpace(line), ".") {
			// Dot-commands are always single-line.
			if err := a.handleScriptLine(line, &firstErr); err != nil {
				return err
			}
			continue
		}
		if acc.Len() > 0 {
			acc.WriteByte('\n')
		}
		acc.WriteString(line)
		if !balanced(acc.String()) {
			continue
		}
		cmd := acc.String()
		acc.Reset()
		if err := a.handleScriptLine(cmd, &firstErr); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return cliError{code: exitIOError, msg: err.Error()}
	}
	if rem := strings.TrimSpace(acc.String()); rem != "" {
		if err := a.handleScriptLine(rem, &firstErr); err != nil {
			return err
		}
	}
	return firstErr
}

// handleScriptLine runs one command in script context. It returns a non-nil error only
// when execution should stop: errQuit, or any error under stopOnError. Otherwise it
// records the first error (for the final exit code) and prints it.
func (a *app) handleScriptLine(line string, firstErr *error) error {
	err := a.dispatch(line)
	if err == nil {
		return nil
	}
	if err == errQuit {
		return errQuit
	}
	a.printError(err)
	if a.cfg.stopOnError {
		return err
	}
	if *firstErr == nil {
		*firstErr = err
	}
	return nil
}

// runScriptFile executes a script file referenced by .read or --file. "-" reads stdin.
func (a *app) runScriptFile(path string) error {
	if path == "-" {
		return a.runScript(os.Stdin)
	}
	f, err := os.Open(path)
	if err != nil {
		return openError(err.Error())
	}
	defer func() { _ = f.Close() }()
	return a.runScript(f)
}

// balanced reports whether every brace, bracket, and paren in s outside of a string is
// matched, which is how the shell decides a multi-line command is complete (spec §3.3).
func balanced(s string) bool {
	depth := 0
	inStr := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch c {
			case '\\':
				i++
			case '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[', '(':
			depth++
		case '}', ']', ')':
			depth--
		}
	}
	return depth <= 0
}
