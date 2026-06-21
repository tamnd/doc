package collection

import (
	"errors"
	"hash/fnv"
	"slices"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/query"
	"github.com/tamnd/doc/storage"
)

// ErrTxnDone reports an operation on a transaction that already committed or
// rolled back.
var ErrTxnDone = errors.New("collection: transaction already finished")

// Txn is a collection transaction: a snapshot taken at Begin plus a buffer of
// writes that become visible only at Commit. Reads see the snapshot and the
// transaction's own buffered writes (read-your-writes); a write conflict with a
// transaction that committed since the snapshot aborts Commit with a retriable
// error (spec 2061 doc 06 §7, §8).
type Txn struct {
	c          *Collection
	startVer   uint64
	txnID      uint64
	writable   bool
	iso        IsolationLevel
	done       bool
	committedV uint64 // commit version assigned at Commit, 0 until a write commit succeeds

	// bypassValidation suppresses validator enforcement for the writes in this
	// transaction (MongoDB's bypassDocumentValidation, spec 2061 doc 09 §10.5).
	bypassValidation bool

	pending      map[string]*pendingOp // overlay key -> buffered write
	order        []string              // pending keys in write order
	insertedRIDs map[string]storage.RID
}

// pendingOp is one document's buffered write within a transaction. It captures the
// net intent so a single _id touched several times in one transaction applies once
// at commit: removeRID tombstones the prior committed version (a delete or the old
// version of a re-insert), and insertDoc, when non-nil, is the new document.
type pendingOp struct {
	key       string
	insertDoc bson.Raw
	removeRID storage.RID
	removeDoc bson.Raw // the committed document being superseded, for secondary-index upkeep
	hasRemove bool
}

func (p *pendingOp) noop() bool { return p.insertDoc == nil && !p.hasRemove }

// SetBypassValidation toggles validator enforcement for the writes buffered in this
// transaction. The loader sets it from LoadOptions.BypassDocumentValidation so a
// bulk ingest can skip a validator it knows the source already satisfies (spec 2061
// doc 14 §19.4).
func (t *Txn) SetBypassValidation(b bool) { t.bypassValidation = b }

// InsertOne buffers an insert and returns the stored _id. The document is deep
// validated and normalized (an _id is minted when absent and moved first). A live
// document with the same _id, committed or buffered in this transaction, is
// rejected with ErrDuplicateKey.
func (t *Txn) InsertOne(d bson.Raw) (bson.RawValue, error) {
	if t.done {
		return bson.RawValue{}, ErrTxnDone
	}
	if !t.writable {
		return bson.RawValue{}, storage.ErrReadOnly
	}
	_, idv, err := t.insertBuffered(d)
	return idv, err
}

// FindOne returns a copy of the first document matching filter, or nil if none
// match. M2-c supports the empty filter (match all, natural order) and top-level
// field equality, with a fast path for an _id point lookup.
func (t *Txn) FindOne(filter bson.Raw) (bson.Raw, error) {
	if t.done {
		return nil, ErrTxnDone
	}
	_, doc, err := t.findMatch(filter)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, nil
	}
	return doc.Clone(), nil
}

// FindOptions carries the optional shaping stages of a find: a projection, a sort,
// and skip/limit bounds. The zero value is a plain filtered scan in natural order.
type FindOptions struct {
	Projection bson.Raw
	Sort       bson.Raw
	Skip       int64
	Limit      int64
}

// Find returns copies of every document matching filter in natural (first-insert)
// order.
func (t *Txn) Find(filter bson.Raw) ([]bson.Raw, error) {
	return t.FindWith(filter, FindOptions{})
}

// FindWith runs the full find pipeline through the query planner: the planner picks
// a collection scan or an index access path, then the execution engine applies the
// residual filter, sort, skip, limit, and projection in MongoDB's order (spec 2061
// doc 11 §3). Documents are returned as independent clones.
func (t *Txn) FindWith(filter bson.Raw, opts FindOptions) ([]bson.Raw, error) {
	if t.done {
		return nil, ErrTxnDone
	}
	p, err := t.buildPlan(filter, opts)
	if err != nil {
		return nil, err
	}
	return p.Execute()
}

// Explain returns the planner's explain document for a find at the given verbosity
// ("queryPlanner", "executionStats", or "allPlansExecution").
func (t *Txn) Explain(filter bson.Raw, opts FindOptions, verbosity string) (bson.Raw, error) {
	if t.done {
		return nil, ErrTxnDone
	}
	p, err := t.buildPlan(filter, opts)
	if err != nil {
		return nil, err
	}
	return p.Explain(verbosity)
}

