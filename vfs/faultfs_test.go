package vfs

import (
	"errors"
	"testing"
)

func newFaultFile(t *testing.T) (*FaultFS, File) {
	t.Helper()
	ff := NewFaultFS(NewMemFS())
	f, err := ff.Open("db.doc", OpenCreate)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return ff, f
}

func TestFaultNonePassthrough(t *testing.T) {
	_, f := newFaultFile(t)
	if _, err := f.WriteAt([]byte("data"), 0); err != nil {
		t.Fatalf("write with no fault armed: %v", err)
	}
	if err := f.Sync(SyncFull); err != nil {
		t.Fatalf("sync with no fault armed: %v", err)
	}
}

func TestFaultWrite(t *testing.T) {
	ff, f := newFaultFile(t)
	sentinel := errors.New("disk full")
	ff.Arm(FaultPlan{Mode: FaultWrite, Err: sentinel, AfterWrites: 1})
	// First write passes (AfterWrites=1 allows one).
	if _, err := f.WriteAt([]byte("ok"), 0); err != nil {
		t.Fatalf("first write should pass: %v", err)
	}
	// Second write faults.
	if _, err := f.WriteAt([]byte("boom"), 10); !errors.Is(err, sentinel) {
		t.Fatalf("second write err = %v, want sentinel", err)
	}
	if ff.Injected() != 1 {
		t.Fatalf("Injected = %d, want 1", ff.Injected())
	}
}

func TestFaultDropDiscardsBytes(t *testing.T) {
	ff, f := newFaultFile(t)
	if _, err := f.WriteAt([]byte("durable"), 0); err != nil {
		t.Fatal(err)
	}
	ff.Arm(FaultPlan{Mode: FaultDrop, AfterWrites: 0})
	// This write reports success but is discarded.
	n, err := f.WriteAt([]byte("LOSTLOST"), 0)
	if err != nil || n != 8 {
		t.Fatalf("dropped write should report success: n=%d err=%v", n, err)
	}
	// The original bytes are still there; the dropped write did not land.
	buf := make([]byte, 7)
	if _, err := f.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "durable" {
		t.Fatalf("read %q, want durable (dropped write must not persist)", buf)
	}
}

func TestFaultTearWritesPrefix(t *testing.T) {
	ff, f := newFaultFile(t)
	ff.Arm(FaultPlan{Mode: FaultTear, AfterWrites: 0, TearAt: 3})
	full := []byte("ABCDEFGH")
	n, err := f.WriteAt(full, 0)
	if n != 3 {
		t.Fatalf("torn write returned n=%d, want 3", n)
	}
	if err == nil {
		t.Fatal("torn write should return an error")
	}
	// Only the first 3 bytes landed.
	buf := make([]byte, 3)
	if _, err := f.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ABC" {
		t.Fatalf("torn prefix = %q, want ABC", buf)
	}
	sz, _ := f.Size()
	if sz != 3 {
		t.Fatalf("file size after tear = %d, want 3", sz)
	}
}

func TestFaultSync(t *testing.T) {
	ff, f := newFaultFile(t)
	ff.Arm(FaultPlan{Mode: FaultSync, AfterSyncs: 1})
	if err := f.Sync(SyncFull); err != nil {
		t.Fatalf("first sync should pass: %v", err)
	}
	if err := f.Sync(SyncFull); !errors.Is(err, ErrInjected) {
		t.Fatalf("second sync err = %v, want ErrInjected", err)
	}
}

func TestFaultOnceDisarms(t *testing.T) {
	ff, f := newFaultFile(t)
	ff.Arm(FaultPlan{Mode: FaultWrite, AfterWrites: 0, Once: true})
	if _, err := f.WriteAt([]byte("x"), 0); !errors.Is(err, ErrInjected) {
		t.Fatalf("first write should fault: %v", err)
	}
	if _, err := f.WriteAt([]byte("y"), 0); err != nil {
		t.Fatalf("after Once, write should pass: %v", err)
	}
	if ff.Injected() != 1 {
		t.Fatalf("Injected = %d, want 1", ff.Injected())
	}
}

func TestFaultDisarm(t *testing.T) {
	ff, f := newFaultFile(t)
	ff.Arm(FaultPlan{Mode: FaultWrite, AfterWrites: 0})
	ff.Disarm()
	if _, err := f.WriteAt([]byte("z"), 0); err != nil {
		t.Fatalf("disarmed write should pass: %v", err)
	}
}

// TestFaultNoPanic exercises every mode at the AfterWrites boundary to confirm
// the injector never panics (an M0 exit criterion).
func TestFaultNoPanic(t *testing.T) {
	modes := []FaultMode{FaultNone, FaultWrite, FaultSync, FaultTear, FaultDrop}
	for _, m := range modes {
		ff, f := newFaultFile(t)
		ff.Arm(FaultPlan{Mode: m, AfterWrites: 0, AfterSyncs: 0, TearAt: 2})
		_, _ = f.WriteAt([]byte("payload"), 0)
		_ = f.Sync(SyncData)
		_, _ = f.ReadAt(make([]byte, 4), 0)
		_ = f.Truncate(1)
		_ = f.Close()
	}
}
