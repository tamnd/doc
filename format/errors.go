package format

import "errors"

// The sentinel errors of the file format. They correspond one-to-one with the
// failure points of the open sequence (spec 2061 doc 03 §3.4) and the page
// validation rules (§4, §12). Callers match them with errors.Is.
var (
	// ErrTooSmall reports a file shorter than the 128-byte fixed header prefix.
	ErrTooSmall = errors.New("doc/format: file smaller than header prefix")

	// ErrNotDocDB reports a 16-byte magic mismatch: the file is not a doc
	// database. It is deliberately distinct from a generic corruption error so
	// that callers can tell "wrong file type" from "right type, damaged".
	ErrNotDocDB = errors.New("doc/format: not a doc database (magic mismatch)")

	// ErrUnsupportedChecksum reports a header naming a checksum algorithm this
	// build cannot compute.
	ErrUnsupportedChecksum = errors.New("doc/format: unsupported checksum algorithm")

	// ErrHeaderCorrupt reports a header whose stored checksum does not match the
	// bytes it covers.
	ErrHeaderCorrupt = errors.New("doc/format: header checksum mismatch")

	// ErrUnsupportedMajor reports a format_major higher than this build knows
	// how to read. The file may be from a future, incompatible release.
	ErrUnsupportedMajor = errors.New("doc/format: format major version too new")

	// ErrInvalidPageSize reports a page_size that is not one of the five
	// permitted powers of two.
	ErrInvalidPageSize = errors.New("doc/format: invalid page size")

	// ErrUnsupportedFeature reports a required feature_flags bit this build does
	// not implement.
	ErrUnsupportedFeature = errors.New("doc/format: unsupported required feature flag")

	// ErrPageChecksum reports a content-page checksum mismatch detected on read.
	ErrPageChecksum = errors.New("doc/format: page checksum mismatch")

	// ErrUnknownPageType reports a page_type byte outside the defined set while
	// traversing a structure that expected a known type.
	ErrUnknownPageType = errors.New("doc/format: unknown page type")

	// ErrPageTooSmall reports an attempt to interpret a buffer shorter than the
	// configured page size as a page.
	ErrPageTooSmall = errors.New("doc/format: buffer smaller than page size")

	// ErrNoSpace reports that a slotted page lacks room for a requested cell.
	ErrNoSpace = errors.New("doc/format: insufficient free space on page")

	// ErrBadSlot reports a slot index outside a page's slot directory.
	ErrBadSlot = errors.New("doc/format: slot index out of range")

	// ErrSlotDead reports an access to a slot that holds a dead (deleted)
	// tombstone rather than a live cell.
	ErrSlotDead = errors.New("doc/format: slot is a dead tombstone")

	// ErrChainCorrupt reports an overflow chain whose links, page types, total
	// length, or end-to-end checksum are inconsistent.
	ErrChainCorrupt = errors.New("doc/format: overflow chain corrupt")
)
