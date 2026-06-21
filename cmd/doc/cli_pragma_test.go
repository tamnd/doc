package main

import (
	"strings"
	"testing"
)

func TestRunPragmaListsAll(t *testing.T) {
	f := tmpDoc(t)
	out, stderr, code := runCLI(t, f, "-e", `.pragma`)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	for _, want := range []string{"synchronous = normal", "default_isolation = snapshot", "journal_mode = wal"} {
		if !strings.Contains(out, want) {
			t.Fatalf(".pragma listing missing %q:\n%s", want, out)
		}
	}
}

func TestRunPragmaReadOne(t *testing.T) {
	f := tmpDoc(t)
	out, stderr, code := runCLI(t, f, "-e", `.pragma synchronous`)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "synchronous = normal") {
		t.Fatalf(".pragma read wrong:\n%s", out)
	}
}

func TestRunPragmaWrite(t *testing.T) {
	f := tmpDoc(t)
	out, stderr, code := runCLI(t, f,
		"-e", `.pragma synchronous=off`,
		"-e", `.pragma synchronous`,
	)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if strings.Count(out, "synchronous = off") != 2 {
		t.Fatalf(".pragma write did not stick:\n%s", out)
	}
}

func TestRunPragmaReadOnlyFails(t *testing.T) {
	f := tmpDoc(t)
	_, stderr, code := runCLI(t, f, "-e", `.pragma page_size=4096`)
	if code == exitOK {
		t.Fatal(".pragma on a read-only knob should fail")
	}
	if !strings.Contains(stderr, "page_size") {
		t.Fatalf("error should name page_size:\n%s", stderr)
	}
}

func TestRunPragmaStartupFlag(t *testing.T) {
	f := tmpDoc(t)
	out, stderr, code := runCLI(t, f,
		"--pragma", "default_isolation=serializable",
		"-e", `.pragma default_isolation`,
	)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(out, "default_isolation = serializable") {
		t.Fatalf("--pragma startup flag not applied:\n%s", out)
	}
}

func TestRunPragmaStartupFlagBad(t *testing.T) {
	f := tmpDoc(t)
	_, stderr, code := runCLI(t, f, "--pragma", "nope=1", "-e", `db.c.find({})`)
	if code == exitOK {
		t.Fatal("a bad --pragma should fail the open")
	}
	if !strings.Contains(stderr, "nope") {
		t.Fatalf("error should name the bad pragma:\n%s", stderr)
	}
}