// DeleteOne deletes the first document matching filter and returns the number
// deleted (0 or 1).
func (t *Txn) DeleteOne(filter bson.Raw) (int64, error) {
	if t.done {
		return 0, ErrTxnDone
	}
	if !t.writable {
		return 0, storage.ErrReadOnly
	}
	if t.c.policy().Capped {
		return 0, ErrCappedDelete
	}
	key, doc, err := t.findMatch(filter)
	if err != nil {
		return 0, err
	}
	if doc == nil {
		return 0, nil
	}
	t.bufferDelete(key)
	return 1, nil
}

// DeleteMany deletes every document matching filter and returns the number
// deleted.
func (t *Txn) DeleteMany(filter bson.Raw) (int64, error) {
	if t.done {
		return 0, ErrTxnDone
	}
	if !t.writable {
		return 0, storage.ErrReadOnly
	}
	if t.c.policy().Capped {
		return 0, ErrCappedDelete
	}
	m, err := compileFilter(filter)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, key := range t.scanKeys() {
		doc := t.currentDoc(key)
		if doc == nil || !m.Match(doc) {
			continue
		}
		t.bufferDelete(key)
		n++
	}
	return n, nil
}

// CountDocuments returns the number of documents matching filter.
func (t *Txn) CountDocuments(filter bson.Raw) (int64, error) {
	if t.done {
		return 0, ErrTxnDone
	}
	m, err := compileFilter(filter)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, key := range t.scanKeys() {
		t.recordRead(key)
		doc := t.currentDoc(key)
		if doc != nil && m.Match(doc) {
			n++
		}
	}
	return n, nil
}

// Commit makes the transaction's buffered writes durable and visible. A read-only
// transaction or one with no effective writes just releases its snapshot. A write
// transaction runs conflict detection and the durable apply through the oracle
// (durability before visibility), then publishes its versions into the overlay.
func (t *Txn) Commit() error {
	if t.done {
		return ErrTxnDone
	}
	if !t.writable || !t.hasWrites() {
		if t.iso == Serializable {
			t.c.orc.ReleaseSSI(t.txnID)
		}
		t.c.orc.Release(t.startVer)
		t.done = true
		t.c.gc()
		return nil
	}
	var (
		cv  uint64
		err error
	)
	if t.iso == Serializable {
		cv, err = t.c.orc.CommitSerializable(t.startVer, t.txnID, t.conflictKeys(), t.durable, t.publish)
	} else {
		cv, err = t.c.orc.Commit(t.startVer, t.conflictKeys(), t.durable, t.publish)
	}
	if err == nil {
		t.committedV = cv
	}
	t.done = true
	t.c.gc()
	return err
}

// SnapshotVersion is the commit version the transaction reads from: writes committed
// at or before it are visible, later ones are not. It is a test and observability hook
// onto the MVCC snapshot, used by the linearizability checker (spec 2061 doc 19 §19.2).
func (t *Txn) SnapshotVersion() uint64 { return t.startVer }

// CommitVersion is the version a successful write commit was assigned, or 0 for a
// read-only transaction, one with no effective writes, or one that has not committed.
func (t *Txn) CommitVersion() uint64 { return t.committedV }

// Rollback discards the transaction's buffered writes and releases its snapshot.
// Abort is free: nothing durable was written before commit, so there is nothing to
// undo (spec 2061 doc 06 §7.6).
func (t *Txn) Rollback() error {
	if t.done {
		return ErrTxnDone
	}
	if t.iso == Serializable {
		t.c.orc.ReleaseSSI(t.txnID)
	}
	t.c.orc.Release(t.startVer)
	t.done = true
	t.c.gc()
	return nil
}

// recordRead registers an overlay key in the transaction's read set when it runs
// under serializable isolation, so the oracle can detect a read-write antidependency
// between this read and a concurrent writer of the same document (spec 2061 doc 06
// §10.4). It is a no-op under snapshot isolation, so the SI read path is untouched.
func (t *Txn) recordRead(key string) {
	if t.iso != Serializable {
		return
	}
	t.c.orc.RecordRead(t.txnID, t.startVer, hashKey(key))
}

// ---- read helpers --------------------------------------------------------

// currentDoc returns the document this transaction sees for an overlay key: its
// own buffered write if it touched the key, otherwise the version visible at its
// snapshot. A nil result means no live document.
func (t *Txn) currentDoc(key string) bson.Raw {
	if p, ok := t.pending[key]; ok {
		return p.insertDoc
	}
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	if ch, ok := t.c.byID[key]; ok {
		return ch.visibleAt(t.startVer)
	}
	return nil
}

