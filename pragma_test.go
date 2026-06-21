package doc

import (
	"context"
	"testing"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/collection"
)

func TestPragmaReadDefaults(t *testing.T) {
	db := openTestDB(t)
	cases := map[string]string{
		"synchronous":       "full",
		"default_isolation": "snapshot",
		"journal_mode":      "wal",
		"read_only":         "false",
	}
	for name, want := range cases {
		got, err := db.Pragma(name, "")
		if err != nil {
			t.Fatalf("Pragma(%q): %v", name, err)
		}
		if got != want {
			t.Errorf("Pragma(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestPragmaWriteSynchronous(t *testing.T) {
	db := openTestDB(t)
	got, err := db.Pragma("synchronous", "off")
	if err != nil {
		t.Fatalf("write synchronous: %v", err)
	}
	if got != "off" {
		t.Fatalf("synchronous after write = %q, want off", got)
	}
	if got := db.eng.SyncLevel(); got != 0 {
		t.Fatalf("engine sync level = %v, want SyncOff", got)
	}
	// extra folds onto full because the engine has no separate extra barrier.
	if got, _ := db.Pragma("synchronous", "extra"); got != "full" {
		t.Fatalf("synchronous=extra read back as %q, want full", got)
	}
}

func TestPragmaWriteDefaultIsolation(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Pragma("default_isolation", "serializable"); err != nil {
		t.Fatalf("write default_isolation: %v", err)
	}
	if got := db.isolationDefault().level(); got != collection.Serializable {
		t.Fatalf("default isolation = %v, want Serializable", got)
	}
	// A session started after the write picks up the new default.
	sess, err := db.StartSession()
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer sess.EndSession(context.Background())
	if err := sess.StartTransaction(); err != nil {
		t.Fatalf("StartTransaction: %v", err)
	}
}

func TestPragmaReadOnlyKnobRejectsWrite(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Pragma("page_size", "4096"); err == nil {
		t.Fatal("writing page_size should fail, it is create-time")
	}
}

func TestPragmaUnknownNameRejected(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Pragma("encryption_key", ""); err == nil {
		t.Fatal("unknown PRAGMA should fail rather than read empty")
	}
	if _, err := db.Pragma("synchronous", "loud"); err == nil {
		t.Fatal("invalid synchronous value should fail")
	}
}

func TestPragmaNamesSorted(t *testing.T) {
	names := PragmaNames()
	if len(names) == 0 {
		t.Fatal("PragmaNames returned nothing")
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Fatalf("PragmaNames not sorted: %v", names)
		}
	}
}

func TestPragmaClosedDB(t *testing.T) {
	db, err := Open(memoryPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := db.Pragma("synchronous", ""); err == nil {
		t.Fatal("Pragma on a closed DB should fail")
	}
}

func TestGetSetParameterCommand(t *testing.T) {
	db := openTestDB(t)
	d := db.Database("admin")

	// setParameter writes and reports the prior value.
	set := runCmd(t, d, bson.NewBuilder().
		AppendInt32("setParameter", 1).
		AppendString("synchronous", "off").
		Build())
	okOf(t, set)
	if v, _ := set.Lookup("synchronous"); v.StringValue() != "full" {
		t.Fatalf("setParameter prior synchronous = %q, want full", v.StringValue())
	}

	// getParameter reads the value just set.
	get := runCmd(t, d, bson.NewBuilder().
		AppendInt32("getParameter", 1).
		AppendInt32("synchronous", 1).
		Build())
	okOf(t, get)
	if v, _ := get.Lookup("synchronous"); v.StringValue() != "off" {
		t.Fatalf("getParameter synchronous = %q, want off", v.StringValue())
	}

	// getParameter: "*" returns every catalogued knob.
	all := runCmd(t, d, bson.NewBuilder().
		AppendString("getParameter", "*").
		Build())
	okOf(t, all)
	if _, ok := all.Lookup("page_size"); !ok {
		t.Fatal("getParameter:* omitted page_size")
	}
}

func BenchmarkPragmaWrite(b *testing.B) {
	db, err := Open(memoryPath)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := db.Pragma("synchronous", "off"); err != nil {
			b.Fatalf("Pragma: %v", err)
		}
	}
}

func TestSetParameterUnknownFails(t *testing.T) {
	db := openTestDB(t)
	d := db.Database("admin")
	res := d.RunCommand(context.Background(), bson.NewBuilder().
		AppendInt32("setParameter", 1).
		AppendString("page_size", "4096").
		Build())
	if _, err := res.Raw(); err == nil {
		t.Fatal("setParameter on a read-only knob should fail")
	}
}
