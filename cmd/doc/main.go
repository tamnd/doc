package main

import (
	"errors"
	"fmt"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run is the testable entry point: it parses arguments, resolves the output defaults
// from whether stdout is a terminal, and routes to the subcommand, the non-interactive
// eval or script path, or the interactive shell. It returns the process exit code.
func run(args []string) int {
	cfg, done, err := parseArgs(args)
	if err != nil {
		return reportTop(err)
	}
	if done {
		return exitOK
	}

	tty := isTerminal(os.Stdout)
	resolveDefaults(cfg, tty)

	// A bare `doc version` works without opening any file. Release builds carry the
	// commit and build date stamped by the linker; a plain `go build` omits them.
	if cfg.subcommand == "version" {
		fmt.Println(versionLine())
		if c, d := buildDetails(); c != "" || d != "" {
			fmt.Printf("commit %s\nbuilt %s\n", c, d)
		}
		return exitOK
	}
	if cfg.host != "" {
		return reportTop(cliError{code: exitUsage, msg: "remote mode (--host) arrives with the wire server in M8"})
	}

	a, err := newApp(cfg, os.Stdout)
	if err != nil {
		return reportTop(err)
	}
	defer a.close()

	switch {
	case cfg.subcommand != "":
		return a.runSubcommand()
	case len(cfg.evals) > 0:
		return a.runEvals()
	case cfg.script != "":
		if err := a.runScriptFile(cfg.script); err != nil && err != errQuit {
			return reportTop(err)
		}
		return exitOK
	case !tty:
		// Stdin is a pipe with no explicit script: read it as a script.
		if err := a.runScript(os.Stdin); err != nil && err != errQuit {
			return reportTop(err)
		}
		return exitOK
	default:
		if err := a.runShell(); err != nil {
			return reportTop(err)
		}
		return exitOK
	}
}

// resolveDefaults fills in the output mode and pretty flag that depend on whether
// stdout is a terminal, unless an explicit flag already set them (spec §2.2).
func resolveDefaults(cfg *config, tty bool) {
	if !cfg.modeSet {
		if tty {
			cfg.mode = modeJSON
		} else {
			cfg.mode = modeJSONL
		}
	}
	if !tty {
		if !cfg.prettySet {
			cfg.pretty = false
		}
		cfg.color = false
	}
}

// runEvals executes each -e expression in order, exiting with the code of the last one
// that failed (or the first under stop-on-error).
func (a *app) runEvals() int {
	code := exitOK
	for _, e := range a.cfg.evals {
		err := a.dispatch(e)
		if err == errQuit {
			break
		}
		if err != nil {
			a.printError(err)
			code = codeOf(err)
			if a.cfg.stopOnError {
				return code
			}
		}
	}
	return code
}

// reportTop prints a top-level error to stderr and returns its exit code.
func reportTop(err error) int {
	if err == nil || err == errQuit {
		return exitOK
	}
	fmt.Fprintln(os.Stderr, "Error: "+err.Error())
	return codeOf(err)
}

// codeOf extracts the exit code carried by a cliError, defaulting to the generic query
// error code for a plain error.
func codeOf(err error) int {
	var ce cliError
	if errors.As(err, &ce) {
		return ce.code
	}
	return exitQueryError
}

// isTerminal reports whether f is a character device (a terminal) rather than a pipe or
// regular file. This is the zero-dependency check the CLI uses to pick its default
// output format and whether to colorize.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
