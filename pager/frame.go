// Package pager is the doc storage substrate: it translates page numbers to
// in-memory frames, owns an explicit buffer pool with a 2Q replacement policy,
// and enforces the write-ahead rule against the WAL so that a committed
// transaction survives any crash that leaves the media intact (spec 2061
// doc 05). Nothing above the pager ever holds a file offset; it holds only a
// page number, and the pager translates offset = page_number * page_size.
//
// The pager knows nothing about documents: a frame is page_size opaque bytes
// the pager redoes, never "document X was updated." Everything
// document-specific lives above the storage SPI seam. This is the doc-internal
// substrate the spec describes as "reused from kv"; doc builds it itself.
package pager

import "sync/atomic"

// Frame is one resident page: its page number, its page_size byte buffer, a pin
// count, a dirty flag, and the LSN of the last change to the page. The pager
// hands out *Frame to callers, who read or write Buf while the frame is pinned
// and call MarkDirty after any mutation (spec 2061 doc 05 §3.1).
type Frame struct {
	// PageID is the page number this frame currently holds.
	PageID uint64
	// Buf is the page_size byte image. Callers read and write it directly while
	// the frame is pinned.
	Buf []byte
	// pins is the pin count; nonzero means the frame is not evictable. Atomic so
	// the hot Fetch path can pin without the pool lock.
	pins atomic.Int32
	// dirty is true when Buf has been modified since it was last clean (read in
	// or written back).
	dirty bool
	// pageLSN is the LSN stamped by the last MarkDirty. It is also written into
	// the page header bytes and is what write-back compares against the durable
	// WAL LSN to enforce the write-ahead rule (spec 2061 doc 05 §3.4). A frame
	// whose pageLSN exceeds the durable WAL LSN holds uncommitted bytes and must
	// not be stolen to the main file.
	pageLSN uint64

	// Replacement-policy bookkeeping, guarded by the pool's policy mutex.
	loc  policyList // which 2Q list the frame is on
	prev *Frame
	next *Frame
}

// Pins returns the current pin count.
func (f *Frame) Pins() int32 { return f.pins.Load() }

// Dirty reports whether the frame has unwritten modifications.
func (f *Frame) Dirty() bool { return f.dirty }

// PageLSN returns the LSN of the last change to this frame.
func (f *Frame) PageLSN() uint64 { return f.pageLSN }

// policyList identifies which 2Q structure a frame is linked into.
type policyList uint8

const (
	listNone policyList = iota
	listA1in            // probation FIFO: pages seen once
	listAm              // hot LRU: pages seen at least twice
)
