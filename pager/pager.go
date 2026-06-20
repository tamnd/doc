package pager

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"github.com/tamnd/doc/format"
	"github.com/tamnd/doc/vfs"
	"github.com/tamnd/doc/wal"
)

// SyncLevel selects the durability barrier applied at commit (spec 2061 doc 05
// §10). It maps the MongoDB-style synchronous PRAGMA onto the VFS sync modes.
type SyncLevel int

const (
	// SyncOff never fsyncs the WAL at commit: a commit is durable across a clean
	// process exit but may be lost on power failure. Fastest; unsafe.
	SyncOff SyncLevel = iota
	// SyncNormal fsyncs the WAL with fdatasync at commit. The default.
	SyncNormal
	// SyncFull fsyncs the WAL with the strongest barrier (F_FULLFSYNC on macOS)
	// at commit. Required for true power-loss durability.
	SyncFull
)

// Sentinel errors.
var (
	// ErrClosed reports an operation on a closed pager.
	ErrClosed = errors.New("pager: closed")
	// ErrCorrupt reports a page that failed its checksum on read.
	ErrCorrupt = errors.New("pager: page checksum verification failed")
	// ErrPoolExhausted reports that every frame is pinned and no page can be read
	// in. The caller is holding too many pins; raise the pool size.
	ErrPoolExhausted = errors.New("pager: buffer pool exhausted (all frames pinned)")
	// ErrReadOnly reports a mutation on a read-only pager.
	ErrReadOnly = errors.New("pager: database opened read-only")
)

// freelistNextOffset is where a free page stores the page number of the next
// page on the freelist stack (the page is otherwise unused while free).
const freelistNextOffset = format.PageHeaderSize

// Options configures a pager at open time. Zero values select the defaults.
type Options struct {
	// PageSize is used only when creating a new database; an existing database's
	// page size is read from its header. Zero defaults to 8 KiB.
	PageSize uint32
	// PoolPages is the buffer pool capacity in frames. Zero defaults to 1024.
	PoolPages int
	// Checksum selects the page checksum algorithm for a new database. Zero
	// defaults to CRC-32C.
	Checksum format.ChecksumAlgo
	// Sync is the commit durability level. Zero (SyncOff) is overridden to
	// SyncNormal unless ReadOnly; pass SyncOff explicitly via the field if you
	// truly want it off.
	Sync SyncLevel
	// ReadOnly opens without write access; mutations return ErrReadOnly.
	ReadOnly bool
	// CheckpointFrames triggers an automatic checkpoint once the current WAL
	// generation reaches this many frames. Zero defaults to 1000.
	CheckpointFrames int
}

func (o *Options) withDefaults() {
	if o.PageSize == 0 {
		o.PageSize = format.PageSize8K
	}
	if o.PoolPages == 0 {
		o.PoolPages = 1024
	}
	if o.Checksum == 0 {
		o.Checksum = format.ChecksumCRC32C
	}
	if o.CheckpointFrames == 0 {
		o.CheckpointFrames = 1000
	}
}

// Pager owns a single .doc file, its .doc-wal sidecar, and the buffer pool over
// them. It is safe for concurrent use; M1 serializes every operation under one
// mutex (fine-grained pool latching is a later tuning lever, spec 2061 doc 05
// §3.5). Page 0 is the database header and is managed as a dedicated frame so it
// is logged and recovered like any other page.
type Pager struct {
	mu sync.Mutex

	fs       vfs.FS
	path     string
	main     vfs.File
	walPath  string
	walFile  vfs.File
	walw     *wal.Writer
	pool     *pool
	pageSize int
	checksum format.ChecksumAlgo
	sync     SyncLevel
	readOnly bool
	ckptN    int

	hdr    format.Header
	frame0 *Frame // page 0: the database header

	nextLSN       uint64 // monotonic LSN clock; stamped into page headers
	walFlushedLSN uint64 // highest LSN durable in the WAL
	checkpointSeq uint32 // current WAL generation
	walFrames     int    // frames appended in the current generation

	closed bool
}

