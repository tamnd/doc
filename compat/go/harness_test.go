package compat

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// client is the shared driver connection to the doc serve instance booted in TestMain.
var client *mongo.Client

// listenRE pulls the bound address out of the line `doc serve` prints once it is ready:
//
//	doc serve listening on mongodb://127.0.0.1:59215
var listenRE = regexp.MustCompile(`mongodb://([0-9.]+:[0-9]+)`)

// TestMain builds the doc binary, starts `doc serve` on an ephemeral loopback port over an
// in-memory database, connects the official MongoDB Go driver to it, runs the suite, and tears
// everything down. This mirrors the conformance runner in spec 2061 doc 19 appendix G: start a
// doc serve instance, connect the driver under test, run the suite, shut it down.
func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	bin, cleanupBin, err := buildDoc()
	if err != nil {
		fmt.Fprintln(os.Stderr, "build doc:", err)
		return 1
	}
	defer cleanupBin()

	addr, stop, err := startServe(bin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start doc serve:", err)
		return 1
	}
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	uri := "mongodb://" + addr + "/?directConnection=true"
	c, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		fmt.Fprintln(os.Stderr, "driver connect:", err)
		return 1
	}
	if err := c.Ping(ctx, nil); err != nil {
		fmt.Fprintln(os.Stderr, "driver ping:", err)
		return 1
	}
	client = c
	defer func() { _ = c.Disconnect(context.Background()) }()

	return m.Run()
}

// buildDoc compiles the doc command into a temporary binary. The path can be overridden with
// DOC_BIN to reuse a prebuilt binary (for example one a release pipeline already produced),
// in which case nothing is built.
func buildDoc() (string, func(), error) {
	if pre := os.Getenv("DOC_BIN"); pre != "" {
		return pre, func() {}, nil
	}
	dir, err := os.MkdirTemp("", "doc-compat")
	if err != nil {
		return "", nil, err
	}
	bin := filepath.Join(dir, "doc")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/tamnd/doc/cmd/doc")
	cmd.Dir = repoRoot()
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	return bin, func() { _ = os.RemoveAll(dir) }, nil
}

// startServe launches `doc serve` on an in-memory database bound to an ephemeral loopback
// port, then reads its stderr until it announces the bound address. It returns that address
// and a stop function that signals the process and waits for it to exit.
func startServe(bin string) (string, func(), error) {
	cmd := exec.Command(bin, ":memory:", "serve", "--bind", "127.0.0.1", "--port", "0")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", nil, err
	}
	if err := cmd.Start(); err != nil {
		return "", nil, err
	}

	addrCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			line := sc.Text()
			if m := listenRE.FindStringSubmatch(line); m != nil {
				addrCh <- m[1]
			}
			// Keep draining after the address so the pipe never blocks the server.
			if !strings.Contains(line, "listening on") {
				continue
			}
		}
	}()

	stop := func() {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _, _ = cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
		}
	}

	select {
	case addr := <-addrCh:
		return addr, stop, nil
	case <-time.After(15 * time.Second):
		stop()
		return "", nil, fmt.Errorf("doc serve never announced a listening address")
	}
}

// repoRoot returns the doc repository root, two directories above this package (compat/go).
func repoRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

// coll returns a handle to a fresh, empty collection in the compat database, dropping any
// leftover from an earlier run so each test starts clean.
func coll(t *testing.T, name string) *mongo.Collection {
	t.Helper()
	c := client.Database("compat").Collection(name)
	if err := c.Drop(context.Background()); err != nil {
		t.Fatalf("drop %s: %v", name, err)
	}
	return c
}

// ctxFor returns a context with a per-test deadline so a hung driver call fails the test
// rather than the whole binary.
func ctxFor(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}
