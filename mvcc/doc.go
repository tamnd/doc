// Package mvcc is doc's concurrency-control core: snapshot isolation over
// per-document version chains, driven by a watermark oracle (spec 2061 doc 06).
//
// The Oracle is the single authority on version assignment and snapshot tracking.
// It hands out a start version at begin (the snapshot a transaction reads from)
// and a commit version at commit (the version stamped on the writes it makes
// visible), tracks the live snapshots to maintain the watermark that drives
// version garbage collection, and runs first-committer-wins write-write conflict
// detection over a pruned committed-since index (doc 06 §4, §8, §14).
//
// A Txn is the unified transaction handle that satisfies storage.Txn. It carries
// the start version, the commit version once assigned, a transaction id that
// stamps its in-flight writes, and the write set the oracle validates. A write
// transaction buffers its versions privately until commit, so it reads its own
// writes, an abort is free (nothing durable was written), and the visibility
// predicate keeps every other transaction from seeing the in-flight state.
//
// A VersionChain is the newest-to-oldest list of committed versions of one
// record. The visibility predicate selects the version a snapshot sees; GC
// truncates the tail no live snapshot can reach.
//
// Store is an in-memory MVCC record store that ties the three together: it is the
// reference engine the property tests run against and the seam the durable
// collection layer (M2-c) plugs the heap and indexes into. The oracle, the
// transaction, the chain, and the visibility predicate are storage-agnostic; the
// engine that publishes committed versions is an interface, so the same core
// drives both the in-memory store here and the WAL-backed heap that follows.
package mvcc