// Open opens the database at path on fs, creating it when it does not exist and
// opts.ReadOnly is false. If the file exists it is recovered: the committed WAL
// prefix is replayed into the main file, then a fresh WAL generation is started
// (spec 2061 doc 05 §14).
func Open(fs vfs.FS, path string, opts Options) (*Pager, error) {
	opts.withDefaults()

	exists, err := fs.Exists(path)
	if err != nil {
		return nil, err
	}
	if !exists && opts.ReadOnly {
		return nil, fmt.Errorf("pager: %s does not exist", path)
	}

	flags := vfs.OpenCreate
	if opts.ReadOnly {
		flags = vfs.OpenReadOnly
	}
	main, err := fs.Open(path, flags)
	if err != nil {
		return nil, err
	}

	p := &Pager{
		fs:       fs,
		path:     path,
		main:     main,
		walPath:  path + "-wal",
		checksum: opts.Checksum,
		sync:     opts.Sync,
		readOnly: opts.ReadOnly,
		ckptN:    opts.CheckpointFrames,
	}

	if exists {
		if err := p.openExisting(opts); err != nil {
			_ = main.Close()
			return nil, err
		}
	} else {
		if err := p.createNew(opts); err != nil {
			_ = main.Close()
			return nil, err
		}
	}
	return p, nil
}

// createNew lays down a fresh database: page 0 holds the encoded header, the
// file is one page long, and a first WAL generation is opened.
func (p *Pager) createNew(opts Options) error {
	p.pageSize = int(opts.PageSize)
	salt := genU64()
	var uuid [16]byte
	if _, err := rand.Read(uuid[:]); err != nil {
		return err
	}
	p.hdr = format.NewHeader(opts.PageSize, opts.Checksum, salt, uuid)

	p.pool = newPool(p.pageSize, opts.PoolPages)
	p.frame0 = &Frame{PageID: 0, Buf: make([]byte, p.pageSize)}
	p.frame0.pins.Add(1) // page 0 is permanently resident; never evict the header
	p.pool.table[0] = p.frame0
	p.pool.resident++
	p.encodeHeaderIntoFrame0()

	// Persist page 0 directly so a freshly created file is openable even before
	// any commit.
	if _, err := p.main.WriteAt(p.frame0.Buf, 0); err != nil {
		return err
	}
	if err := p.main.Sync(vfs.SyncFull); err != nil {
		return err
	}
	p.frame0.dirty = false

	p.checkpointSeq = 1
	return p.openFreshWAL()
}

// openExisting reads the header, replays the committed WAL prefix into the main
// file, and starts a fresh WAL generation.
func (p *Pager) openExisting(opts Options) error {
	hb := make([]byte, format.HeaderSize)
	if _, err := p.main.ReadAt(hb, 0); err != nil {
		return err
	}
	hdr, err := format.DecodeHeader(hb)
	if err != nil {
		return err
	}
	p.hdr = hdr
	p.pageSize = int(hdr.PageSize)
	p.checksum = hdr.ChecksumAlgo
	p.pool = newPool(p.pageSize, opts.PoolPages)

	gen, err := p.recover()
	if err != nil {
		return err
	}

	// Re-read page 0 (replay may have updated the header) and make it the
	// resident frame 0.
	if _, err := p.main.ReadAt(hb, 0); err != nil {
		return err
	}
	if p.hdr, err = format.DecodeHeader(hb); err != nil {
		return err
	}
	p.frame0 = &Frame{PageID: 0, Buf: make([]byte, p.pageSize)}
	full := make([]byte, p.pageSize)
	if _, err := p.main.ReadAt(full, 0); err != nil {
		return err
	}
	copy(p.frame0.Buf, full)
	p.frame0.pins.Add(1) // page 0 is permanently resident; never evict the header
	p.pool.table[0] = p.frame0
	p.pool.resident++

	p.checkpointSeq = gen + 1
	if p.readOnly {
		return nil
	}
	return p.openFreshWAL()
}

