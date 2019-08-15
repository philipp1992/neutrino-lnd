package chainview

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcutil/gcs/builder"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/lightningnetwork/lnd/channeldb"
)

type lwFilterUpdate struct {
	newUtxos     []channeldb.EdgePoint
	updateHeight uint32
	done         chan struct{}
}

// CfFilteredChainView is an implementation of the FilteredChainView interface
// which is supported by an underlying Bitcoin light client which supports
// client side filtering of Golomb Coded Sets. Rather than fetching all the
// blocks, the light client is able to query filters locally, to test if an
// item in a block modifies any of our watched set of UTXOs.
type LWFilteredChainView struct {
	started int32 // To be used atomically.
	stopped int32 // To be used atomically.

	// bestHeight is the height of the latest block added to the
	// blockQueue from the onFilteredConnectedMethod. It is used to
	// determine up to what height we would need to rescan in case
	// of a filter update.
	bestHeightMtx sync.Mutex
	bestHeight    uint32

	// chainView is the active rescan which only watches our specified
	// sub-set of the UTXO set.
	chainClient *chain.LightWalletClient
	chainConn 	*chain.LightWalletConn

	// rescanErrChan is the channel that any errors encountered during the
	// rescan will be sent over.
	rescanErrChan <-chan error

	// blockEventQueue is the ordered queue used to keep the order
	// of connected and disconnected blocks sent to the reader of the
	// chainView.
	blockQueue *blockEventQueue

	// filterUpdates is a channel in which updates to the utxo filter
	// attached to this instance are sent over.
	filterUpdates chan lwFilterUpdate

	// filterBlockReqs is a channel in which requests to filter select
	// blocks will be sent over.
	filterBlockReqs chan *filterBlockReq

	// chainFilter is the
	filterMtx   sync.RWMutex
	chainFilter map[wire.OutPoint][]byte

	quit chan struct{}
	wg   sync.WaitGroup
}

// A compile time check to ensure CfFilteredChainView implements the
// chainview.FilteredChainView.
var _ FilteredChainView = (*CfFilteredChainView)(nil)

// NewCfFilteredChainView creates a new instance of the CfFilteredChainView
// which is connected to an active neutrino node.
//
// NOTE: The node should already be running and syncing before being passed into
// this function.
func NewLWfFilteredChainView(chainConn *chain.LightWalletConn) (*LWFilteredChainView, error) {

	chainview := &LWFilteredChainView{
		blockQueue:    	 newBlockEventQueue(),
		quit:          	 make(chan struct{}),
		rescanErrChan: 	 make(chan error),
		chainFilter:   	 make(map[wire.OutPoint][]byte),
		filterUpdates:   make(chan lwFilterUpdate),
		filterBlockReqs: make(chan *filterBlockReq),
		chainConn: chainConn,
		chainClient: chainConn.NewLightWalletClient(),
	}

	return chainview, nil
}

// Start kicks off the FilteredChainView implementation. This function must be
// called before any calls to UpdateFilter can be processed.
//
// NOTE: This is part of the FilteredChainView interface.
func (c *LWFilteredChainView) Start() error {
	// Already started?
	if atomic.AddInt32(&c.started, 1) != 1 {
		return nil
	}

	log.Infof("FilteredChainView starting")
	err := c.chainClient.Start()
	if err != nil {
		return err
	}

	_, bestHeight, err := c.chainClient.GetBestBlock()
	if err != nil {
		return err
	}

	c.bestHeightMtx.Lock()
	c.bestHeight = uint32(bestHeight)
	c.bestHeightMtx.Unlock()
	c.blockQueue.Start()

	c.wg.Add(1)
	go c.chainFilterer()

	return nil
}

// Stop signals all active goroutines for a graceful shutdown.
//
// NOTE: This is part of the FilteredChainView interface.
func (c *LWFilteredChainView) Stop() error {
	// Already shutting down?
	if atomic.AddInt32(&c.stopped, 1) != 1 {
		return nil
	}

	log.Infof("FilteredChainView stopping")

	close(c.quit)
	c.blockQueue.Stop()
	c.wg.Wait()

	return nil
}

// onFilteredBlockConnected is called for each block that's connected to the
// end of the main chain. Based on our current chain filter, the block may or
// may not include any relevant transactions.
func (c *LWFilteredChainView) onFilteredBlockConnected(height int32,
	header *wire.BlockHeader, txns []*btcutil.Tx) {

	mtxs := make([]*wire.MsgTx, len(txns))
	c.filterMtx.Lock()
	for i, tx := range txns {
		mtx := tx.MsgTx()
		mtxs[i] = mtx

		for _, txIn := range mtx.TxIn {
			delete(c.chainFilter, txIn.PreviousOutPoint)
		}

	}
	c.filterMtx.Unlock()

	// We record the height of the last connected block added to the
	// blockQueue such that we can scan up to this height in case of
	// a rescan. It must be protected by a mutex since a filter update
	// might be trying to read it concurrently.
	c.bestHeightMtx.Lock()
	c.bestHeight = uint32(height)
	c.bestHeightMtx.Unlock()

	block := &FilteredBlock{
		Hash:         header.BlockHash(),
		Height:       uint32(height),
		Transactions: mtxs,
	}

	c.blockQueue.Add(&blockEvent{
		eventType: connected,
		block:     block,
	})
}

