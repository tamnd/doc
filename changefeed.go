package doc

import (
	"sync"

	"github.com/tamnd/doc/bson"
	"github.com/tamnd/doc/collection"
)

// changeFeedCapacity is how many recent events the feed keeps for late readers and
// resume. A stream that falls more than this many events behind the head can no
// longer be resumed from the buffer and is told to start over (spec 2061 doc 14 §15).
const changeFeedCapacity = 1024

// feedEvent is one change as it sits in the in-memory feed: the namespace it came
// from, the operation, the document key and images, and the position that both orders
// it and resumes from it. seq is a process-local monotonic counter, unique per event;
// cv is the transaction commit version it was produced under, reported to callers as
// the cluster time.
type feedEvent struct {
	seq    uint64
	cv     uint64
	db     string
	coll   string
	op     string
	id     bson.RawValue
	doc    bson.Raw
	before bson.Raw
}

// changeFeed is the database's in-memory change broadcaster. The engine commit path
// calls publish with one transaction's records; every live ChangeStream reads from
// the shared ring by sequence position and is woken when new events land. It is the
// pragmatic stand-in for WAL logical decoding at this milestone: events live only in
// memory and only while the process runs, but the resume-token and ordering surface
// matches what a durable feed would expose (spec 2061 doc 18 §8, doc 14 §15).
type changeFeed struct {
	mu   sync.Mutex
	seq  uint64
	ring []feedEvent
	cap  int
	subs map[*feedSub]struct{}
}

// feedSub is one stream's registration. wake is a buffered-by-one channel used as a
// condition signal: publish drops a token in without blocking, and a waiting stream
// drains it and rescans the ring.
type feedSub struct {
	wake chan struct{}
}

func newChangeFeed() *changeFeed {
	return &changeFeed{cap: changeFeedCapacity, subs: make(map[*feedSub]struct{})}
}

// publish records one committed transaction's change records into the ring and wakes
// every subscriber. It runs from the engine commit path under the engine lock, so it
// stays short: append, evict, signal. A slow stream is never blocked here; it catches
// up from the ring on its own goroutine or learns it fell too far behind.
func (f *changeFeed) publish(db, coll string, recs []collection.ChangeRecord, cv uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range recs {
		f.seq++
		f.ring = append(f.ring, feedEvent{
			seq:    f.seq,
			cv:     cv,
			db:     db,
			coll:   coll,
			op:     r.Op,
			id:     r.ID,
			doc:    r.Doc,
			before: r.Before,
		})
	}
	if over := len(f.ring) - f.cap; over > 0 {
		f.ring = f.ring[over:]
	}
	for s := range f.subs {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

// register adds a subscriber and returns it with the current head sequence, the
// position a stream watching "from now" resumes after.
func (f *changeFeed) register() (*feedSub, uint64) {
	s := &feedSub{wake: make(chan struct{}, 1)}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs[s] = struct{}{}
	return s, f.seq
}

// unregister removes a subscriber. A stream calls it from Close.
func (f *changeFeed) unregister(s *feedSub) {
	f.mu.Lock()
	delete(f.subs, s)
	f.mu.Unlock()
}

// since returns the ring events with seq greater than fromSeq, in order. missed is
// true when fromSeq points before the oldest event still buffered, meaning events were
// evicted before the caller read them and the stream can no longer resume from here.
func (f *changeFeed) since(fromSeq uint64) (evs []feedEvent, missed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ring) == 0 {
		return nil, false
	}
	if oldest := f.ring[0].seq; fromSeq+1 < oldest {
		return nil, true
	}
	for _, ev := range f.ring {
		if ev.seq > fromSeq {
			evs = append(evs, ev)
		}
	}
	return evs, false
}