// recover scans the WAL and replays its committed prefix into the main file. It
// returns the WAL generation it scanned (its checkpoint_seq) so the caller can
// start the next generation. A missing or empty WAL recovers nothing.
func (p *Pager) recover() (gen uint32, err error) {
	exists, err := p.fs.Exists(p.walPath)
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	wf, err := p.fs.Open(p.walPath, vfs.OpenRead)
	if err != nil {
		return 0, err
	}
	defer func() { _ = wf.Close() }()

	res, err := wal.Scan(wf, uint32(p.pageSize))
	if err != nil {
		return 0, err
	}
	if len(res.Committed) == 0 {
		return 0, nil
	}

	// Redo: write each committed full-page image to its page offset in order.
	// Full-image replay is idempotent, so replaying a prefix already partly
	// reflected in the main file (an interrupted checkpoint or a stolen page)
	// simply rewrites the same bytes (spec 2061 doc 05 §14.4).
	for _, fr := range res.Committed {
		off := int64(fr.Header.PageID) * int64(p.pageSize)
		if _, err := p.main.WriteAt(fr.Payload, off); err != nil {
			return 0, err
		}
	}
	// Set the main file to the size the last commit recorded, repairing a crash
	// that left the file longer or shorter than the committed database.
	if err := p.main.Truncate(int64(res.DBSizePages) * int64(p.pageSize)); err != nil {
		return 0, err
	}
	if err := p.main.Sync(vfs.SyncFull); err != nil {
		return 0, err
	}

	// Decode the scanned generation's checkpoint_seq from the WAL header so the
	// next generation is strictly greater (keeps LSNs globally monotone).
	gen = p.scanGeneration(wf)
	return gen, nil
}

// scanGeneration reads just the WAL header to learn its checkpoint_seq. A
// failure to read it falls back to 0 (the next generation becomes 1).
func (p *Pager) scanGeneration(wf vfs.File) uint32 {
	hb := make([]byte, wal.WALHeaderSize)
	if _, err := wf.ReadAt(hb, 0); err != nil {
		return 0
	}
	h, err := wal.DecodeWALHeader(hb)
	if err != nil {
		return 0
	}
	return h.CheckpointSeq
}

// openFreshWAL truncates the WAL file and writes a new generation header with
// fresh salts. Called at create, after recovery, and after each checkpoint.
func (p *Pager) openFreshWAL() error {
	wf, err := p.fs.Open(p.walPath, vfs.OpenCreate)
	if err != nil {
		return err
	}
	p.walFile = wf
	s1, s2 := genSalt(), genSalt()
	h := wal.NewHeader(uint32(p.pageSize), p.checkpointSeq, s1, s2)
	w, err := wal.CreateWriter(wf, h)
	if err != nil {
		return err
	}
	if err := wf.Sync(vfs.SyncFull); err != nil {
		return err
	}
	p.walw = w
	p.walFrames = 0
	return nil
}

// PageSize returns the fixed page size in bytes.
func (p *Pager) PageSize() int { return p.pageSize }

// Checksum returns the page checksum algorithm the database was created with.
// Layers above the pager (the heap's overflow chains) need it to stamp the
// pages they format before handing them back for write-back.
func (p *Pager) Checksum() format.ChecksumAlgo { return p.checksum }

// CatalogRoot returns the root page of the catalog/_id-index B-tree, or
// format.NullPage when none exists yet. In M1 (single collection, single index)
// this header slot holds the _id index root directly; the catalog that will
// eventually occupy it arrives in M3.
func (p *Pager) CatalogRoot() uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hdr.CatalogRoot
}

// SetCatalogRoot persists root into the header and dirties page 0 so the change
// is logged with the current transaction and made durable on the next Commit.
func (p *Pager) SetCatalogRoot(root uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hdr.CatalogRoot = root
	p.touchHeaderLocked()
}

// PageCount returns the number of pages the header records.
func (p *Pager) PageCount() uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hdr.PageCount
}

// Fetch returns the frame for pageID, pinned. The caller reads or writes Buf and
// must call Unpin when done. forUpdate is accepted for interface parity with the
// spec's latch-on-fetch optimization; M1's coarse lock makes it a no-op.
func (p *Pager) Fetch(pageID uint64, forUpdate bool) (*Frame, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fetchLocked(pageID)
}