// onFilteredBlockDisconnected is a callback which is executed once a block is
// disconnected from the end of the main chain.
func (c *LWFilteredChainView) onFilteredBlockDisconnected(height int32,
	header *wire.BlockHeader) {

	log.Debugf("got disconnected block at height %d: %v", height,
		header.BlockHash())

	filteredBlock := &FilteredBlock{
		Hash:   header.BlockHash(),
		Height: uint32(height),
	}

	c.blockQueue.Add(&blockEvent{
		eventType: disconnected,
		block:     filteredBlock,
	})
}

// chainFilterer is the primary coordination goroutine within the
// CfFilteredChainView. This goroutine handles errors from the running rescan.
func (c *LWFilteredChainView) chainFilterer() {
	defer c.wg.Done()


	decodeJSONBlock := func(block *btcjson.RescannedBlock,
		height uint32) (*FilteredBlock, error) {
		hash, err := chainhash.NewHashFromStr(block.Hash)
		if err != nil {
			return nil, err

		}
		txs := make([]*wire.MsgTx, 0, len(block.Transactions))
		for _, str := range block.Transactions {
			b, err := hex.DecodeString(str)
			if err != nil {
				return nil, err
			}
			tx := &wire.MsgTx{}
			err = tx.Deserialize(bytes.NewReader(b))
			if err != nil {
				return nil, err
			}
			txs = append(txs, tx)
		}
		return &FilteredBlock{
			Hash:         *hash,
			Height:       height,
			Transactions: txs,
		}, nil
	}

	for {
		select {


		case update := <-c.filterUpdates:
			// First, we'll add all the new UTXO's to the set of
			// watched UTXO's, eliminating any duplicates in the
			// process.
			log.Tracef("Updating chain filter with new UTXO's: %v",
				update.newUtxos)

			var outpoints []wire.OutPoint
			c.filterMtx.Lock()
			for _, newOp := range update.newUtxos {
				c.chainFilter[newOp.OutPoint] = newOp.FundingPkScript
				outpoints = append(outpoints, newOp.OutPoint)
			}
			c.filterMtx.Unlock()


			// Apply the new TX filter to the chain client, which
			// will cause all following notifications from and
			// calls to it return blocks filtered with the new
			// filter.
			err := c.chainClient.LoadTxFilter(false, outpoints)
			if err != nil {
				log.Errorf("Unable to update filter: %v", err)
				continue
			}

			// All blocks gotten after we loaded the filter will
			// have the filter applied, but we will need to rescan
			// the blocks up to the height of the block we last
			// added to the blockQueue.
			c.bestHeightMtx.Lock()
			bestHeight := c.bestHeight
			c.bestHeightMtx.Unlock()

			// If the update height matches our best known height,
			// then we don't need to do any rewinding.
			if update.updateHeight == bestHeight {
				continue
			}

			// Otherwise, we'll rewind the state to ensure the
			// caller doesn't miss any relevant notifications.
			// Starting from the height _after_ the update height,
			// we'll walk forwards, rescanning one block at a time
			// with the chain client applying the newly loaded
			// filter to each blocck.
			for i := update.updateHeight + 1; i < bestHeight+1; i++ {
				blockHash, err := c.chainClient.GetBlockHash(int64(i))
				if err != nil {
					log.Warnf("Unable to get block hash "+
						"for block at height %d: %v",
						i, err)
					continue
				}

				// To avoid dealing with the case where a reorg
				// is happening while we rescan, we scan one
				// block at a time, skipping blocks that might
				// have gone missing.
				rescanned, err := c.chainClient.RescanBlocks(
					[]chainhash.Hash{*blockHash},
				)
				if err != nil {
					log.Warnf("Unable to rescan block "+
						"with hash %v at height %d: %v",
						blockHash, i, err)
					continue
				}

				// If no block was returned from the rescan, it
				// means no matching transactions were found.
				if len(rescanned) != 1 {
					log.Tracef("rescan of block %v at "+
						"height=%d yielded no "+
						"transactions", blockHash, i)
					continue
				}
				decoded, err := decodeJSONBlock(
					&rescanned[0], i,
				)
				if err != nil {
					log.Errorf("Unable to decode block: %v",
						err)
					continue
				}
				c.blockQueue.Add(&blockEvent{
					eventType: connected,
					block:     decoded,
				})
			}

			// We've received a new request to manually filter a block.
		case err := <-c.rescanErrChan:
			log.Errorf("Error encountered during rescan: %v", err)
		case <-c.quit:
			return
		}
	}
}

