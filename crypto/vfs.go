package crypto

import (
	"errors"
	"io"

	"github.com/tamnd/doc/vfs"
)

// FS wraps a vfs.FS and encrypts the main database file transparently. Every other path the
// pager opens (the WAL sidecar, the shared-memory file) passes through in cleartext: the WAL
// frame encryption the spec calls for (doc 17 §3.1) is the chief remaining piece of this
// slice, tracked in the M8-d notes. Because the pager checkpoints on a clean close, the
// durable .doc image is fully encrypted at rest; only an unflushed WAL tail is cleartext
// between a commit and the next checkpoint.
//
// FS sits beneath the pager, so it works in page-sized units. A logical page the pager reads
// or writes at offset i*pageSize is stored on disk in a slot of pageSize+EnvelopeOverhead
// bytes at SuperHeaderSize+i*slotSize, holding the AEAD envelope. Offsets stay stable: page
// number i always maps to the same slot, which is what the AAD binds against relocation.
type FS struct {
	inner    vfs.FS
	mainPath string
	key      KeyOption
	pageSize uint32 // the page size for a newly created file; ignored when opening an existing one
}

// NewFS wraps inner so that opens of mainPath are encrypted under key. pageSize is the page
// size for a file created through this FS; opening an existing encrypted file reads its page
// size from the stored header instead.
func NewFS(inner vfs.FS, mainPath string, key KeyOption, pageSize uint32) *FS {
	return &FS{inner: inner, mainPath: mainPath, key: key, pageSize: pageSize}
}

// Open opens path. The main database path is wrapped in an encrypting file; any other path
// is opened in cleartext.
func (f *FS) Open(path string, flags vfs.OpenFlags) (vfs.File, error) {
	inner, err := f.inner.Open(path, flags)
	if err != nil {
		return nil, err
	}
	if path != f.mainPath {
		return inner, nil
	}
	ef, err := f.wrap(inner, flags)
	if err != nil {
		_ = inner.Close()
		return nil, err
	}
	return ef, nil
}

func (f *FS) Delete(path string, syncDir bool) error         { return f.inner.Delete(path, syncDir) }
func (f *FS) Exists(path string) (bool, error)               { return f.inner.Exists(path) }
func (f *FS) ShmMap(p string, r int, c bool) ([]byte, error) { return f.inner.ShmMap(p, r, c) }

// wrap initializes a fresh encrypted file (writing the super-header) or loads an existing
// one (reading the header and unwrapping the DEK, which fails with ErrWrongKey on a bad
// key).
func (f *FS) wrap(inner vfs.File, flags vfs.OpenFlags) (*encFile, error) {
	size, err := inner.Size()
	if err != nil {
		return nil, err
	}
	if size == 0 && flags&vfs.OpenCreate != 0 && flags&vfs.OpenReadOnly == 0 {
		return f.create(inner)
	}
	return f.load(inner)
}

// create writes the super-header for a brand-new encrypted file and returns the encrypting
// file positioned over empty page space.
func (f *FS) create(inner vfs.File) (*encFile, error) {
	if f.pageSize == 0 {
		return nil, errors.New("crypto: page size required to create an encrypted file")
	}
	h, dek, err := newHeader(f.pageSize, f.key)
	if err != nil {
		return nil, err
	}
	if _, err := inner.WriteAt(h.encode(), 0); err != nil {
		return nil, err
	}
	if err := inner.Sync(vfs.SyncFull); err != nil {
		return nil, err
	}
	return newEncFile(inner, h, dek), nil
}

// load reads an existing file's super-header and unwraps the DEK.
func (f *FS) load(inner vfs.File) (*encFile, error) {
	hb := make([]byte, SuperHeaderSize)
	if _, err := inner.ReadAt(hb, 0); err != nil {
		return nil, err
	}
	h, err := decodeHeader(hb)
	if err != nil {
		return nil, err
	}
	dek, err := h.openKeys(f.key)
	if err != nil {
		return nil, err
	}
	return newEncFile(inner, h, dek), nil
}