func (p *Pager) fetchLocked(pageID uint64) (*Frame, error) {
	if p.closed {
		return nil, ErrClosed
	}
	if f := p.pool.lookup(pageID); f != nil {
		f.pins.Add(1)
		p.pool.recordHit(f)
		return f, nil
	}
	f, err := p.obtainFrameLocked()
	if err != nil {
		return nil, err
	}
	if err := p.readPageInto(pageID, f.Buf); err != nil {
		// Roll the half-installed frame back so the pool is not left with a
		// phantom entry.
		p.pool.resident--
		return nil, err
	}
	f.pageLSN = p.readPageLSN(pageID, f.Buf)
	f.dirty = false
	p.pool.admit(f, pageID)
	f.pins.Add(1)
	return f, nil
}

// obtainFrameLocked returns a detached frame ready to hold a new page, evicting
// and writing back a victim if the pool is full.
func (p *Pager) obtainFrameLocked() (*Frame, error) {
	p.pool.committedLSN = p.walFlushedLSN
	f, victim, ok := p.pool.obtainFrame()
	if !ok {
		return nil, ErrPoolExhausted
	}
	if victim != nil {
		if err := p.writeBack(victim); err != nil {
			return nil, err
		}
		f = p.pool.reuseVictim(victim)
	}
	return f, nil
}

// readPageInto reads pageID from the main file, verifying the checksum for a
// content page. A short read for an in-range page (allocated but never written
// back) yields a zeroed buffer rather than an error.
func (p *Pager) readPageInto(pageID uint64, buf []byte) error {
	off := int64(pageID) * int64(p.pageSize)
	n, err := p.main.ReadAt(buf, off)
	if err != nil && n < len(buf) {
		if pageID < uint64(p.hdr.PageCount) {
			for i := range buf {
				buf[i] = 0
			}
			return nil
		}
		return err
	}
	if pageID == 0 {
		// Page 0 carries the database header with its own checksum, validated at
		// open; nothing more to verify here.
		return nil
	}
	if vErr := format.VerifyPageChecksum(buf, p.checksum); vErr != nil {
		return fmt.Errorf("%w: page %d", ErrCorrupt, pageID)
	}
	return nil
}

// readPageLSN extracts the page_lsn from a content page's common header (0 for
// page 0, which has no such field).
func (p *Pager) readPageLSN(pageID uint64, buf []byte) uint64 {
	if pageID == 0 {
		return 0
	}
	return binary.LittleEndian.Uint64(buf[8:16])
}

// MarkDirty marks f modified and stamps it with the next LSN, writing that LSN
// into the page's common header (page 0 excepted). Call it after every in-place
// mutation of a frame's bytes (spec 2061 doc 05 §2.2).
func (p *Pager) MarkDirty(f *Frame) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.markDirtyLocked(f)
}

func (p *Pager) markDirtyLocked(f *Frame) {
	p.nextLSN++
	f.pageLSN = p.nextLSN
	f.dirty = true
	if f.PageID != 0 {
		binary.LittleEndian.PutUint64(f.Buf[8:16], f.pageLSN)
	}
}

// Unpin releases one pin on f.
func (p *Pager) Unpin(f *Frame) {
	if f == nil {
		return
	}
	f.pins.Add(-1)
}

// Allocate returns a new pinned, zeroed page, popping the freelist or extending
// the file. The header (page 0) is updated and dirtied; the caller stamps the
// page's type before use (spec 2061 doc 05 §2.3).
func (p *Pager) Allocate() (uint64, *Frame, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, nil, ErrClosed
	}
	if p.readOnly {
		return 0, nil, ErrReadOnly
	}

	if p.hdr.FreelistRoot != format.NullPage {
		return p.allocateFromFreelist()
	}
	return p.allocateByGrowing()
}

