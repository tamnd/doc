package wire

import (
	"context"
	"sync"

	"github.com/tamnd/doc"
)

// cursorStore holds the server-side cursors that outlive a single command, so a driver
// can pull a large result across getMore calls (spec 2061 doc 16 §7). M8-a allocates the
// store and tears it down per connection; the read commands in M8-b populate it.
type cursorStore struct {
	mu   sync.Mutex
	open map[int64]*serverCursor
}

// serverCursor is one open cursor: the library cursor plus the connection that owns it,
// so a dropped client never leaks a cursor.
type serverCursor struct {
	connID int32
	cur    *doc.Cursor
}

func newCursorStore() *cursorStore {
	return &cursorStore{open: make(map[int64]*serverCursor)}
}

// closeForConn closes and drops every cursor owned by a connection, called when that
// connection goes away.
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
