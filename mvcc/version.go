package mvcc

import "github.com/tamnd/doc/bson"

// VersionKind tags what a version did to a record (spec 2061 doc 06 §3.2).
type VersionKind uint8

const (
	// KindInsert is the first version of a record: full document, no predecessor.
	KindInsert VersionKind = iota
	// KindUpdate is a new full document superseding the prior version.
	KindUpdate
	// KindDelete is a tombstone: the record does not exist as of this version.
	KindDelete
)

// version is one entry in a record's version chain (spec 2061 doc 06 §3.1). While
// in flight it carries the writing transaction's id and a zero commit version; at
// publish the commit version is assigned and the id cleared. prev links to the
// older version, forming a newest-to-oldest chain.
type version struct {
	commitVer uint64
	txnID     uint64
	kind      VersionKind
	data      bson.Raw // nil for a Delete tombstone
	prev      *version
}

// visibleTo reports whether a committed version is visible to a snapshot at
// startVer. Pending (in-flight) versions are handled separately by the
// transaction's own read-your-writes path, so a chain entry is always committed
// (txnID 0) and the predicate reduces to the commit-version test (spec 2061 doc
// 06 §5.2).
func (v *version) visibleTo(startVer uint64) bool {
	return v.txnID == 0 && v.commitVer <= startVer
}

// VersionChain is the newest-to-oldest list of committed versions of one record.
// The head is the current version; prev pointers walk into history. All entries
// are committed; in-flight writes live in the writing transaction's pending map
// until they publish onto the head.
type VersionChain struct {
	head *version
}

// visibleAt returns the document a snapshot at startVer sees, and whether the
// record exists at that snapshot. It walks from the head to the first visible
// version: a Delete there means the record does not exist; an Insert or Update
// returns its document. Exhausting the chain means the record did not yet exist
// (spec 2061 doc 06 §5.3).
func (c *VersionChain) visibleAt(startVer uint64) (bson.Raw, bool) {
	for v := c.head; v != nil; v = v.prev {
		if !v.visibleTo(startVer) {
			continue
		}
		if v.kind == KindDelete {
			return nil, false
		}
		return v.data, true
	}
	return nil, false
}

// Len returns the number of versions in the chain. It is a test and GC-accounting
// hook, not a hot-path call.
func (c *VersionChain) Len() int {
	n := 0
	for v := c.head; v != nil; v = v.prev {
		n++
	}
	return n
}

// gc truncates the tail of the chain that no live snapshot can reach and reports
// whether the whole record is dead (spec 2061 doc 06 §14.2). Every live snapshot
// has a start version at or above the watermark, so it sees the newest version
// committed at or before the watermark; every older version is invisible to all
// of them and is dropped. The kept head is that newest-at-or-below-watermark
// version (or a newer one above the watermark). A record whose surviving head is
// a tombstone committed below the watermark is dead and its slot is reclaimable.
func (c *VersionChain) gc(watermark uint64) (dead bool) {
	// Find the newest version committed at or before the watermark: it and
	// everything newer must be kept; everything older than it is unreachable.
	var keepTail *version
	for v := c.head; v != nil; v = v.prev {
		if v.commitVer <= watermark {
			keepTail = v
			break
		}
	}
	if keepTail == nil {
		// Every version is above the watermark (all committed after the oldest
		// snapshot started); nothing is collectable yet.
		return false
	}
	keepTail.prev = nil
	if keepTail == c.head && keepTail.kind == KindDelete && keepTail.commitVer < watermark {
		// The record's only reachable state is a tombstone no snapshot can see.
		return true
	}
	return false
}
