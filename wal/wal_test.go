package wal

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/tamnd/doc/vfs"
)

const testPageSize = 4096

func openWAL(t *testing.T) vfs.File {
	t.Helper()
	fs := vfs.NewMemFS()
	f, err := fs.Open("db.doc-wal", vfs.OpenCreate)
	if err != nil {
		t.Fatalf("open wal: %v", err)
	}
	return f
}

func page(fill byte) []byte {
	p := make([]byte, testPageSize)
	for i := range p {
		p[i] = fill
	}
	return p
}

func TestHeaderRoundTrip(t *testing.T) {
	h := NewHeader(testPageSize, 3, 0xAABBCCDD, 0x11223344)
	enc := h.Encode()
	if len(enc) != WALHeaderSize {
		t.Fatalf("encoded header is %d bytes, want %d", len(enc), WALHeaderSize)
	}
	got, err := DecodeWALHeader(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != h {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, h)
	}
}

func TestHeaderBaseLSN(t *testing.T) {
	h := NewHeader(testPageSize, 2, 1, 2)
	if got := h.BaseLSN(); got != 2*LSNEpoch+1 {
		t.Fatalf("BaseLSN = %d, want %d", got, 2*LSNEpoch+1)
	}
}

func TestHeaderRejectsBadMagic(t *testing.T) {
	enc := NewHeader(testPageSize, 0, 1, 2).Encode()
	enc[0] ^= 0xFF
	if _, err := DecodeWALHeader(enc); err != ErrBadWALMagic {
		t.Fatalf("err = %v, want ErrBadWALMagic", err)
	}
}

func TestHeaderRejectsCorruption(t *testing.T) {
	enc := NewHeader(testPageSize, 0, 1, 2).Encode()
	enc[16] ^= 0x01 // flip a salt byte without fixing the checksum
	if _, err := DecodeWALHeader(enc); err != ErrWALHeaderCorrupt {
		t.Fatalf("err = %v, want ErrWALHeaderCorrupt", err)
	}
}

func TestHeaderRejectsVersion(t *testing.T) {
	enc := NewHeader(testPageSize, 0, 1, 2).Encode()
	binary.LittleEndian.PutUint16(enc[4:6], 99)
	c1, c2 := headerChecksum(enc[0:24])
	binary.LittleEndian.PutUint32(enc[24:28], c1)
	binary.LittleEndian.PutUint32(enc[28:32], c2)
	if _, err := DecodeWALHeader(enc); err != ErrWALVersion {
		t.Fatalf("err = %v, want ErrWALVersion", err)
	}
}

func TestWriteThenScanSingleCommit(t *testing.T) {
	f := openWAL(t)
	w, err := CreateWriter(f, NewHeader(testPageSize, 0, 7, 9))
	if err != nil {
		t.Fatalf("create writer: %v", err)
	}
	frames := []PageImage{
		{PageID: 1, Payload: page(0x11)},
		{PageID: 2, Payload: page(0x22)},
	}
	commitLSN, lastFrame, err := w.AppendCommit(frames, 3)
	if err != nil {
		t.Fatalf("append commit: %v", err)
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if lastFrame != 2 {
		t.Fatalf("last frame = %d, want 2", lastFrame)
	}
	if commitLSN != NewHeader(testPageSize, 0, 7, 9).BaseLSN()+1 {
		t.Fatalf("commit LSN = %d", commitLSN)
	}

	res, err := Scan(f, testPageSize)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Committed) != 2 {
		t.Fatalf("committed frames = %d, want 2", len(res.Committed))
	}
	if res.DBSizePages != 3 {
		t.Fatalf("db size pages = %d, want 3", res.DBSizePages)
	}
	if res.DurableLSN != commitLSN {
		t.Fatalf("durable LSN = %d, want %d", res.DurableLSN, commitLSN)
	}
	if res.TornTail {
		t.Fatal("clean WAL should not report a torn tail")
	}
	if !bytes.Equal(res.Committed[0].Payload, page(0x11)) {
		t.Fatal("frame 0 payload mismatch")
	}
	if res.Committed[1].Header.PageID != 2 {
		t.Fatalf("frame 1 page id = %d, want 2", res.Committed[1].Header.PageID)
	}
}

