package format

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

// chainStore is a tiny page-number → buffer map used to exercise overflow chain
// assembly without a real pager.
type chainStore struct {
	pages map[uint32][]byte
}

func newChainStore() *chainStore { return &chainStore{pages: make(map[uint32][]byte)} }

func (c *chainStore) fetch(no uint32) ([]byte, error) {
	p, ok := c.pages[no]
	if !ok {
		return nil, fmt.Errorf("page %d not found", no)
	}
	return p, nil
}

func writeChain(t *testing.T, store *chainStore, payload []byte, ps uint32, algo ChecksumAlgo) uint32 {
	t.Helper()
	n := OverflowPageCount(len(payload), ps)
	bufs := make([][]byte, n)
	nos := make([]uint32, n)
	for i := 0; i < n; i++ {
		bufs[i] = make([]byte, ps)
		nos[i] = uint32(100 + i) // arbitrary, distinct page numbers
		store.pages[nos[i]] = bufs[i]
	}
	return WriteOverflowChain(payload, bufs, nos, 1, algo)
}

func TestOverflowRoundTrip(t *testing.T) {
	const ps = PageSize8K
	headCap := OverflowHeadCapacity(ps)
	contCap := OverflowContCapacity(ps)
	sizes := []int{
		0,
		1,
		headCap - 1,
		headCap,
		headCap + 1,
		headCap + contCap,
		headCap + 3*contCap + 17,
		100 * 1024,
	}
	for _, n := range sizes {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			payload := make([]byte, n)
			for i := range payload {
				payload[i] = byte(i*7 + 1)
			}
			store := newChainStore()
			head := writeChain(t, store, payload, ps, ChecksumCRC32C)
			got, err := ReadOverflowChain(head, store.fetch, ChecksumCRC32C)
			if err != nil {
				t.Fatalf("ReadOverflowChain: %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("payload mismatch for n=%d: got %d bytes", n, len(got))
			}
		})
	}
}

func TestOverflowPageCountMath(t *testing.T) {
	const ps = PageSize8K
	headCap := OverflowHeadCapacity(ps)
	contCap := OverflowContCapacity(ps)
	cases := map[int]int{
		0:                     1,
		headCap:               1,
		headCap + 1:           2,
		headCap + contCap:     2,
		headCap + contCap + 1: 3,
	}
	for n, want := range cases {
		if got := OverflowPageCount(n, ps); got != want {
			t.Errorf("OverflowPageCount(%d) = %d, want %d", n, got, want)
		}
	}
}

func TestOverflowDetectsPageCorruption(t *testing.T) {
	const ps = PageSize8K
	payload := make([]byte, 50*1024)
	for i := range payload {
		payload[i] = byte(i)
	}
	store := newChainStore()
	head := writeChain(t, store, payload, ps, ChecksumCRC32C)
	// Corrupt a byte in a continuation page body; the per-page checksum must
	// catch it.
	cont := store.pages[101]
	cont[BodyOffset+10] ^= 0xFF
	if _, err := ReadOverflowChain(head, store.fetch, ChecksumCRC32C); err == nil {
		t.Fatal("corruption of a continuation page not detected")
	}
}

func TestOverflowDetectsChainChecksumWithoutPageChecksum(t *testing.T) {
	const ps = PageSize8K
	payload := make([]byte, 30*1024)
	for i := range payload {
		payload[i] = byte(i)
	}
	store := newChainStore()
	// Use ChecksumNone so the per-page checksum does not catch the change; the
	// end-to-end chain checksum must still detect it.
	head := writeChain(t, store, payload, ps, ChecksumNone)
	cont := store.pages[101]
	cont[BodyOffset+5] ^= 0x01
	if _, err := ReadOverflowChain(head, store.fetch, ChecksumNone); !errors.Is(err, ErrChainCorrupt) {
		t.Fatalf("err = %v, want ErrChainCorrupt", err)
	}
}

func TestOverflowRejectsWrongHeadType(t *testing.T) {
	const ps = PageSize8K
	store := newChainStore()
	// A page that is not an overflow head.
	p := make([]byte, ps)
	InitPage(p, PageHeap, 1)
	WritePageChecksum(p, ChecksumCRC32C)
	store.pages[100] = p
	if _, err := ReadOverflowChain(100, store.fetch, ChecksumCRC32C); !errors.Is(err, ErrChainCorrupt) {
		t.Fatalf("err = %v, want ErrChainCorrupt", err)
	}
}

func BenchmarkOverflowWriteRead64K(b *testing.B) {
	const ps = PageSize8K
	payload := make([]byte, 64*1024)
	b.SetBytes(int64(len(payload)))
	for i := 0; i < b.N; i++ {
		store := newChainStore()
		n := OverflowPageCount(len(payload), ps)
		bufs := make([][]byte, n)
		nos := make([]uint32, n)
		for j := 0; j < n; j++ {
			bufs[j] = make([]byte, ps)
			nos[j] = uint32(100 + j)
			store.pages[nos[j]] = bufs[j]
		}
		head := WriteOverflowChain(payload, bufs, nos, 1, ChecksumCRC32C)
		if _, err := ReadOverflowChain(head, store.fetch, ChecksumCRC32C); err != nil {
			b.Fatal(err)
		}
	}
}
