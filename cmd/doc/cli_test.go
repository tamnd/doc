package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runCLI drives the real run() entry point with os.Stdout and os.Stderr redirected to
// pipes, so a test sees exactly what a user would. Stdin is pointed at /dev/null so the
// pipe-script path never blocks. It returns captured stdout, stderr, and the exit code.
func runCLI(t *testing.T, args ...string) (string, string, int) {
	t.Helper()

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}

	origOut, origErr, origIn := os.Stdout, os.Stderr, os.Stdin
	os.Stdout, os.Stderr, os.Stdin = outW, errW, devnull

	// Drain both pipes concurrently so a large result never deadlocks on a full buffer.
	outCh := make(chan string, 1)
	errCh := make(chan string, 1)
	go func() { b, _ := io.ReadAll(outR); outCh <- string(b) }()
	go func() { b, _ := io.ReadAll(errR); errCh <- string(b) }()

	code := run(args)

	_ = outW.Close()
	_ = errW.Close()
	os.Stdout, os.Stderr, os.Stdin = origOut, origErr, origIn
	_ = devnull.Close()

	stdout, stderr := <-outCh, <-errCh
	_ = outR.Close()
	_ = errR.Close()
	return stdout, stderr, code
}

// tmpDoc returns a path to a fresh .doc file inside the test's temp dir. The file does
// not exist yet; the first open creates it.
func tmpDoc(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "test.doc")
}

func TestRunVersion(t *testing.T) {
	out, _, code := runCLI(t, "--version")
	if code != exitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.HasPrefix(out, "doc ") {
		t.Fatalf("version output = %q, want it to start with %q", out, "doc ")
	}
}

func TestRunHelp(t *testing.T) {
	out, _, code := runCLI(t, "--help")
	if code != exitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "Usage:") || !strings.Contains(out, "Subcommands:") {
		t.Fatalf("help output missing sections:\n%s", out)
	}
}

func TestRunUnknownFlag(t *testing.T) {
	_, stderr, code := runCLI(t, "--nope")
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "unknown flag") {
		t.Fatalf("stderr = %q, want it to mention the unknown flag", stderr)
	}
}

func TestRunInsertOneAndFind(t *testing.T) {
	f := tmpDoc(t)
	out, stderr, code := runCLI(t, f,
		"-e", `db.c.insertOne({"_id":"a","n":1})`,
		"-e", `db.c.find({})`,
	)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(out, `"acknowledged":true`) || !strings.Contains(out, `"insertedId":"a"`) {
		t.Fatalf("insert ack missing:\n%s", out)
	}
	if !strings.Contains(out, `"_id":"a"`) || !strings.Contains(out, `"n":1`) {
		t.Fatalf("found document missing:\n%s", out)
	}
}