func TestScanDiscardsUncommittedTail(t *testing.T) {
	// A committed transaction followed by an in-flight transaction whose commit
	// frame never landed: recovery keeps the first, discards the second.
	f := openWAL(t)
	w, err := CreateWriter(f, NewHeader(testPageSize, 0, 1, 2))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.AppendCommit([]PageImage{{PageID: 1, Payload: page(0xAA)}}, 2); err != nil {
		t.Fatal(err)
	}
	// Append two more non-commit frames directly (simulating an interrupted txn:
	// frames written, fsync of the commit never reached).
	if _, _, err := w.appendFrame(2, 0, page(0xBB)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.appendFrame(3, 0, page(0xCC)); err != nil {
		t.Fatal(err)
	}
	_ = w.Sync()

	res, err := Scan(f, testPageSize)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Committed) != 1 {
		t.Fatalf("committed = %d, want 1 (uncommitted tail must be discarded)", len(res.Committed))
	}
	if res.Committed[0].Header.PageID != 1 {
		t.Fatalf("kept the wrong frame: page %d", res.Committed[0].Header.PageID)
	}
	if res.ScannedFrames != 3 {
		t.Fatalf("scanned frames = %d, want 3", res.ScannedFrames)
	}
}

func TestScanStopsAtTornFrame(t *testing.T) {
	// Corrupt a byte inside the second committed frame's payload. The chained
	// checksum breaks there; recovery keeps only the prefix before the break.
	f := openWAL(t)
	w, err := CreateWriter(f, NewHeader(testPageSize, 0, 5, 6))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.AppendCommit([]PageImage{{PageID: 1, Payload: page(0x01)}}, 2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.AppendCommit([]PageImage{{PageID: 2, Payload: page(0x02)}}, 3); err != nil {
		t.Fatal(err)
	}
	_ = w.Sync()

	// Flip a byte in the second frame's payload region.
	secondPayloadOff := FrameOffset(2, testPageSize) + FrameHeaderSize + 100
	b := make([]byte, 1)
	if _, err := f.ReadAt(b, secondPayloadOff); err != nil {
		t.Fatal(err)
	}
	b[0] ^= 0xFF
	if _, err := f.WriteAt(b, secondPayloadOff); err != nil {
		t.Fatal(err)
	}

	res, err := Scan(f, testPageSize)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Committed) != 1 {
		t.Fatalf("committed = %d, want 1 (torn frame and beyond discarded)", len(res.Committed))
	}
	if !res.TornTail {
		t.Fatal("a corrupted frame should mark TornTail")
	}
	if res.DBSizePages != 2 {
		t.Fatalf("db size = %d, want 2 (first commit)", res.DBSizePages)
	}
}

func TestScanEmptyWAL(t *testing.T) {
	f := openWAL(t)
	// Just a header, no frames.
	if _, err := CreateWriter(f, NewHeader(testPageSize, 0, 1, 2)); err != nil {
		t.Fatal(err)
	}
	res, err := Scan(f, testPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Committed) != 0 || res.DurableLSN != 0 {
		t.Fatalf("empty WAL should yield no committed frames: %+v", res)
	}
}

func TestScanNeverInitialized(t *testing.T) {
	// A zero-length WAL file (never written) must scan as "nothing to recover."
	f := openWAL(t)
	res, err := Scan(f, testPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Committed) != 0 {
		t.Fatal("zero-length WAL should recover nothing")
	}
}

func TestScanRejectsPageSizeMismatch(t *testing.T) {
	f := openWAL(t)
	if _, err := CreateWriter(f, NewHeader(testPageSize, 0, 1, 2)); err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(f, 8192); err != ErrPageSizeMismatch {
		t.Fatalf("err = %v, want ErrPageSizeMismatch", err)
	}
}

func TestResumeWriterAfterRecovery(t *testing.T) {
	// Scan, then resume appending: the new frames must chain onto the recovered
	// prefix and themselves recover cleanly.
	f := openWAL(t)
	h := NewHeader(testPageSize, 0, 3, 4)
	w, err := CreateWriter(f, h)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.AppendCommit([]PageImage{{PageID: 1, Payload: page(0x10)}}, 2); err != nil {
		t.Fatal(err)
	}
	_ = w.Sync()

	res, err := Scan(f, testPageSize)
	if err != nil {
		t.Fatal(err)
	}
	rw := ResumeWriter(f, h, res.LastFrameNo, res.DurableLSN+1, res.LastChecksum)
	if _, _, err := rw.AppendCommit([]PageImage{{PageID: 5, Payload: page(0x50)}}, 6); err != nil {
		t.Fatal(err)
	}
	_ = rw.Sync()

	res2, err := Scan(f, testPageSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Committed) != 2 {
		t.Fatalf("after resume, committed = %d, want 2", len(res2.Committed))
	}
	if res2.Committed[1].Header.PageID != 5 || res2.DBSizePages != 6 {
		t.Fatalf("resumed frame wrong: %+v", res2.Committed[1].Header)
	}
}