// encFile is the encrypting view of the main database file. It translates the pager's
// page-sized logical I/O into envelope slots on the underlying file.
type encFile struct {
	inner    vfs.File
	dek      []byte
	epoch    uint32
	fileID   [16]byte
	pageSize int
	slotSize int
}

func newEncFile(inner vfs.File, h *header, dek []byte) *encFile {
	return &encFile{
		inner:    inner,
		dek:      dek,
		epoch:    h.epoch,
		fileID:   h.fileID,
		pageSize: int(h.pageSize),
		slotSize: int(h.pageSize) + EnvelopeOverhead,
	}
}

// slotOffset returns the physical offset of page i's envelope slot.
func (f *encFile) slotOffset(page int64) int64 {
	return SuperHeaderSize + page*int64(f.slotSize)
}

// ReadAt serves a logical read by decrypting the pages it spans and copying out the
// requested bytes. Reads within the file are page-bounded in practice (a full page, or the
// header prefix at offset 0); the loop handles a read that crosses a page boundary anyway.
func (f *encFile) ReadAt(p []byte, off int64) (int, error) {
	total := 0
	for total < len(p) {
		cur := off + int64(total)
		page := cur / int64(f.pageSize)
		intra := cur % int64(f.pageSize)
		plain, err := f.readPage(page)
		if err != nil {
			return total, err
		}
		n := copy(p[total:], plain[intra:])
		if n == 0 {
			return total, io.EOF
		}
		total += n
	}
	return total, nil
}

// readPage reads and decrypts one page's slot. A slot past the end of the file returns
// io.EOF; a slot that fails authentication returns ErrIntegrityViolation.
func (f *encFile) readPage(page int64) ([]byte, error) {
	env := make([]byte, f.slotSize)
	if _, err := f.inner.ReadAt(env, f.slotOffset(page)); err != nil {
		return nil, err
	}
	return openPage(f.dek, uint32(page), f.epoch, f.fileID, env)
}

// WriteAt encrypts each page of a logical write into its slot. The pager writes whole pages
// at page-aligned offsets, so a partial or unaligned write is a programming error.
func (f *encFile) WriteAt(p []byte, off int64) (int, error) {
	if off%int64(f.pageSize) != 0 || len(p)%f.pageSize != 0 {
		return 0, errors.New("crypto: encrypted write must be page aligned")
	}
	for i := 0; i < len(p); i += f.pageSize {
		page := (off + int64(i)) / int64(f.pageSize)
		env, err := sealPage(f.dek, uint32(page), f.epoch, f.fileID, p[i:i+f.pageSize])
		if err != nil {
			return i, err
		}
		if _, err := f.inner.WriteAt(env, f.slotOffset(page)); err != nil {
			return i, err
		}
	}
	return len(p), nil
}

// Truncate sets the logical size, translating it to the physical slot count plus the
// super-header.
func (f *encFile) Truncate(size int64) error {
	pages := size / int64(f.pageSize)
	return f.inner.Truncate(f.slotOffset(pages))
}

// Size reports the logical size: the number of whole page slots times the page size.
func (f *encFile) Size() (int64, error) {
	phys, err := f.inner.Size()
	if err != nil {
		return 0, err
	}
	if phys <= SuperHeaderSize {
		return 0, nil
	}
	pages := (phys - SuperHeaderSize) / int64(f.slotSize)
	return pages * int64(f.pageSize), nil
}

func (f *encFile) Sync(mode vfs.SyncMode) error { return f.inner.Sync(mode) }

// Close zeros the DEK before closing the underlying file, so the key does not linger in the
// heap after the database is closed (spec 2061 doc 17 §5.3).
func (f *encFile) Close() error {
	for i := range f.dek {
		f.dek[i] = 0
	}
	return f.inner.Close()
}
