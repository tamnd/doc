package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile drops content into a fresh file under the test temp dir and returns its path.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

func TestRunImportJSONL(t *testing.T) {
	f := tmpDoc(t)
	src := writeFile(t, "in.jsonl", "{\"_id\":1,\"n\":10}\n{\"_id\":2,\"n\":20}\n")
	out, stderr, code := runCLI(t, f,
		"-e", `.import `+src+` --collection orders`,
		"-e", `db.orders.countDocuments({})`,
	)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "imported 2, skipped 0 into default.orders") {
		t.Fatalf("import report missing:\n%s", out)
	}
	if !strings.Contains(out, "2") {
		t.Fatalf("count after import wrong:\n%s", out)
	}
}

func TestRunImportJSONArray(t *testing.T) {
	f := tmpDoc(t)
	src := writeFile(t, "in.json", `[{"_id":1},{"_id":2},{"_id":3}]`)
	out, stderr, code := runCLI(t, f,
		"-e", `.import `+src+` --collection c --format json`,
		"-e", `db.c.countDocuments({})`,
	)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "imported 3") {
		t.Fatalf("json-array import report missing:\n%s", out)
	}
}

func TestRunImportCSVWithHeader(t *testing.T) {
	f := tmpDoc(t)
	src := writeFile(t, "in.csv", "name,age\nann,30\nbob,41\n")
	out, stderr, code := runCLI(t, f,
		"-e", `.import `+src+` --collection people --format csv`,
		"-e", `db.people.find({"name":"ann"})`,
	)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	// age coerces to a number, name stays a string.
	if !strings.Contains(out, `"name":"ann"`) || !strings.Contains(out, `"age":30`) {
		t.Fatalf("csv import did not store typed cells:\n%s", out)
	}
}

func TestRunImportDropReplaces(t *testing.T) {
	f := tmpDoc(t)
	src := writeFile(t, "in.jsonl", "{\"_id\":9}\n")
	out, stderr, code := runCLI(t, f,
		"-e", `db.c.insertMany([{"_id":1},{"_id":2}])`,
		"-e", `.import `+src+` --collection c --drop`,
		"-e", `db.c.countDocuments({})`,
	)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	// The drop wipes the two seeded docs, leaving only the imported one.
	last := lastLine(out)
	if strings.TrimSpace(last) != "1" {
		t.Fatalf("count after --drop import = %q, want 1:\n%s", last, out)
	}
}

func TestRunExportJSONLRoundTrip(t *testing.T) {
	f := tmpDoc(t)
	dst := filepath.Join(t.TempDir(), "out.jsonl")
	if _, stderr, code := runCLI(t, f,
		"-e", `db.c.insertMany([{"_id":1,"v":"a"},{"_id":2,"v":"b"}])`,
		"-e", `.export `+dst+` --collection c`,
	); code != exitOK {
		t.Fatalf("export exit, stderr = %s", stderr)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("exported %d lines, want 2:\n%s", len(lines), data)
	}

	// Reimport into a fresh database file and confirm the documents survive.
	g := tmpDoc(t)
	out, stderr, code := runCLI(t, g,
		"-e", `.import `+dst+` --collection c`,
		"-e", `db.c.find({"_id":2})`,
	)
	if code != exitOK {
		t.Fatalf("reimport exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(out, `"v":"b"`) {
		t.Fatalf("round-tripped document missing:\n%s", out)
	}
}

func TestRunExportCSV(t *testing.T) {
	f := tmpDoc(t)
	dst := filepath.Join(t.TempDir(), "out.csv")
	if _, stderr, code := runCLI(t, f,
		"-e", `db.c.insertMany([{"name":"ann","age":30},{"name":"bob","age":41}])`,
		"-e", `.export `+dst+` --collection c --fields name,age`,
	); code != exitOK {
		t.Fatalf("export exit, stderr = %s", stderr)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read csv: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "name,age") {
		t.Fatalf("csv header missing:\n%s", text)
	}
	if !strings.Contains(text, "ann,30") || !strings.Contains(text, "bob,41") {
		t.Fatalf("csv rows missing:\n%s", text)
	}
}

func TestRunExportFilterAndSort(t *testing.T) {
	f := tmpDoc(t)
	dst := filepath.Join(t.TempDir(), "out.jsonl")
	if _, stderr, code := runCLI(t, f,
		"-e", `db.c.insertMany([{"_id":1,"k":3},{"_id":2,"k":1},{"_id":3,"k":2}])`,
		"-e", `.export `+dst+` --collection c --filter {"k":{"$gte":2}} --sort {"k":1}`,
	); code != exitOK {
		t.Fatalf("export exit, stderr = %s", stderr)
	}
	data, _ := os.ReadFile(dst)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("filtered export = %d lines, want 2:\n%s", len(lines), data)
	}
	// Sorted ascending by k: the first line is _id 3 (k=2), then _id 1 (k=3).
	if !strings.Contains(lines[0], `"_id":3`) {
		t.Fatalf("export not sorted by k:\n%s", data)
	}
}

func TestRunDumpAndLoadRoundTrip(t *testing.T) {
	f := tmpDoc(t)
	dir := t.TempDir()
	if _, stderr, code := runCLI(t, f,
		"-e", `db.orders.insertMany([{"_id":1,"sku":"a"},{"_id":2,"sku":"b"}])`,
		"-e", `.createindex orders {"sku":1}`,
		"-e", `.dump `+dir,
	); code != exitOK {
		t.Fatalf("dump exit, stderr = %s", stderr)
	}
	// The dump writes a bson stream and a metadata sidecar under <dir>/<db>.
	if _, err := os.Stat(filepath.Join(dir, "default", "orders.bson")); err != nil {
		t.Fatalf("dump bson missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "default", "orders.metadata.json")); err != nil {
		t.Fatalf("dump metadata missing: %v", err)
	}

	// Load into a fresh file and confirm both the data and the secondary index return.
	g := tmpDoc(t)
	out, stderr, code := runCLI(t, g,
		"-e", `.load `+dir,
		"-e", `db.orders.countDocuments({})`,
		"-e", `.indexes orders`,
	)
	if code != exitOK {
		t.Fatalf("load exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "sku_1") {
		t.Fatalf("rebuilt index not listed after load:\n%s", out)
	}
}

func TestRunImportSubcommand(t *testing.T) {
	f := tmpDoc(t)
	src := writeFile(t, "in.jsonl", "{\"_id\":1}\n{\"_id\":2}\n")
	out, stderr, code := runCLI(t, f, "import", src, "--collection", "c")
	if code != exitOK {
		t.Fatalf("import subcommand exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "imported 2") {
		t.Fatalf("import subcommand report missing:\n%s", out)
	}
}

// lastLine returns the final non-empty line of s.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}