func TestAppendCommitRejectsEmpty(t *testing.T) {
	f := openWAL(t)
	w, err := CreateWriter(f, NewHeader(testPageSize, 0, 1, 2))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.AppendCommit(nil, 1); err != ErrEmptyCommit {
		t.Fatalf("err = %v, want ErrEmptyCommit", err)
	}
}

// TestRecoveryAtEveryCommitBoundary is the WAL-level analogue of M1's exit
// criterion: for a workload of N sequential commits, truncating the WAL at every
// commit boundary must recover exactly the committed prefix up to that point —
// no more, no less.
func TestRecoveryAtEveryCommitBoundary(t *testing.T) {
	const commits = 40
	f := openWAL(t)
	h := NewHeader(testPageSize, 0, 0x1234, 0x5678)
	w, err := CreateWriter(f, h)
	if err != nil {
		t.Fatal(err)
	}
	// Record the byte length of the WAL after each commit's fsync.
	boundaries := make([]int64, commits)
	for i := 0; i < commits; i++ {
		pg := page(byte(i))
		if _, _, err := w.AppendCommit([]PageImage{{PageID: uint64(i + 1), Payload: pg}}, uint32(i+2)); err != nil {
			t.Fatal(err)
		}
		_ = w.Sync()
		boundaries[i] = w.Offset()
	}

	full := make([]byte, boundaries[commits-1])
	if _, err := f.ReadAt(full, 0); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < commits; i++ {
		// Build a WAL truncated exactly at commit i's boundary.
		fs := vfs.NewMemFS()
		tf, _ := fs.Open("t.doc-wal", vfs.OpenCreate)
		if _, err := tf.WriteAt(full[:boundaries[i]], 0); err != nil {
			t.Fatal(err)
		}
		res, err := Scan(tf, testPageSize)
		if err != nil {
			t.Fatalf("scan at boundary %d: %v", i, err)
		}
		if len(res.Committed) != i+1 {
			t.Fatalf("boundary %d: committed = %d, want %d", i, len(res.Committed), i+1)
		}
		if res.DBSizePages != uint32(i+2) {
			t.Fatalf("boundary %d: db size = %d, want %d", i, res.DBSizePages, i+2)
		}
		if res.Committed[i].Header.PageID != uint64(i+1) {
			t.Fatalf("boundary %d: last page id = %d, want %d", i, res.Committed[i].Header.PageID, i+1)
		}
	}
}

// TestRecoveryAtEveryByteOffset is the torn-write analogue: truncating the WAL
// at every byte offset (a crash mid-append) must always recover a valid
// committed prefix and never panic or over-recover.
func TestRecoveryAtEveryByteOffset(t *testing.T) {
	const commits = 6
	f := openWAL(t)
	h := NewHeader(testPageSize, 0, 0xABCD, 0xEF01)
	w, err := CreateWriter(f, h)
	if err != nil {
		t.Fatal(err)
	}
	commitOffsets := make([]int64, commits)
	for i := 0; i < commits; i++ {
		if _, _, err := w.AppendCommit([]PageImage{{PageID: uint64(i + 1), Payload: page(byte(i))}}, uint32(i+2)); err != nil {
			t.Fatal(err)
		}
		_ = w.Sync()
		commitOffsets[i] = w.Offset()
	}
	total := w.Offset()
	full := make([]byte, total)
	if _, err := f.ReadAt(full, 0); err != nil {
		t.Fatal(err)
	}

	committedAt := func(off int64) int {
		// The number of commits whose full frame ends at or before off.
		n := 0
		for _, co := range commitOffsets {
			if co <= off {
				n++
			}
		}
		return n
	}

	for off := int64(0); off <= total; off++ {
		fs := vfs.NewMemFS()
		tf, _ := fs.Open("t.doc-wal", vfs.OpenCreate)
		if off > 0 {
			if _, err := tf.WriteAt(full[:off], 0); err != nil {
				t.Fatal(err)
			}
		}
		res, err := Scan(tf, testPageSize)
		if err != nil {
			t.Fatalf("scan at offset %d: %v", off, err)
		}
		want := committedAt(off)
		if len(res.Committed) != want {
			t.Fatalf("offset %d: committed = %d, want %d", off, len(res.Committed), want)
		}
	}
}