func (p *Pager) allocateFromFreelist() (uint64, *Frame, error) {
	pageID := uint64(p.hdr.FreelistRoot)
	f, err := p.fetchLocked(pageID)
	if err != nil {
		return 0, nil, err
	}
	next := binary.LittleEndian.Uint32(f.Buf[freelistNextOffset : freelistNextOffset+4])
	p.hdr.FreelistRoot = next
	if p.hdr.FreelistPageCount > 0 {
		p.hdr.FreelistPageCount--
	}
	for i := range f.Buf {
		f.Buf[i] = 0
	}
	p.markDirtyLocked(f)
	p.touchHeaderLocked()
	return pageID, f, nil
}

func (p *Pager) allocateByGrowing() (uint64, *Frame, error) {
	pageID := uint64(p.hdr.PageCount)
	p.hdr.PageCount++
	p.touchHeaderLocked()

	f, err := p.obtainFrameLocked()
	if err != nil {
		// Undo the header bump so a transient pool exhaustion is not persisted.
		p.hdr.PageCount--
		p.touchHeaderLocked()
		return 0, nil, err
	}
	for i := range f.Buf {
		f.Buf[i] = 0
	}
	p.pool.admit(f, pageID)
	f.pins.Add(1)
	p.markDirtyLocked(f)
	return pageID, f, nil
}

// Free returns pageID to the freelist. The caller must hold the page pinned; the
// pager records the free and unpins it (spec 2061 doc 05 §2.3).
func (p *Pager) Free(pageID uint64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	if p.readOnly {
		return ErrReadOnly
	}
	f := p.pool.lookup(pageID)
	if f == nil {
		var err error
		f, err = p.fetchLocked(pageID)
		if err != nil {
			return err
		}
	}
	format.InitPage(f.Buf, format.PageFree, 0)
	binary.LittleEndian.PutUint32(f.Buf[freelistNextOffset:freelistNextOffset+4], p.hdr.FreelistRoot)
	p.hdr.FreelistRoot = uint32(pageID)
	p.hdr.FreelistPageCount++
	p.markDirtyLocked(f)
	p.touchHeaderLocked()
	p.Unpin(f)
	return nil
}

// touchHeaderLocked re-encodes the in-memory header into frame 0 and marks it
// dirty so the header change is logged with the transaction.
func (p *Pager) touchHeaderLocked() {
	p.encodeHeaderIntoFrame0()
	p.markDirtyLocked(p.frame0)
}

func (p *Pager) encodeHeaderIntoFrame0() {
	p.hdr.FileChangeCounter++
	p.hdr.VersionValidFor = p.hdr.FileChangeCounter
	enc := p.hdr.Encode()
	for i := range p.frame0.Buf {
		p.frame0.Buf[i] = 0
	}
	copy(p.frame0.Buf, enc)
}

// Commit makes every page dirtied since the last commit durable: it appends
// their full images to the WAL as one commit batch, fsyncs at the configured
// level, and advances the durable LSN. Pages stay dirty (and reach the main file
// only at checkpoint or eviction) but are now recoverable (spec 2061 doc 05 §9).
// A commit with no dirty pages is a no-op.
func (p *Pager) Commit() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.commitLocked()
}

func (p *Pager) commitLocked() error {
	if p.closed {
		return ErrClosed
	}
	if p.readOnly {
		return ErrReadOnly
	}

	// Collect the uncommitted dirty frames: those whose pageLSN is past the last
	// durable LSN. Single-writer M1 means these all belong to one transaction.
	var dirty []*Frame
	p.pool.forEachResident(func(f *Frame) {
		if f.dirty && f.pageLSN > p.walFlushedLSN {
			dirty = append(dirty, f)
		}
	})
	if len(dirty) == 0 {
		return nil
	}
	// Order by pageLSN so frames are logged in the order they were dirtied.
	sortFramesByLSN(dirty)

	images := make([]wal.PageImage, len(dirty))
	var maxLSN uint64
	for i, f := range dirty {
		if f.PageID != 0 {
			format.WritePageChecksum(f.Buf, p.checksum)
		}
		buf := make([]byte, p.pageSize)
		copy(buf, f.Buf)
		images[i] = wal.PageImage{PageID: f.PageID, Payload: buf}
		if f.pageLSN > maxLSN {
			maxLSN = f.pageLSN
		}
	}

	if _, _, err := p.walw.AppendCommit(images, p.hdr.PageCount); err != nil {
		return err
	}
	if err := p.syncWAL(); err != nil {
		return err
	}
	p.walFlushedLSN = maxLSN
	p.walFrames += len(images)

	if p.walFrames >= p.ckptN {
		return p.checkpointLocked()
	}
	return nil
}

