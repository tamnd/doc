package index

import "github.com/tamnd/doc/storage"

// Compile-time confirmation that the M1 index satisfies the storage seam.
var (
	_ storage.IndexStore = (*BTree)(nil)
	_ storage.IndexCursor = (*cursor)(nil)
	_ storage.Txn         = (*Tx)(nil)
)
