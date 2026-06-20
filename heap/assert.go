package heap

import "github.com/tamnd/doc/storage"

// Compile-time assurance that Heap satisfies the storage SPI and Tx the Txn SPI.
var (
	_ storage.RecordStore = (*Heap)(nil)
	_ storage.Txn         = (*Tx)(nil)
)