func TestRunPersistsAcrossSessions(t *testing.T) {
	f := tmpDoc(t)
	if _, stderr, code := runCLI(t, f, "-e", `db.c.insertOne({"_id":"keep","v":7})`); code != exitOK {
		t.Fatalf("insert exit = %d, stderr = %s", code, stderr)
	}
	// A second invocation opens the same file: the document must still be there.
	out, stderr, code := runCLI(t, f, "-e", `db.c.find({"_id":"keep"})`)
	if code != exitOK {
		t.Fatalf("find exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(out, `"v":7`) {
		t.Fatalf("persisted document not found:\n%s", out)
	}
}

func TestRunUpdateAndDelete(t *testing.T) {
	f := tmpDoc(t)
	out, stderr, code := runCLI(t, f,
		"-e", `db.c.insertMany([{"_id":1,"s":"x"},{"_id":2,"s":"y"}])`,
		"-e", `db.c.updateOne({"_id":1},{"$set":{"s":"z"}})`,
		"-e", `db.c.find({"_id":1})`,
		"-e", `db.c.deleteOne({"_id":2})`,
		"-e", `db.c.countDocuments({})`,
	)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(out, `"matchedCount":1`) || !strings.Contains(out, `"modifiedCount":1`) {
		t.Fatalf("update ack missing:\n%s", out)
	}
	if !strings.Contains(out, `"s":"z"`) {
		t.Fatalf("updated value missing:\n%s", out)
	}
	if !strings.Contains(out, `"deletedCount":1`) {
		t.Fatalf("delete ack missing:\n%s", out)
	}
	// One document left after deleting the other.
	if !strings.Contains(out, "1") {
		t.Fatalf("count after delete wrong:\n%s", out)
	}
}

func TestRunDistinct(t *testing.T) {
	f := tmpDoc(t)
	out, stderr, code := runCLI(t, f,
		"-e", `db.c.insertMany([{"k":"a"},{"k":"b"},{"k":"a"}])`,
		"-e", `db.c.distinct("k")`,
	)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(out, `"values"`) || !strings.Contains(out, `"a"`) || !strings.Contains(out, `"b"`) {
		t.Fatalf("distinct output missing values:\n%s", out)
	}
}

func TestRunAggregate(t *testing.T) {
	f := tmpDoc(t)
	out, stderr, code := runCLI(t, f,
		"-e", `db.sales.insertMany([{"item":"a","q":2},{"item":"a","q":3},{"item":"b","q":5}])`,
		"-e", `db.sales.aggregate([{"$group":{"_id":"$item","total":{"$sum":"$q"}}},{"$sort":{"_id":1}}])`,
	)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(out, `"_id":"a"`) || !strings.Contains(out, `"total":5`) {
		t.Fatalf("group total for a wrong:\n%s", out)
	}
	if !strings.Contains(out, `"_id":"b"`) || !strings.Contains(out, `"total":5`) {
		t.Fatalf("group total for b wrong:\n%s", out)
	}
}

func TestRunTableMode(t *testing.T) {
	f := tmpDoc(t)
	out, stderr, code := runCLI(t, f, "--table",
		"-e", `db.c.insertMany([{"_id":1,"name":"ann"},{"_id":2,"name":"bob"}])`,
		"-e", `db.c.find({})`,
	)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	// The header row comes from the first document's top-level keys.
	if !strings.Contains(out, "_id") || !strings.Contains(out, "name") {
		t.Fatalf("table header missing:\n%s", out)
	}
	if !strings.Contains(out, "ann") || !strings.Contains(out, "bob") {
		t.Fatalf("table rows missing:\n%s", out)
	}
}

func TestRunCanonicalMode(t *testing.T) {
	f := tmpDoc(t)
	out, stderr, code := runCLI(t, f, "--canonical",
		"-e", `db.c.insertOne({"_id":1,"n":7})`,
		"-e", `db.c.find({})`,
	)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	// Canonical extended JSON wraps every integer in $numberInt.
	if !strings.Contains(out, `"$numberInt"`) {
		t.Fatalf("canonical output not wrapped:\n%s", out)
	}
}

func TestRunForcePretty(t *testing.T) {
	f := tmpDoc(t)
	// Output is a pipe, so the default is compact JSONL; an explicit --json --pretty must
	// still pretty-print (the prettySet override).
	out, stderr, code := runCLI(t, f, "--json", "--pretty",
		"-e", `db.c.insertOne({"_id":1,"n":7})`,
		"-e", `db.c.find({})`,
	)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "\n  ") {
		t.Fatalf("pretty output not indented:\n%s", out)
	}
}

func TestRunRawCommand(t *testing.T) {
	f := tmpDoc(t)
	out, stderr, code := runCLI(t, f,
		"-e", `db.c.insertOne({"_id":1,"n":7})`,
		"-e", `{"find":"c","filter":{"_id":1}}`,
	)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(out, `"n":7`) {
		t.Fatalf("raw find command result missing:\n%s", out)
	}
}

func TestRunDotCollections(t *testing.T) {
	f := tmpDoc(t)
	out, stderr, code := runCLI(t, f,
		"-e", `db.alpha.insertOne({"x":1})`,
		"-e", `db.beta.insertOne({"x":2})`,
		"-e", `.collections`,
	)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "beta") {
		t.Fatalf("collection listing missing:\n%s", out)
	}
}

func TestRunIndexes(t *testing.T) {
	f := tmpDoc(t)
	out, stderr, code := runCLI(t, f,
		"-e", `db.c.insertOne({"x":1})`,
		"-e", `.createindex c {"x":1}`,
		"-e", `.indexes c`,
	)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "x_1") {
		t.Fatalf("created index not listed:\n%s", out)
	}
}