// FilterBlock takes a block hash, and returns a FilteredBlocks which is the
// result of applying the current registered UTXO sub-set on the block
// corresponding to that block hash. If any watched UTXO's are spent by the
// selected lock, then the internal chainFilter will also be updated.
//
// NOTE: This is part of the FilteredChainView interface.
func (c *LWFilteredChainView) FilterBlock(blockHash *chainhash.Hash) (*FilteredBlock, error) {
	// First, we'll fetch the block header itself so we can obtain the
	// height which is part of our return value.
	blockHeight, err := c.chainClient.GetBlockHeight(blockHash)
	if err != nil {
		return nil, err
	}

	filteredBlock := &FilteredBlock{
		Hash:   *blockHash,
		Height: uint32(blockHeight),
	}

	// Before we can match the filter, we'll need to map each item in our
	// chain filter to the representation that included in the compact
	// filters.
	c.filterMtx.RLock()
	relevantPoints := make([][]byte, 0, len(c.chainFilter))
	for _, filterEntry := range c.chainFilter {
		relevantPoints = append(relevantPoints, filterEntry)
	}
	c.filterMtx.RUnlock()

	// If we don't have any items within our current chain filter, then we
	// can exit early as we don't need to fetch the filter.
	if len(relevantPoints) == 0 {
		return filteredBlock, nil
	}

	filter, err := c.chainClient.GetCFilter(blockHash)
	if err != nil {
		return nil, err
	}

	if filter == nil {
		return nil, fmt.Errorf("Unable to fetch filter")
	}

	// With our relevant points constructed, we can finally match against
	// the retrieved filter.
	matched, err := filter.MatchAny(builder.DeriveKey(blockHash),
		relevantPoints)

	if err != nil {
		return nil, err
	}

	// If there wasn't a match, then we'll return the filtered block as is
	// (void of any transactions).
	if !matched {
		return filteredBlock, nil
	}

	// If we reach this point, then there was a match, so we'll need to
	// fetch the block itself so we can scan it for any actual matches (as
	// there's a fp rate).
	block, err := c.chainClient.GetBlock(blockHash)
	if err != nil {
		return nil, err
	}

	// Finally, we'll step through the block, input by input, to see if any
	// transactions spend any outputs from our watched sub-set of the UTXO
	// set.
	for _, tx := range block.Transactions {
		for _, txIn := range tx.TxIn {
			prevOp := txIn.PreviousOutPoint

			c.filterMtx.RLock()
			_, ok := c.chainFilter[prevOp]
			c.filterMtx.RUnlock()

			if ok {
				filteredBlock.Transactions = append(
					filteredBlock.Transactions,
					tx,
				)

				c.filterMtx.Lock()
				delete(c.chainFilter, prevOp)
				c.filterMtx.Unlock()

				break
			}
		}
	}

	return filteredBlock, nil
}

// UpdateFilter updates the UTXO filter which is to be consulted when creating
// FilteredBlocks to be sent to subscribed clients. This method is cumulative
// meaning repeated calls to this method should _expand_ the size of the UTXO
// sub-set currently being watched.  If the set updateHeight is _lower_ than
// the best known height of the implementation, then the state should be
// rewound to ensure all relevant notifications are dispatched.
//
// NOTE: This is part of the FilteredChainView interface.
func (c *LWFilteredChainView) UpdateFilter(ops []channeldb.EdgePoint,
	updateHeight uint32) error {

	log.Tracef("Updating chain filter with new UTXO's: %v", ops)

	select {

	case c.filterUpdates <- lwFilterUpdate{
		newUtxos:     ops,
		updateHeight: updateHeight,
	}:
		return nil

	case <-c.quit:
		return fmt.Errorf("chain filter shutting down")
	}

}

// FilteredBlocks returns the channel that filtered blocks are to be sent over.
// Each time a block is connected to the end of a main chain, and appropriate
// FilteredBlock which contains the transactions which mutate our watched UTXO
// set is to be returned.
//
// NOTE: This is part of the FilteredChainView interface.
func (c *LWFilteredChainView) FilteredBlocks() <-chan *FilteredBlock {
	return c.blockQueue.newBlocks
}

// DisconnectedBlocks returns a receive only channel which will be sent upon
// with the empty filtered blocks of blocks which are disconnected from the
// main chain in the case of a re-org.
//
// NOTE: This is part of the FilteredChainView interface.
func (c *LWFilteredChainView) DisconnectedBlocks() <-chan *FilteredBlock {
	return c.blockQueue.staleBlocks
}
