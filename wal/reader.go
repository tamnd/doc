package wal

import (
	"encoding/binary"
	"errors"
	"io"

	"github.com/tamnd/doc/vfs"
)

// Frame is one scanned WAL frame: its decoded header, its 1-based ordinal, and
// its page payload.
type Frame struct {
	Header  FrameHeader
	FrameNo uint32
	Payload []byte
}

// ScanResult is the outcome of a recovery scan over a WAL generation.
type ScanResult struct {
	// Committed holds every frame up to and including the last frame whose
	// commit marker verified - the durable, committed prefix. Frames after the
	// last commit marker (an in-flight transaction interrupted by the crash) are
	// excluded: recovery restores the committed prefix and nothing else
	// (spec 2061 doc 05 §1.1, §14).
	Committed []Frame
	// DurableLSN is the frame_lsn of the last committed frame, or 0 if the WAL
	// holds no complete commit.
	DurableLSN uint64
	// DBSizePages is the database size in pages recorded by the last commit
	// marker; recovery sets the main file to this length (spec 2061 doc 05 §6.2).
	DBSizePages uint32
	// LastFrameNo is the ordinal of the last committed frame (0 if none); the
	// resume writer appends after it.
	LastFrameNo uint32
	// LastChecksum is the chained checksum of the last committed frame; the
	// resume writer chains from it.
	LastChecksum uint32
	// TornTail is true when the scan stopped before clean EOF because a frame
	// failed its checksum or was short - an interrupted append. The bytes past
	// the durable prefix are discarded.
	TornTail bool
	// ScannedFrames is the total number of structurally-complete frames the scan
	// validated before stopping (including uncommitted ones past the last
	// commit). Diagnostic only.
	ScannedFrames uint32
}

// Scan walks a WAL file from its first frame, validating the salt-seeded checksum
// chain, and returns the committed prefix (spec 2061 doc 05 §14.3). It stops at
// the first frame that fails its checksum, is truncated, or is a short/zero read
// - that frame and everything after it are an interrupted, uncommitted tail and
// are discarded. The committed prefix is everything up to the last frame whose
// commit marker verified.
//
// The file's WAL header is read and validated first; its page size must match
// the supplied expectedPageSize (the main database's page size).
func Scan(file vfs.File, expectedPageSize uint32) (ScanResult, error) {
	var res ScanResult

	hb := make([]byte, WALHeaderSize)
	n, err := file.ReadAt(hb, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return res, err
	}
	if n < WALHeaderSize {
		// No (or partial) header: an empty/never-initialized WAL. Nothing to
		// recover; the database opens from the main file alone.
		return res, nil
	}
	hdr, err := DecodeWALHeader(hb)
	if err != nil {
		// A corrupt WAL header means no trustworthy frames. Treat as no durable
		// WAL content rather than failing the open; the main file is consistent
		// as of the last checkpoint.
		return res, nil
	}
	if hdr.PageSize != expectedPageSize {
		return res, ErrPageSizeMismatch
	}

	frameSize := FrameSize(hdr.PageSize)
	prev := hdr.initialPrevChecksum()
	offset := int64(WALHeaderSize)
	var frameNo uint32
	lastCommitIdx := -1
	frameBuf := make([]byte, frameSize)

	var frames []Frame
	for {
		frameNo++
		rn, rerr := file.ReadAt(frameBuf, offset)
		if rn < int(frameSize) {
			// Short read: truncated final frame or clean EOF. Either way this is
			// the tail; stop. TornTail is set only if some bytes were present
			// (a genuine partial frame), not on a clean boundary.
			if rn > 0 {
				res.TornTail = true
			}
			break
		}
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return res, rerr
		}

		var fh FrameHeader
		fh.PageID = binary.LittleEndian.Uint64(frameBuf[0:8])
		fh.FrameLSN = binary.LittleEndian.Uint64(frameBuf[8:16])
		fh.CommitMarker = binary.LittleEndian.Uint32(frameBuf[16:20])
		fh.Checksum = binary.LittleEndian.Uint32(frameBuf[20:24])
		payload := frameBuf[FrameHeaderSize:]

		want := frameChecksum(hdr.Salt1, hdr.Salt2, prev, frameBuf[0:20], payload)
		if want != fh.Checksum {
			// Broken chain: a torn frame, a stale frame from a previous
			// generation, or a bit flip. This is the durable boundary.
			res.TornTail = true
			break
		}

		// Structurally valid frame. Copy the payload (frameBuf is reused).
		pcopy := make([]byte, len(payload))
		copy(pcopy, payload)
		frames = append(frames, Frame{Header: fh, FrameNo: frameNo, Payload: pcopy})
		res.ScannedFrames = frameNo

		if fh.IsCommit() {
			lastCommitIdx = len(frames) - 1
		}
		prev = fh.Checksum
		offset += frameSize
	}

	if lastCommitIdx >= 0 {
		res.Committed = frames[:lastCommitIdx+1]
		last := res.Committed[len(res.Committed)-1]
		res.DurableLSN = last.Header.FrameLSN
		res.DBSizePages = last.Header.CommitMarker
		res.LastFrameNo = last.FrameNo
		res.LastChecksum = last.Header.Checksum
	}
	return res, nil
}