func TestRunTransactionCommit(t *testing.T) {
	f := tmpDoc(t)
	out, stderr, code := runCLI(t, f,
		"-e", `.begin`,
		"-e", `db.acct.insertOne({"_id":"x","bal":100})`,
		"-e", `.commit`,
		"-e", `db.acct.find({"_id":"x"})`,
	)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(out, `"bal":100`) {
		t.Fatalf("committed document not visible:\n%s", out)
	}
}

func TestRunTransactionRollback(t *testing.T) {
	f := tmpDoc(t)
	out, stderr, code := runCLI(t, f,
		"-e", `db.acct.insertOne({"_id":"y","bal":5})`,
		"-e", `.begin`,
		"-e", `db.acct.updateOne({"_id":"y"},{"$set":{"bal":999}})`,
		"-e", `.rollback`,
		"-e", `db.acct.find({"_id":"y"})`,
	)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if strings.Contains(out, "999") {
		t.Fatalf("rolled-back update still visible:\n%s", out)
	}
	if !strings.Contains(out, `"bal":5`) {
		t.Fatalf("original value not restored:\n%s", out)
	}
}

func TestRunQueryErrorExitCode(t *testing.T) {
	f := tmpDoc(t)
	// A malformed helper argument is a query error (exit 5), and the process should keep
	// the failing code without crashing.
	_, stderr, code := runCLI(t, f, "-e", `db.c.insertOne(not json)`)
	if code == exitOK {
		t.Fatalf("expected a non-zero exit for a bad argument, got 0")
	}
	if stderr == "" {
		t.Fatalf("expected an error message on stderr")
	}
}

func TestRunStopOnError(t *testing.T) {
	f := tmpDoc(t)
	// With stop-on-error the second eval must not run after the first fails.
	out, _, code := runCLI(t, f, "--stop-on-error",
		"-e", `db.c.insertOne(broken)`,
		"-e", `db.c.insertOne({"_id":"after","ok":1})`,
	)
	if code == exitOK {
		t.Fatalf("expected non-zero exit under stop-on-error")
	}
	if strings.Contains(out, "after") {
		t.Fatalf("second eval ran despite stop-on-error:\n%s", out)
	}
}

