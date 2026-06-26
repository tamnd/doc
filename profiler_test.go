package doc

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestProfilerLevelPragma(t *testing.T) {
	db := openTestDB(t)
	if got, _ := db.Pragma("profile", ""); got != "0" {
		t.Fatalf("default profile level = %q, want 0", got)
	}
	if _, err := db.Pragma("profile", "2"); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.Pragma("profile", ""); got != "2" {
		t.Fatalf("profile level = %q, want 2", got)
	}
	if _, err := db.Pragma("profile", "5"); err == nil {
		t.Fatal("level 5 should be rejected")
	}
}

func TestProfileCommandReportsPrevious(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	d := db.Database("app")

	var res M
	if err := d.RunCommand(ctx, M{"profile": 1}).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if was, _ := res["was"].(int32); was != 0 {
		t.Fatalf("was = %v, want 0", res["was"])
	}
	// Reading with -1 must not change the level and must report level 1.
	if err := d.RunCommand(ctx, M{"profile": -1}).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if was, _ := res["was"].(int32); was != 1 {
		t.Fatalf("was = %v, want 1", res["was"])
	}
	if db.prof.Level() != 1 {
		t.Fatalf("level changed to %d on a read", db.prof.Level())
	}
}

func TestProfilerLevel2WritesSystemProfile(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	db.prof.SetLevel(2)
	coll := db.Database("app").Collection("orders")
	for i := 0; i < 3; i++ {
		if _, err := coll.InsertOne(ctx, M{"i": i}); err != nil {
			t.Fatal(err)
		}
	}

	prof := db.Database("app").Collection(systemProfileName)
	n, err := prof.CountDocuments(ctx, M{})
	if err != nil {
		t.Fatal(err)
	}
	if n < 3 {
		t.Fatalf("system.profile holds %d events, want >= 3", n)
	}
	// Profile writes must not themselves be profiled (no runaway recursion).
	var ev M
	if err := prof.FindOne(ctx, M{"op": "insert", "ns": "app.orders"}).Decode(&ev); err != nil {
		t.Fatalf("profile event for the insert is missing: %v", err)
	}
	if ev["ns"] != "app.orders" {
		t.Fatalf("event ns = %v, want app.orders", ev["ns"])
	}
}

func TestProfilerLevel0Silent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	coll := db.Database("app").Collection("c")
	if _, err := coll.InsertOne(ctx, M{"x": 1}); err != nil {
		t.Fatal(err)
	}
	// At level 0 the system.profile collection is never created.
	names, err := db.Database("app").ListCollectionNames(ctx, M{})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if n == systemProfileName {
			t.Fatal("system.profile created at profile level 0")
		}
	}
}

func TestSlowOpLogsWarn(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	db, err := Open(memoryPath,
		WithLogger(slog.New(h)),
		WithSlowOpThreshold(time.Nanosecond), // every op counts as slow
		WithProfileLevel(1),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	coll := db.Database("app").Collection("c")
	if _, err := coll.InsertOne(context.Background(), M{"x": 1}); err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	if !strings.Contains(out, `"msg":"slow operation"`) {
		t.Fatalf("no slow-op log line:\n%s", out)
	}
	// Walk the records and confirm the slow-op line is WARN with the right fields.
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if rec["msg"] != "slow operation" {
			continue
		}
		found = true
		if rec["level"] != "WARN" {
			t.Fatalf("slow op logged at %v, want WARN", rec["level"])
		}
		if rec["component"] != "COMMAND" {
			t.Fatalf("component = %v, want COMMAND", rec["component"])
		}
		if rec["ns"] != "app.c" {
			t.Fatalf("ns = %v, want app.c", rec["ns"])
		}
	}
	if !found {
		t.Fatal("slow operation record not parsed")
	}
}

func TestStartupLog(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	db, err := Open(memoryPath, WithLogger(slog.New(h)))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if !strings.Contains(buf.String(), `"msg":"database opened"`) {
		t.Fatalf("no startup log:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), `"component":"STORAGE"`) {
		t.Fatalf("startup log missing component:\n%s", buf.String())
	}
}
