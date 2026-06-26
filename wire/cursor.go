package wire

import (
	"context"
	"sync"

	"github.com/tamnd/doc"
)

// cursorStore holds the server-side cursors that outlive a single command, so a driver
// can pull a large result across getMore calls (spec 2061 doc 16 §7). Cursor id 0 is
// reserved for "no cursor"; live ids start at 1.
type cursorStore struct {
	mu   sync.Mutex
	seq  int64
	open map[int64]*serverCursor
}

// serverCursor is one open cursor: the library cursor, the namespace for the reply, and
// the connection that owns it. A cursor is pinned to its connection, so only getMore
// from the same connection can advance it (spec 2061 doc 16 §7.4).
type serverCursor struct {
	id     int64
	connID int32
	ns     string
	cur    *doc.Cursor
}

func newCursorStore() *cursorStore {
	return &cursorStore{open: make(map[int64]*serverCursor)}
}

// register stores a cursor under a freshly allocated id and returns it.
func (s *cursorStore) register(connID int32, ns string, cur *doc.Cursor) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	id := s.seq
	s.open[id] = &serverCursor{id: id, connID: connID, ns: ns, cur: cur}
	return id
}

// get looks up a cursor by id, scoped to the owning connection. A cursor owned by a
// different connection reads as not found, which the caller reports as CursorNotFound.
func (s *cursorStore) get(connID, _ int32, id int64) (*serverCursor, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.open[id]
	if !ok || c.connID != connID {
		return nil, false
	}
	return c, true
}

// remove drops a cursor from the store and returns it; the caller closes it.
func (s *cursorStore) remove(connID int32, id int64) (*serverCursor, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.open[id]
	if !ok || c.connID != connID {
		return nil, false
	}
	delete(s.open, id)
	return c, true
}

// closeForConn closes and drops every cursor owned by a connection, called when that
// connection goes away so a dropped client never leaks a cursor.
func (s *cursorStore) closeForConn(connID int32) {
	s.mu.Lock()
	var doomed []*serverCursor
	for id, c := range s.open {
		if c.connID == connID {
			doomed = append(doomed, c)
			delete(s.open, id)
		}
	}
	s.mu.Unlock()
	for _, c := range doomed {
		_ = c.cur.Close(context.Background())
	}
}

// count reports the number of open cursors, for tests and diagnostics.
func (s *cursorStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.open)
}