// committedVersion returns the RID and document bytes of the committed version
// visible at the snapshot. The document feeds secondary-index maintenance, which
// must delete the exact entries the superseded version produced.
func (t *Txn) committedVersion(key string) (storage.RID, bson.Raw, bool) {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	ch, ok := t.c.byID[key]
	if !ok {
		return storage.NullRID, nil, false
	}
	for _, v := range ch.versions {
		if v.commitVer <= t.startVer {
			if v.doc == nil {
				return storage.NullRID, nil, false
			}
			return v.rid, v.doc, true
		}
	}
	return storage.NullRID, nil, false
}

// findMatch returns the overlay key and document of the first match for filter,
// using the _id index fast path for a sole-_id equality filter and a natural-order
// scan otherwise.
func (t *Txn) findMatch(filter bson.Raw) (string, bson.Raw, error) {
	m, err := compileFilter(filter)
	if err != nil {
		return "", nil, err
	}
	if key, ok, kerr := idEqualityKey(filter); kerr != nil {
		return "", nil, kerr
	} else if ok {
		// A point _id read is an antidependency edge whether or not it finds a
		// document: a concurrent insert of this _id is a phantom this read missed.
		t.recordRead(key)
		if doc := t.currentDoc(key); doc != nil && m.Match(doc) {
			return key, doc, nil
		}
		return "", nil, nil
	}
	for _, key := range t.scanKeys() {
		t.recordRead(key)
		doc := t.currentDoc(key)
		if doc != nil && m.Match(doc) {
			return key, doc, nil
		}
	}
	return "", nil, nil
}

// scanKeys returns the overlay keys to walk for a natural-order scan: the
// committed keys in first-insert order, followed by keys this transaction inserted
// that are not yet committed.
func (t *Txn) scanKeys() []string {
	t.c.mu.Lock()
	keys := t.c.snapshotOrder()
	t.c.mu.Unlock()
	seen := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		seen[k] = struct{}{}
	}
	for _, k := range t.order {
		if _, ok := seen[k]; !ok {
			keys = append(keys, k)
			seen[k] = struct{}{}
		}
	}
	return keys
}

// ---- write helpers -------------------------------------------------------

func (t *Txn) ensurePending(key string) *pendingOp {
	if t.pending == nil {
		t.pending = make(map[string]*pendingOp)
	}
	p, ok := t.pending[key]
	if !ok {
		p = &pendingOp{key: key}
		t.pending[key] = p
		t.order = append(t.order, key)
	}
	return p
}

// hasWrites reports whether any buffered op has an effective durable effect.
func (t *Txn) hasWrites() bool {
	for _, key := range t.order {
		if !t.pending[key].noop() {
			return true
		}
	}
	return false
}

