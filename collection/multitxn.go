package collection

import (
	"slices"

	"github.com/tamnd/doc/mvcc"
	"github.com/tamnd/doc/pager"
)

// MultiTxn is a transaction that spans more than one collection in the same file.
// Every participating collection reads from a single shared snapshot and its
// buffered writes are applied and made durable together in one group commit, so a
// transaction touching two collections is atomic across both (spec 2061 doc 06 §7,
// doc 14 §14). It is constructed by the engine, which owns the shared oracle and
// pager.
//
// A MultiTxn is not safe for concurrent use; drive it from one goroutine, the same
// way a single-collection Txn is driven.
type MultiTxn struct {
	orc      *mvcc.Oracle
	pgr      *pager.Pager
	startVer uint64
	txnID    uint64
	iso      IsolationLevel

	subs  map[*Collection]*Txn
	order []*Collection
	done  bool
}

// NewMultiTxn opens a multi-collection transaction over the shared oracle and
// pager. It acquires one snapshot for the whole transaction; the per-collection
// sub-transactions returned by For all read from it.
func NewMultiTxn(orc *mvcc.Oracle, pgr *pager.Pager, iso IsolationLevel) *MultiTxn {
	startVer, txnID := orc.Acquire()
	if iso == Serializable {
		orc.RegisterSSI(txnID, startVer)
	}
	return &MultiTxn{
		orc:      orc,
		pgr:      pgr,
		startVer: startVer,
		txnID:    txnID,
		iso:      iso,
		subs:     make(map[*Collection]*Txn),
	}
}

// For returns the sub-transaction this multi-collection transaction uses for c,
// creating it on first use. The returned Txn exposes the full read and write
// surface; all sub-transactions share one snapshot and commit together.
func (m *MultiTxn) For(c *Collection) *Txn {
	if t, ok := m.subs[c]; ok {
		return t
	}
	t := &Txn{c: c, startVer: m.startVer, txnID: m.txnID, writable: true, iso: m.iso}
	m.subs[c] = t
	m.order = append(m.order, c)
	return t
}

// SnapshotVersion is the commit version every participating sub-transaction reads
// from.
func (m *MultiTxn) SnapshotVersion() uint64 { return m.startVer }

// hasWrites reports whether any participating sub-transaction buffered an
// effective write.
func (m *MultiTxn) hasWrites() bool {
	for _, c := range m.order {
		if m.subs[c].hasWrites() {
			return true
		}
	}
	return false
}

// combinedWriteSet gathers the conflict-detection keys of every participant into
// one sorted, deduplicated set for the oracle's first-committer-wins check.
func (m *MultiTxn) combinedWriteSet() []uint64 {
	var all []uint64
	for _, c := range m.order {
		all = append(all, m.subs[c].conflictKeys()...)
	}
	slices.Sort(all)
	return slices.Compact(all)
}

// Commit makes every participant's writes durable and visible under one commit
// version. A write-write conflict on any participant aborts the whole transaction
// with a retriable error and leaves nothing committed. A read-only transaction
// releases its snapshot without touching the pager.
func (m *MultiTxn) Commit() error {
	if m.done {
		return ErrTxnDone
	}
	m.done = true

	if !m.hasWrites() {
		if m.iso == Serializable {
			m.orc.ReleaseSSI(m.txnID)
		}
		m.orc.Release(m.startVer)
		m.gcAll()
		return nil
	}

	writeSet := m.combinedWriteSet()
	durable := func(cv uint64) error {
		for _, c := range m.order {
			if err := m.subs[c].apply(cv); err != nil {
				return err
			}
		}
		return m.pgr.Commit()
	}
	publish := func(cv uint64) {
		for _, c := range m.order {
			m.subs[c].publish(cv)
		}
	}

	var (
		cv  uint64
		err error
	)
	if m.iso == Serializable {
		cv, err = m.orc.CommitSerializable(m.startVer, m.txnID, writeSet, durable, publish)
	} else {
		cv, err = m.orc.Commit(m.startVer, writeSet, durable, publish)
	}
	if err == nil {
		for _, c := range m.order {
			c.fireChange(m.subs[c].changeRecords(), cv)
		}
	}
	m.gcAll()
	return mapCommitErr(err)
}

// Rollback discards every participant's buffered writes and releases the shared
// snapshot. Like a single-collection abort it is free: nothing durable was written
// before commit.
func (m *MultiTxn) Rollback() error {
	if m.done {
		return ErrTxnDone
	}
	m.done = true
	if m.iso == Serializable {
		m.orc.ReleaseSSI(m.txnID)
	}
	m.orc.Release(m.startVer)
	m.gcAll()
	return nil
}

// gcAll runs version garbage collection on every participating collection after the
// transaction settles, mirroring the single-collection commit path.
func (m *MultiTxn) gcAll() {
	for _, c := range m.order {
		c.gc()
	}
}
