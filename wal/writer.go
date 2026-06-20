package wal

import (
	"encoding/binary"

	"github.com/tamnd/doc/vfs"
)

// PageImage is one page's new content destined for the WAL: the main-file page
// number and the full page_size-byte image (the first-touch rule means M1 always
// logs full images, spec 2061 doc 05 §6.4).
type PageImage struct {
	PageID  uint64
	Payload []byte
}

// Writer appends frames to a .doc-wal file. It is not safe for concurrent use;
// the caller (the commit path) serializes appends, which matches the WAL
// manager holding an exclusive write position (spec 2061 doc 05 §6.5).
type Writer struct {
	file        vfs.File
	header      Header
	pageSize    uint32
	writeOffset int64  // byte offset of the next frame
	nextFrame   uint32 // 1-based ordinal of the next frame to write
	nextLSN     uint64 // frame_lsn for the next frame
	prevCksum   uint32 // cumulative checksum of the last appended frame
}

// CreateWriter writes a fresh WAL header to file and returns a Writer positioned
// to append the generation's first frame. The file is truncated to just the
// header so any stale frames from a recycled file are logically gone (their
// old-salt checksums would fail the chain regardless, but truncating is tidy).
func CreateWriter(file vfs.File, h Header) (*Writer, error) {
	hb := h.Encode()
	if err := file.Truncate(0); err != nil {
		return nil, err
	}
	if _, err := file.WriteAt(hb, 0); err != nil {
		return nil, err
	}
	return &Writer{
		file:        file,
		header:      h,
		pageSize:    h.PageSize,
		writeOffset: WALHeaderSize,
		nextFrame:   1,
		nextLSN:     h.BaseLSN(),
		prevCksum:   h.initialPrevChecksum(),
	}, nil
}

// ResumeWriter returns a Writer that continues appending after an existing
// scanned generation: afterFrame frames already exist, the next LSN is nextLSN,
// and prevCksum chains from the last existing frame. Used after recovery has
// determined the durable tail and the database resumes writing.
func ResumeWriter(file vfs.File, h Header, afterFrame uint32, nextLSN uint64, prevCksum uint32) *Writer {
	return &Writer{
		file:        file,
		header:      h,
		pageSize:    h.PageSize,
		writeOffset: WALHeaderSize + int64(afterFrame)*FrameSize(h.PageSize),
		nextFrame:   afterFrame + 1,
		nextLSN:     nextLSN,
		prevCksum:   prevCksum,
	}
}

// appendFrame writes one frame and advances the writer's position, LSN, and
// chained checksum. payload must be exactly pageSize bytes.
func (w *Writer) appendFrame(pageID uint64, commitMarker uint32, payload []byte) (lsn uint64, frameNo uint32, err error) {
	hdr := make([]byte, FrameHeaderSize)
	lsn = w.nextLSN
	encodeFrameHeaderPrefix(hdr, pageID, lsn, commitMarker)
	cksum := frameChecksum(w.header.Salt1, w.header.Salt2, w.prevCksum, hdr[0:20], payload)
	binary.LittleEndian.PutUint32(hdr[20:24], cksum)

	if _, err = w.file.WriteAt(hdr, w.writeOffset); err != nil {
		return 0, 0, err
	}
	if _, err = w.file.WriteAt(payload, w.writeOffset+FrameHeaderSize); err != nil {
		return 0, 0, err
	}
	frameNo = w.nextFrame
	w.prevCksum = cksum
	w.writeOffset += FrameSize(w.pageSize)
	w.nextFrame++
	w.nextLSN++
	return lsn, frameNo, nil
}

// AppendCommit appends all frames of one transaction and stamps the last frame
// with commitMarker = dbSizePages (always >= 1, since the header page exists),
// making the batch the durable-commit signal. It returns the commit LSN - the
// frame_lsn of the last frame, which the MVCC layer uses as the version number
// (spec 2061 doc 05 §9.5). Frames are appended but NOT fsync'd; the caller
// invokes Sync (group commit batches the fsync across transactions).
func (w *Writer) AppendCommit(frames []PageImage, dbSizePages uint32) (commitLSN uint64, lastFrameNo uint32, err error) {
	if len(frames) == 0 {
		return 0, 0, ErrEmptyCommit
	}
	if dbSizePages == 0 {
		// A commit marker must be nonzero; the db always has at least the header
		// page, so a zero size is a programming error.
		dbSizePages = 1
	}
	for i, fr := range frames {
		marker := uint32(0)
		if i == len(frames)-1 {
			marker = dbSizePages
		}
		lsn, frameNo, aerr := w.appendFrame(fr.PageID, marker, fr.Payload)
		if aerr != nil {
			return 0, 0, aerr
		}
		if i == len(frames)-1 {
			commitLSN = lsn
			lastFrameNo = frameNo
		}
	}
	return commitLSN, lastFrameNo, nil
}

// Sync forces all appended frames to durable storage. The WAL uses the strongest
// available data-sync primitive (spec 2061 doc 05 §11.3); the vfs File maps
// SyncData to fdatasync on Linux and F_FULLFSYNC on macOS.
func (w *Writer) Sync() error {
	return w.file.Sync(vfs.SyncData)
}

// NextLSN returns the LSN that will be assigned to the next appended frame.
func (w *Writer) NextLSN() uint64 { return w.nextLSN }

// FrameCount returns the number of frames appended so far in this generation.
func (w *Writer) FrameCount() uint32 { return w.nextFrame - 1 }

// Offset returns the byte offset at which the next frame will be written.
func (w *Writer) Offset() int64 { return w.writeOffset }
