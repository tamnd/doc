package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/tamnd/doc"
)

// outputMode is the wire shape of a command's result.
type outputMode int

const (
	modeJSON  outputMode = iota // pretty or compact extended JSON, one doc at a time
	modeJSONL                   // one compact JSON object per line
	modeTable                   // aligned ASCII columns
	modeBSON                    // raw BSON binary
)

// config holds everything parsed from the command line and the environment that the
// rest of the CLI reads. It is assembled once in parseArgs and then only the mutable
// display fields (mode, pretty, and the like) change as dot-commands run.
type config struct {
	file       string
	db         string
	readonly   bool
	cacheBytes int64
	sync       doc.SyncLevel
	pragmas    []string

	mode      outputMode
	modeSet   bool // an explicit format flag was given
	pretty    bool
	prettySet bool // an explicit --pretty or --no-pretty was given
	canonical bool
	headers   bool
	width     int
	limit     int64
	color     bool
	quiet     bool

	// evals are -e expressions to run in order; script is a -f path ("-" for stdin).
	evals  []string
	script string

	stopOnError bool
	force       bool
	noRC        bool

	// subcommand and its args, when the invocation is non-interactive by subcommand.
	subcommand string
	subArgs    []string

	host string // remote mode address; when set the CLI would speak the wire protocol
}

// parseArgs turns os.Args[1:] into a config, applying environment defaults first so an
// explicit flag always wins. It returns a non-nil error (a cliError carrying an exit
// code) on a usage problem, and handleExit true when the caller should print nothing
// further and exit zero (the --version and --help paths).
func parseArgs(args []string) (*config, bool, error) {
	c := &config{
		db:         envOr("DOC_DB", "default"),
		cacheBytes: 64 << 20,
		sync:       doc.SyncNormal,
		pretty:     true,
		headers:    true,
		color:      true,
	}
	c.file = os.Getenv("DOC_FILE")
	c.host = os.Getenv("DOC_HOST")
	if f := os.Getenv("DOC_FORMAT"); f != "" {
		if m, ok := parseMode(f); ok {
			c.mode, c.modeSet = m, true
		}
	}
	if cv := os.Getenv("DOC_CACHE"); cv != "" {
		if n, err := parseSize(cv); err == nil {
			c.cacheBytes = n
		}
	}
	if sv := os.Getenv("DOC_SYNC"); sv != "" {
		if l, ok := parseSync(sv); ok {
			c.sync = l
		}
	}
	if wv := os.Getenv("DOC_WIDTH"); wv != "" {
		if n, err := strconv.Atoi(wv); err == nil {
			c.width = n
		}
	}
	if os.Getenv("NO_COLOR") != "" {
		c.color = false
	}

	var positionals []string
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--":
			positionals = append(positionals, args[i+1:]...)
			i = len(args)
			continue
		case a == "--version" || a == "-v":
			fmt.Println(versionLine())
			return nil, true, nil
		case a == "--help" || a == "-h":
			fmt.Print(usageText)
			return nil, true, nil
		case a == "--eval" || a == "-e":
			v, ni, err := flagValue(args, i, a)
			if err != nil {
				return nil, false, err
			}
			c.evals = append(c.evals, v)
			i = ni
		case a == "--file" || a == "-f":
			v, ni, err := flagValue(args, i, a)
			if err != nil {
				return nil, false, err
			}
			c.script = v
			i = ni
		case a == "--db":
			v, ni, err := flagValue(args, i, a)
			if err != nil {
				return nil, false, err
			}
			c.db = v
			i = ni
		case a == "--cache":
			v, ni, err := flagValue(args, i, a)
			if err != nil {
				return nil, false, err
			}
			n, perr := parseSize(v)
			if perr != nil {
				return nil, false, usageError("bad --cache value: " + v)
			}
			c.cacheBytes = n
			i = ni
		case a == "--sync":
			v, ni, err := flagValue(args, i, a)
			if err != nil {
				return nil, false, err
			}
			l, ok := parseSync(v)
			if !ok {
				return nil, false, usageError("bad --sync level: " + v)
			}
			c.sync = l
			i = ni
		case a == "--pragma":
			v, ni, err := flagValue(args, i, a)
			if err != nil {
				return nil, false, err
			}
			c.pragmas = append(c.pragmas, v)
			i = ni
		case a == "--host":
			v, ni, err := flagValue(args, i, a)
			if err != nil {
				return nil, false, err
			}
			c.host = v
			i = ni
		case a == "--width":
			v, ni, err := flagValue(args, i, a)
			if err != nil {
				return nil, false, err
			}
			n, perr := strconv.Atoi(v)
			if perr != nil {
				return nil, false, usageError("bad --width value: " + v)
			}
			c.width = n
			i = ni
		case a == "--limit":
			v, ni, err := flagValue(args, i, a)
			if err != nil {
				return nil, false, err
			}
			n, perr := strconv.ParseInt(v, 10, 64)
			if perr != nil {
				return nil, false, usageError("bad --limit value: " + v)
			}
			c.limit = n
			i = ni
		case a == "--json":
			c.setMode(modeJSON)
			i++
		case a == "--jsonl":
			c.setMode(modeJSONL)
			i++
		case a == "--table":
			c.setMode(modeTable)
			i++
		case a == "--bson":
			c.setMode(modeBSON)
			i++
		case a == "--pretty":
			c.pretty, c.prettySet = true, true
			i++
		case a == "--no-pretty":
			c.pretty, c.prettySet = false, true
			i++
		case a == "--canonical":
			c.canonical = true
			i++
		case a == "--readonly" || a == "-r":
			c.readonly = true
			i++
		case a == "--quiet" || a == "-q":
			c.quiet = true
			i++
		case a == "--no-color":
			c.color = false
			i++
		case a == "--no-rc":
			c.noRC = true
			i++
		case a == "--force":
			c.force = true
			i++
		case a == "--stop-on-error":
			c.stopOnError = true
			i++
		case a == "--continue-on-error":
			c.stopOnError = false
			i++
		case strings.HasPrefix(a, "--") || (strings.HasPrefix(a, "-") && len(a) > 1 && a != "-"):
			return nil, false, usageError("unknown flag: " + a)
		default:
			positionals = append(positionals, a)
			i++
			// Once a subcommand is identified, hand the rest of the line to it
			// verbatim so its own --flags are not mistaken for global ones.
			if rest, ok := subcommandTail(positionals, args, i); ok {
				c.subArgs = rest
				i = len(args)
			}
		}
	}

	if c.mode == modeBSON {
		// BSON output is always canonical and never pretty (spec §14.4).
		c.canonical = true
		c.pretty = false
	}

	// Resolve positionals: [file] [subcommand [args...]]. When the scan above already
	// peeled the subcommand's own arguments into c.subArgs, only the name resolution
	// is left here.
	tailTaken := c.subArgs != nil
	if len(positionals) > 0 {
		if isSubcommand(positionals[0]) {
			c.subcommand = positionals[0]
			if !tailTaken {
				c.subArgs = positionals[1:]
			}
		} else {
			c.file = positionals[0]
			if len(positionals) > 1 {
				c.subcommand = positionals[1]
				if !tailTaken {
					c.subArgs = positionals[2:]
				}
			}
		}
	}
	return c, false, nil
}