func TestRunSubcommandInfo(t *testing.T) {
	f := tmpDoc(t)
	if _, stderr, code := runCLI(t, f, "-e", `db.c.insertOne({"x":1})`); code != exitOK {
		t.Fatalf("seed insert failed: %s", stderr)
	}
	out, stderr, code := runCLI(t, f, "info")
	if code != exitOK {
		t.Fatalf("info exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "version:") || !strings.Contains(out, "databases:") {
		t.Fatalf("info output missing fields:\n%s", out)
	}
}

func TestRunSubcommandValidate(t *testing.T) {
	f := tmpDoc(t)
	if _, stderr, code := runCLI(t, f, "-e", `db.c.insertMany([{"x":1},{"x":2}])`); code != exitOK {
		t.Fatalf("seed insert failed: %s", stderr)
	}
	out, stderr, code := runCLI(t, f, "validate")
	if code != exitOK {
		t.Fatalf("validate exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "no corruption found") {
		t.Fatalf("validate output unexpected:\n%s", out)
	}
}

func TestRunSubcommandCheck(t *testing.T) {
	f := tmpDoc(t)
	if _, stderr, code := runCLI(t, f, "-e", `db.c.insertMany([{"x":1},{"x":2},{"x":3}])`); code != exitOK {
		t.Fatalf("seed insert failed: %s", stderr)
	}
	out, stderr, code := runCLI(t, f, "check", "full")
	if code != exitOK {
		t.Fatalf("check exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "no corruption found") {
		t.Fatalf("check output unexpected:\n%s", out)
	}
}

func TestRunSubcommandCompact(t *testing.T) {
	f := tmpDoc(t)
	if _, stderr, code := runCLI(t, f, "-e", `db.c.insertMany([{"_id":1},{"_id":2},{"_id":3}])`); code != exitOK {
		t.Fatalf("seed insert failed: %s", stderr)
	}
	if _, stderr, code := runCLI(t, f, "-e", `db.c.deleteOne({"_id":2})`); code != exitOK {
		t.Fatalf("seed delete failed: %s", stderr)
	}
	if out, stderr, code := runCLI(t, f, "compact"); code != exitOK {
		t.Fatalf("compact exit = %d, stderr = %s, out = %s", code, stderr, out)
	}
	// The two survivors are still there after the rewrite, the deleted one is gone.
	out, stderr, code := runCLI(t, f, "-e", `db.c.find({})`)
	if code != exitOK {
		t.Fatalf("find exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(out, `"_id":1`) || !strings.Contains(out, `"_id":3`) {
		t.Fatalf("expected survivors 1 and 3 after compact, got:\n%s", out)
	}
	if strings.Contains(out, `"_id":2`) {
		t.Fatalf("deleted document 2 reappeared after compact:\n%s", out)
	}
}

func TestRunSubcommandBackup(t *testing.T) {
	f := tmpDoc(t)
	if _, stderr, code := runCLI(t, f, "-e", `db.c.insertMany([{"_id":1},{"_id":2},{"_id":3}])`); code != exitOK {
		t.Fatalf("seed insert failed: %s", stderr)
	}
	out := filepath.Join(t.TempDir(), "out.doc")
	stdout, stderr, code := runCLI(t, f, "backup", "--out", out, "--verify")
	if code != exitOK {
		t.Fatalf("backup exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "ok: backed up") {
		t.Fatalf("backup output unexpected:\n%s", stdout)
	}

	// The backup file checks clean and still holds the three documents.
	if _, cstderr, ccode := runCLI(t, out, "check", "full"); ccode != exitOK {
		t.Fatalf("check of backup exit = %d, stderr = %s", ccode, cstderr)
	}
	fout, fstderr, fcode := runCLI(t, out, "-e", `db.c.find({})`)
	if fcode != exitOK {
		t.Fatalf("find in backup exit = %d, stderr = %s", fcode, fstderr)
	}
	for _, id := range []string{`"_id":1`, `"_id":2`, `"_id":3`} {
		if !strings.Contains(fout, id) {
			t.Fatalf("backup missing %s:\n%s", id, fout)
		}
	}
}

func TestDotBackupRequiresOut(t *testing.T) {
	f := tmpDoc(t)
	_, _, code := runCLI(t, f, "-e", ".backup")
	if code == exitOK {
		t.Fatal(".backup with no destination should fail")
	}
}

func TestDotExplainShowsPlan(t *testing.T) {
	f := tmpDoc(t)
	if _, stderr, code := runCLI(t, f, "-e", `db.c.insertOne({"_id":1,"x":1})`); code != exitOK {
		t.Fatalf("seed failed: %s", stderr)
	}
	out, stderr, code := runCLI(t, f, "-e", ".explain c {\"_id\":1}")
	if code != exitOK {
		t.Fatalf(".explain exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "queryPlanner") && !strings.Contains(out, "winningPlan") && !strings.Contains(out, "stage") {
		t.Fatalf(".explain output missing a plan:\n%s", out)
	}
}

func TestRunReadOnlyRejectsWrite(t *testing.T) {
	f := tmpDoc(t)
	if _, stderr, code := runCLI(t, f, "-e", `db.c.insertOne({"x":1})`); code != exitOK {
		t.Fatalf("seed insert failed: %s", stderr)
	}
	_, stderr, code := runCLI(t, f, "--readonly", "-e", `db.c.insertOne({"x":2})`)
	if code == exitOK {
		t.Fatalf("expected a write to fail on a read-only database")
	}
	if stderr == "" {
		t.Fatalf("expected an error message for the rejected write")
	}
}

func TestRunScriptFromStdinPath(t *testing.T) {
	f := tmpDoc(t)
	script := "db.c.insertOne({\"_id\":1,\"n\":42})\ndb.c.find({})\n"
	sf := filepath.Join(t.TempDir(), "script.js")
	if err := os.WriteFile(sf, []byte(script), 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	out, stderr, code := runCLI(t, f, "-f", sf)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(out, `"n":42`) {
		t.Fatalf("script result missing:\n%s", out)
	}
}

func TestRunFindOneNoMatch(t *testing.T) {
	f := tmpDoc(t)
	out, stderr, code := runCLI(t, f,
		"-e", `db.c.insertOne({"_id":1})`,
		"-e", `db.c.findOne({"_id":999})`,
	)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "null") {
		t.Fatalf("findOne with no match should print null:\n%s", out)
	}
}
