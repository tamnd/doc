package pager

// pool is the buffer pool: a bounded set of frames, a page table mapping page id
// to frame, and the 2Q replacement structures (spec 2061 doc 05 §3). 2Q is
// scan-resistant: a page seen once enters the probation FIFO a1in and ages out
// without polluting the hot LRU am; a page re-referenced after eviction (its id
// still in the a1out ghost set) is promoted to am and stays resident.
//
// pool is not internally synchronized; the Pager serializes all access under its
// mutex (M1 favors a correct coarse lock over fine-grained pool latching, which
// is a later tuning lever, spec 2061 doc 05 §3.5).
type pool struct {
	pageSize int
	capacity int // maximum resident frames

	table map[uint64]*Frame // page id -> resident frame

	// 2Q lists. a1in and am are intrusive doubly-linked lists threaded through
	// Frame.prev/next; head is most-recently-added, tail is the eviction end.
	a1in list
	am   list

	// a1out is the ghost set: page ids recently evicted from a1in. A miss whose
	// id is in a1out is a second reference and is admitted straight to am. It
	// holds ids only, no buffers, bounded to ghostCap entries in FIFO order.
	a1out      map[uint64]struct{}
	a1outOrder []uint64

	kin      int // a1in target size; over this, eviction prefers a1in
	ghostCap int // maximum a1out entries

	resident  int // frames currently allocated (<= capacity)
	evictions int // victims reclaimed over the pool's life, for the cache metric

	// committedLSN is the highest durably-committed LSN, mirrored from the Pager
	// before eviction. A dirty frame whose pageLSN exceeds it holds uncommitted
	// bytes and must never be stolen to the main file (a redo-only WAL cannot
	// undo it, spec 2061 doc 05 §1.2); eviction skips such frames.
	committedLSN uint64
}

func newPool(pageSize, capacity int) *pool {
	if capacity < 2 {
		capacity = 2
	}
	kin := capacity / 4
	if kin < 1 {
		kin = 1
	}
	return &pool{
		pageSize: pageSize,
		capacity: capacity,
		table:    make(map[uint64]*Frame, capacity),
		a1out:    make(map[uint64]struct{}),
		kin:      kin,
		ghostCap: capacity / 2,
	}
}

// lookup returns the resident frame for pageID, or nil.
func (p *pool) lookup(pageID uint64) *Frame { return p.table[pageID] }

// recordHit touches a resident frame on a cache hit. A frame in am moves to the
// MRU end; a frame in a1in stays put (a1in is a FIFO, hits do not reorder it).
func (p *pool) recordHit(f *Frame) {
	if f.loc == listAm {
		p.am.remove(f)
		p.am.pushFront(f)
	}
}

// admit installs a freshly-loaded frame for pageID. A frame whose id was in the
// a1out ghost set is a second reference and goes to am; a first reference goes
// to a1in.
func (p *pool) admit(f *Frame, pageID uint64) {
	f.PageID = pageID
	p.table[pageID] = f
	if _, ghost := p.a1out[pageID]; ghost {
		p.removeGhost(pageID)
		f.loc = listAm
		p.am.pushFront(f)
	} else {
		f.loc = listA1in
		p.a1in.pushFront(f)
	}
}

// obtainFrame returns a frame ready to hold a new page. If the pool has not yet
// grown to capacity it allocates a fresh frame; otherwise it evicts a victim.
// The returned frame is unlinked from every structure and absent from the table;
// the caller fills Buf and calls admit. evicted reports a dirty victim that must
// be written back first (returned so the Pager can do I/O outside policy
// bookkeeping). ok is false when every resident frame is pinned.
func (p *pool) obtainFrame() (f *Frame, evicted *Frame, ok bool) {
	if p.resident < p.capacity {
		p.resident++
		return &Frame{Buf: make([]byte, p.pageSize)}, nil, true
	}
	v := p.selectVictim()
	if v == nil {
		return nil, nil, false
	}
	// Detach the victim from the table and lists; the caller reuses its buffer.
	delete(p.table, v.PageID)
	if v.dirty {
		// Hand the victim back so the Pager can write it through the WAL rule;
		// the Pager then calls reuseVictim once the bytes are safe.
		return nil, v, true
	}
	v.loc = listNone
	return v, nil, true
}

