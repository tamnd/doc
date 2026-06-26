package pager

import (
	"encoding/binary"
	"fmt"

	"github.com/tamnd/doc/format"
)

// CheckPages verifies file-level page invariants and returns one string per
// violation (spec 2061 doc 19 §17): every page in the freelist carries the free
// page type and the chain length matches the header's freelist count with no
// cycle, and, when full is true, every content page in the file passes its
// trailing checksum. A freelist page whose type is not PageFree is the signal
// that a live page and a free page overlap. It reads under the pager lock and
// mutates nothing.
func (p *Pager) CheckPages(full bool) []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	var problems []string
	freed := make(map[uint32]struct{})
	pno := p.hdr.FreelistRoot
	steps := uint32(0)
	for pno != format.NullPage && pno != 0 {
		if _, dup := freed[pno]; dup {
			problems = append(problems, fmt.Sprintf("freelist: cycle detected at page %d", pno))
			break
		}
		freed[pno] = struct{}{}
		f, err := p.fetchLocked(uint64(pno))
		if err != nil {
			problems = append(problems, fmt.Sprintf("freelist: page %d unreadable: %v", pno, err))
			break
		}
		if ty := format.DecodePageHeader(f.Buf).Type; ty != format.PageFree {
			problems = append(problems, fmt.Sprintf("freelist: page %d has type %v, expected free (a live page overlaps the freelist)", pno, ty))
		}
		next := binary.LittleEndian.Uint32(f.Buf[freelistNextOffset : freelistNextOffset+4])
		p.Unpin(f)
		pno = next
		steps++
	}
	if steps != p.hdr.FreelistPageCount {
		problems = append(problems, fmt.Sprintf("freelist: walked %d pages, header records %d", steps, p.hdr.FreelistPageCount))
	}

	if full {
		problems = append(problems, p.checkAllChecksumsLocked()...)
	}
	return problems
}

// checkAllChecksumsLocked verifies every content page's trailing checksum,
// serving each page through the buffer pool so a committed-but-uncheckpointed
// image is verified rather than a stale on-disk one. fetchLocked already rejects a
// corrupt page it reads from disk; this also re-verifies pages already resident. A
// page allocated but never written reads as all zeros and is skipped, matching the
// pager's own tolerant read path. The caller holds p.mu and should checkpoint
// first so resident pages carry their written-back checksum.
func (p *Pager) checkAllChecksumsLocked() []string {
	var problems []string
	for id := uint32(1); id < p.hdr.PageCount; id++ {
		f, err := p.fetchLocked(uint64(id))
		if err != nil {
			problems = append(problems, fmt.Sprintf("page %d: %v", id, err))
			continue
		}
		if !isZeroPage(f.Buf) {
			if vErr := format.VerifyPageChecksum(f.Buf, p.checksum); vErr != nil {
				problems = append(problems, fmt.Sprintf("page %d: %v", id, vErr))
			}
		}
		p.Unpin(f)
	}
	return problems
}

// isZeroPage reports whether buf is entirely zero, the shape of a page that was
// allocated by growing the file but has not yet been written back.
func isZeroPage(buf []byte) bool {
	for _, b := range buf {
		if b != 0 {
			return false
		}
	}
	return true
}