// conflictKeys returns the sorted, deduplicated conflict-detection keys for the
// transaction's effective writes: a 64-bit hash of each touched overlay key.
func (t *Txn) conflictKeys() []uint64 {
	set := make(map[uint64]struct{})
	for _, key := range t.order {
		if t.pending[key].noop() {
			continue
		}
		set[hashKey(key)] = struct{}{}
	}
	out := make([]uint64, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// durable applies the buffered writes to the heap and _id index stamped with the
// settled commit version, then drives one pager group commit. It runs under the
// oracle lock before the versions are published, so a failure aborts the commit
// before any version becomes visible (spec 2061 doc 06 §7.5). A failure mid-apply
// leaves the inherited M1 single-writer ceiling: the dirty pages are not undone
// (no per-transaction rollback yet), so a durable apply must be all-or-nothing in
// practice; on the fault-free conformance path it always completes.
func (t *Txn) durable(cv uint64) error {
	if err := t.apply(cv); err != nil {
		return err
	}
	return t.c.pgr.Commit()
}

// apply writes the transaction's buffered mutations into the heap and indexes
// stamped with cv, stages any dirty catalog state, but does not drive the pager
// commit. The single-collection durable path commits the pager immediately after;
// a multi-collection transaction applies every participant and then commits the
// shared pager once, so all collections become durable in one group commit (spec
// 2061 doc 06 §7.5, doc 14 §14).
func (t *Txn) apply(cv uint64) error {
	wt := writeTxn{version: cv}
	t.insertedRIDs = make(map[string]storage.RID)
	// Pre-check unique secondary indexes against the live committed state before any
	// page is mutated, so a duplicate aborts the commit without a partial write (the
	// inherited M1 ceiling has no per-transaction undo). RIDs this transaction is
	// itself removing are exempt, since their entries are deleted in the same commit.
	if err := t.c.checkUniqueSecondary(t.pending, t.order); err != nil {
		return err
	}
	for _, key := range t.order {
		p := t.pending[key]
		if p.hasRemove {
			if err := t.c.hp.Delete(wt, p.removeRID); err != nil {
				return err
			}
			if err := t.c.idx.Delete(wt, storage.IndexKey(key), p.removeRID); err != nil {
				return err
			}
			if err := t.c.deleteSecondary(wt, p.removeDoc, p.removeRID); err != nil {
				return err
			}
		}
		if p.insertDoc != nil {
			rid, err := t.c.hp.Insert(wt, p.insertDoc)
			if err != nil {
				return err
			}
			if err := t.c.idx.Put(wt, storage.IndexKey(key), rid); err != nil {
				return err
			}
			if err := t.c.insertSecondary(wt, p.insertDoc, rid); err != nil {
				return err
			}
			t.insertedRIDs[key] = rid
		}
	}
	if t.c.catalogDirty {
		if err := t.c.cat.Stage(); err != nil {
			return err
		}
		t.c.catalogDirty = false
	}
	if t.c.idRootDirty && t.c.persistExtra != nil {
		if err := t.c.persistExtra(); err != nil {
			return err
		}
		t.c.idRootDirty = false
	}
	return nil
}

// publish installs the transaction's new versions at the head of each overlay
// chain, stamped with the commit version. It runs under the oracle lock so no
// snapshot can be taken at cv until every version is in place.
func (t *Txn) publish(cv uint64) {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	for _, key := range t.order {
		p := t.pending[key]
		if p.noop() {
			continue
		}
		ch, ok := t.c.byID[key]
		if !ok {
			ch = &chain{}
			t.c.byID[key] = ch
			t.c.order = append(t.c.order, key)
		}
		nv := &docVersion{commitVer: cv, rid: storage.NullRID}
		if p.hasRemove {
			delete(t.c.ridOwner, p.removeRID)
		}
		if p.insertDoc != nil {
			nv.rid = t.insertedRIDs[key]
			nv.doc = p.insertDoc
			t.c.ridOwner[nv.rid] = key
		}
		ch.versions = append([]*docVersion{nv}, ch.versions...)
		if len(ch.versions) > 1 {
			t.c.dirty[key] = struct{}{}
		}
	}
}

// ---- filter matching -----------------------------------------------------

const idFieldName = "_id"

// compileFilter validates and compiles a filter document into a query.Matcher. A
// nil or empty filter compiles to a match-all matcher.
func compileFilter(filter bson.Raw) (*query.Matcher, error) {
	if len(filter) > 0 {
		if err := filter.WellFormed(); err != nil {
			return nil, err
		}
	}
	return query.Compile(filter)
}

// idEqualityKey reports the overlay key for a filter that is a single _id equality
// against a scalar value, enabling the index point-lookup fast path. A filter with
// more fields, a non-_id field, or an _id matched by an operator document or a
// composite value is not eligible and falls back to a scan.
func idEqualityKey(filter bson.Raw) (string, bool, error) {
	if len(filter) == 0 {
		return "", false, nil
	}
	elems, err := filter.Elements()
	if err != nil {
		return "", false, err
	}
	if len(elems) != 1 || elems[0].Key != idFieldName {
		return "", false, nil
	}
	v := elems[0].Value
	if v.Type == bson.TypeDocument || v.Type == bson.TypeArray {
		return "", false, nil
	}
	key, kerr := overlayKey(v)
	if kerr != nil {
		return "", false, kerr
	}
	return key, true, nil
}

// hashKey is the FNV-1a 64-bit hash of an overlay key, used as the oracle's
// conflict-detection key. A collision would only ever cause a spurious, retriable
// conflict, never a missed one, so it is safe for first-committer-wins.
func hashKey(key string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return h.Sum64()
}

// gc prunes overlay versions no live snapshot can still read: below the watermark
// only the newest version per chain is kept. It runs outside the oracle lock and
// visits only chains known to hold more than one version, so a read on a
// single-version collection pays nothing.
func (c *Collection) gc() {
	wm := c.orc.Watermark()
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.dirty {
		ch := c.byID[key]
		if ch == nil {
			delete(c.dirty, key)
			continue
		}
		ch.pruneBelow(wm)
		if len(ch.versions) <= 1 {
			delete(c.dirty, key)
		}
	}
}

// pruneBelow drops every version older than the newest version at or below wm,
// which is the oldest a live snapshot at wm could read; versions above wm are all
// retained.
func (c *chain) pruneBelow(wm uint64) {
	keep := len(c.versions)
	for i, v := range c.versions {
		if v.commitVer <= wm {
			keep = i + 1
			break
		}
	}
	if keep < len(c.versions) {
		c.versions = c.versions[:keep]
	}
}