// reuseVictim finishes reclaiming a dirty victim after the Pager has written it
// back. The victim is now clean and detached; it becomes the fresh frame.
func (p *pool) reuseVictim(v *Frame) *Frame {
	v.dirty = false
	v.loc = listNone
	v.prev, v.next = nil, nil
	return v
}

// selectVictim picks an evictable (unpinned) frame to reclaim, preferring a1in
// when it is over its target size, otherwise the am LRU tail. It records an
// a1in eviction in the ghost set. It returns nil when no frame is evictable.
func (p *pool) selectVictim() *Frame {
	preferA1 := p.a1in.size > p.kin
	if v := p.evictFrom(preferA1); v != nil {
		return v
	}
	// Fall back to the other list if the preferred one had only pinned frames.
	return p.evictFrom(!preferA1)
}

func (p *pool) evictFrom(fromA1 bool) *Frame {
	l := &p.am
	if fromA1 {
		l = &p.a1in
	}
	for v := l.tail; v != nil; v = v.prev {
		if v.pins.Load() != 0 {
			continue
		}
		if v.dirty && v.pageLSN > p.committedLSN {
			// Uncommitted: not stealable. Skip; the owning transaction holds it
			// resident until it commits or aborts.
			continue
		}
		l.remove(v)
		if fromA1 {
			p.addGhost(v.PageID)
		}
		p.evictions++
		return v
	}
	return nil
}

func (p *pool) addGhost(pageID uint64) {
	if _, ok := p.a1out[pageID]; ok {
		return
	}
	p.a1out[pageID] = struct{}{}
	p.a1outOrder = append(p.a1outOrder, pageID)
	for len(p.a1outOrder) > p.ghostCap {
		old := p.a1outOrder[0]
		p.a1outOrder = p.a1outOrder[1:]
		delete(p.a1out, old)
	}
}

func (p *pool) removeGhost(pageID uint64) {
	delete(p.a1out, pageID)
	for i, id := range p.a1outOrder {
		if id == pageID {
			p.a1outOrder = append(p.a1outOrder[:i], p.a1outOrder[i+1:]...)
			break
		}
	}
}

// forget drops the resident frame for pageID from the page table and whichever 2Q
// list holds it, and frees its residency slot. Incremental vacuum calls it for the
// trailing pages it truncates, so the pool holds no frame for a page the file no
// longer contains. The frame must be clean and unpinned, which the truncated free
// pages always are after a checkpoint.
func (p *pool) forget(pageID uint64) {
	f := p.table[pageID]
	if f == nil {
		return
	}
	delete(p.table, pageID)
	switch f.loc {
	case listA1in:
		p.a1in.remove(f)
	case listAm:
		p.am.remove(f)
	}
	f.loc = listNone
	p.resident--
}

// forEachResident calls fn for every resident frame. Used by the checkpoint to
// scan the dirty set.
func (p *pool) forEachResident(fn func(*Frame)) {
	for _, f := range p.table {
		fn(f)
	}
}

// list is a minimal intrusive doubly-linked list of frames.
type list struct {
	head *Frame
	tail *Frame
	size int
}

func (l *list) pushFront(f *Frame) {
	f.prev = nil
	f.next = l.head
	if l.head != nil {
		l.head.prev = f
	}
	l.head = f
	if l.tail == nil {
		l.tail = f
	}
	l.size++
}

func (l *list) remove(f *Frame) {
	if f.prev != nil {
		f.prev.next = f.next
	} else {
		l.head = f.next
	}
	if f.next != nil {
		f.next.prev = f.prev
	} else {
		l.tail = f.prev
	}
	f.prev, f.next = nil, nil
	l.size--
}
