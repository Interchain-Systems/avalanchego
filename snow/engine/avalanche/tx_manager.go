package avalanche

import (
	"github.com/ava-labs/avalanche-go/cache"
	"github.com/ava-labs/avalanche-go/ids"
	"github.com/ava-labs/avalanche-go/snow/consensus/snowstorm"
	"github.com/ava-labs/avalanche-go/snow/engine/avalanche/vertex"
)

// txManager implements snowstorm.TxGetter
type txManager struct {
	pinnedTxs map[[32]byte]snowstorm.Tx

	vm vertex.DAGVM

	// Cache of transactions
	// Key: Tx ID
	// Value: The transaction as a *
	txCache cache.LRU
}

// GetTx gets a tx by its ID
func (tm *txManager) GetTx(id ids.ID) (snowstorm.Tx, error) {
	// First, check the pinned txs
	if tx, ok := tm.pinnedTxs[id.Key()]; ok {
		return tx, nil
	}
	// Try the cache
	if tx, ok := tm.txCache.Get(id); ok {
		return tx.(snowstorm.Tx), nil
	}
	// Try storage
	return tm.vm.GetTx(id)
}

// SaveTx persists a tx
func (tm *txManager) SaveTx(tx snowstorm.Tx) error {
	tm.txCache.Put(tx.ID(), tx) // Put in cache
	return tm.vm.SaveTx(tx)     // Persist
}

// PinTx puts a transaction in memory, where it will stay until UnpinTx is called
func (tm *txManager) PinTx(tx snowstorm.Tx) {
	tm.pinnedTxs[tx.ID().Key()] = tx
}

// UnpinTx removes a pinned transaction from memory
func (tm *txManager) UnpinTx(id ids.ID) {
	delete(tm.pinnedTxs, id.Key())
}