// subcommandTail reports whether the positionals collected so far identify a
// subcommand whose remaining arguments (everything from args[i:]) should be passed
// through untouched. It fires the moment the subcommand word is seen, either as the
// first positional or as the second one following a file path.
func subcommandTail(positionals, args []string, i int) ([]string, bool) {
	if len(positionals) == 1 && isSubcommand(positionals[0]) {
		return append([]string{}, args[i:]...), true
	}
	if len(positionals) == 2 && !isSubcommand(positionals[0]) && isSubcommand(positionals[1]) {
		return append([]string{}, args[i:]...), true
	}
	return nil, false
}

// setMode records an explicit format flag and rejects a second, conflicting one.
func (c *config) setMode(m outputMode) {
	c.mode = m
	c.modeSet = true
}

// flagValue returns the value for a flag at args[i], whether written as "--flag v" or
// "--flag=v", and the next index to resume parsing from.
func flagValue(args []string, i int, name string) (string, int, error) {
	a := args[i]
	if _, val, ok := strings.Cut(a, "="); ok {
		return val, i + 1, nil
	}
	if i+1 >= len(args) {
		return "", i, usageError("flag needs a value: " + name)
	}
	return args[i+1], i + 2, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseMode(s string) (outputMode, bool) {
	switch strings.ToLower(s) {
	case "json":
		return modeJSON, true
	case "jsonl", "ndjson":
		return modeJSONL, true
	case "table":
		return modeTable, true
	case "bson":
		return modeBSON, true
	default:
		return modeJSON, false
	}
}

func parseSync(s string) (doc.SyncLevel, bool) {
	switch strings.ToUpper(s) {
	case "OFF":
		return doc.SyncOff, true
	case "NORMAL":
		return doc.SyncNormal, true
	case "FULL":
		return doc.SyncFull, true
	case "EXTRA":
		// The engine tops out at FULL (fsync on every commit). EXTRA, which the spec
		// lists for parity with SQLite, maps onto FULL until a stricter level exists.
		return doc.SyncFull, true
	default:
		return doc.SyncNormal, false
	}
}

// parseSize reads a byte count with an optional K, M, or G suffix (powers of 1024).
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'k', 'K':
		mult, s = 1<<10, s[:len(s)-1]
	case 'm', 'M':
		mult, s = 1<<20, s[:len(s)-1]
	case 'g', 'G':
		mult, s = 1<<30, s[:len(s)-1]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, err
	}
	return n * mult, nil
}