// syncWAL fsyncs the WAL at the configured durability level.
func (p *Pager) syncWAL() error {
	switch p.sync {
	case SyncOff:
		return nil
	case SyncFull:
		return p.walFile.Sync(vfs.SyncFull)
	default:
		return p.walFile.Sync(vfs.SyncData)
	}
}

// Checkpoint writes every committed dirty page to the main file, fsyncs it, and
// starts a fresh WAL generation, bounding WAL growth (spec 2061 doc 05 §13).
func (p *Pager) Checkpoint() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.checkpointLocked()
}

func (p *Pager) checkpointLocked() error {
	if p.closed {
		return ErrClosed
	}
	if p.readOnly {
		return ErrReadOnly
	}
	p.pool.committedLSN = p.walFlushedLSN

	var dirty []*Frame
	p.pool.forEachResident(func(f *Frame) {
		if f.dirty && f.pageLSN <= p.walFlushedLSN {
			dirty = append(dirty, f)
		}
	})
	sortFramesByLSN(dirty)
	for _, f := range dirty {
		if err := p.writeBack(f); err != nil {
			return err
		}
	}
	if err := p.main.Sync(vfs.SyncFull); err != nil {
		return err
	}

	// Fresh generation: the committed data now lives in the main file, so the
	// old WAL can be discarded.
	if err := p.walFile.Close(); err != nil {
		return err
	}
	p.checkpointSeq++
	return p.openFreshWAL()
}

// writeBack writes one dirty frame to the main file, enforcing the write-ahead
// rule: the WAL must be durable through the page's LSN first (spec 2061 doc 05
// §3.4). It is the sole enforcement point of that rule.
func (p *Pager) writeBack(f *Frame) error {
	if p.walFlushedLSN < f.pageLSN {
		// The page is not yet durable in the WAL. In M1 this only happens for an
		// uncommitted page, which must never be stolen; refuse rather than
		// corrupt the recovery invariant.
		return fmt.Errorf("pager: write-ahead rule violation: page %d lsn %d > durable %d",
			f.PageID, f.pageLSN, p.walFlushedLSN)
	}
	off := int64(f.PageID) * int64(p.pageSize)
	if _, err := p.main.WriteAt(f.Buf, off); err != nil {
		return err
	}
	f.dirty = false
	return nil
}

// Sync fsyncs the main file. Exposed for completeness; the commit and checkpoint
// paths manage their own durability.
func (p *Pager) Sync() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	return p.main.Sync(vfs.SyncFull)
}

// Close checkpoints (flushing committed data to the main file), closes the WAL
// and main files, and marks the pager closed. After a clean Close the next Open
// finds an up-to-date main file and an empty WAL.
func (p *Pager) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	var firstErr error
	if !p.readOnly {
		if err := p.checkpointLocked(); err != nil {
			firstErr = err
		}
	}
	if p.walFile != nil {
		if err := p.walFile.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := p.main.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	p.closed = true
	return firstErr
}

// sortFramesByLSN sorts frames ascending by pageLSN with a simple insertion sort
// (the dirty set per commit is small).
func sortFramesByLSN(fs []*Frame) {
	for i := 1; i < len(fs); i++ {
		f := fs[i]
		j := i - 1
		for j >= 0 && fs[j].pageLSN > f.pageLSN {
			fs[j+1] = fs[j]
			j--
		}
		fs[j+1] = f
	}
}

func genSalt() uint32 {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("pager: CSPRNG failure: " + err.Error())
	}
	return binary.LittleEndian.Uint32(b[:])
}

func genU64() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("pager: CSPRNG failure: " + err.Error())
	}
	return binary.LittleEndian.Uint64(b[:])
}
