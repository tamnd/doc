package doc

import "errors"

// The error sentinels mirror mongo-go-driver so callers can use errors.Is with
// the names they already know (spec 2061 doc 14 §17.1).
var (
	// ErrNoDocuments is returned by SingleResult.Decode when the query matched
	// nothing. It is not an application error; check it with errors.Is.
	ErrNoDocuments = errors.New("mongo: no documents in result")

	// ErrNilDocument reports a nil document passed where one is required.
	ErrNilDocument = errors.New("doc: nil document")

	// ErrNilCursor reports a method called on a nil cursor.
	ErrNilCursor = errors.New("doc: nil cursor")

	// ErrClientDisconnected reports use of a session or handle whose owning
	// database has been closed.
	ErrClientDisconnected = errors.New("doc: client is disconnected")

	// ErrEmptySlice reports an empty slice passed to InsertMany or BulkWrite.
	ErrEmptySlice = errors.New("doc: empty slice")

	// ErrBusy reports the file is locked by another process and the busy timeout
	// elapsed before the lock could be acquired.
	ErrBusy = errors.New("doc: file is locked by another process")

	// ErrReadOnly reports a write attempted on a database opened read-only.
	ErrReadOnly = errors.New("doc: database is read-only")

	// ErrClosed reports use of a handle after the database was closed.
	ErrClosed = errors.New("doc: database is closed")

	// ErrOptionConflict reports a create-time option that conflicts with the
	// values already locked into an existing file header.
	ErrOptionConflict = errors.New("doc: option conflicts with existing file header")

	// ErrIndexConflict reports an index that already exists with different
	// options than requested.
	ErrIndexConflict = errors.New("doc: index already exists with different options")

	// ErrNamespaceNotFound reports an operation on a collection that does not
	// exist where existence is required.
	ErrNamespaceNotFound = errors.New("doc: namespace not found")

	// ErrNamespaceExists reports CreateCollection on a name already in use.
	ErrNamespaceExists = errors.New("doc: namespace already exists")
)
