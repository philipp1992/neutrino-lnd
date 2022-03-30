package lightwalletnotify

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/queue"
)

const (
	// notifierType uniquely identifies this concrete implementation of the
	// ChainNotifier interface.
	notifierType = "lightwallet"
)

// chainUpdate encapsulates an update to the current main chain. This struct is
// used as an element within an unbounded queue in order to avoid blocking the
// main rpc dispatch rule.
type chainUpdate struct {
	blockHash   *chainhash.Hash
	blockHeight int32
}

// TODO(roasbeef): generalize struct below:
//  * move chans to config
//  * extract common code
//  * allow outside callers to handle send conditions

// LightWalletNotifier implements the ChainNotifier interface using a bitcoind
// chain client. Multiple concurrent clients are supported. All notifications
// are achieved via non-blocking sends on client channels.
type LightWalletNotifier struct {
	confClientCounter  uint64 // To be used atomically.
	spendClientCounter uint64 // To be used atomically.
	epochClientCounter uint64 // To be used atomically.

	started int32 // To be used atomically.
	stopped int32 // To be used atomically.

	chainConn   *chain.LightWalletClient
	chainParams *chaincfg.Params

	notificationCancels  chan interface{}
	notificationRegistry chan interface{}

	txNotifier *chainntnfs.TxNotifier

	blockEpochClients map[uint64]*blockEpochRegistration

	bestBlockMtx sync.RWMutex
	bestBlock chainntnfs.BlockEpoch

	rescanErr <-chan error

	// spendHintCache is a cache used to query and update the latest height
	// hints for an outpoint. Each height hint represents the earliest
	// height at which the outpoint could have been spent within the chain.
	spendHintCache chainntnfs.SpendHintCache

	// confirmHintCache is a cache used to query the latest height hints for
	// a transaction. Each height hint represents the earliest height at
	// which the transaction could have confirmed within the chain.
	confirmHintCache chainntnfs.ConfirmHintCache

	wg   sync.WaitGroup
	quit chan struct{}
}

// Ensure LightWalletNotifier implements the ChainNotifier interface at compile
// time.
var _ chainntnfs.ChainNotifier = (*LightWalletNotifier)(nil)

// New returns a new LightWalletNotifier instance. This function assumes the
// bitcoind node detailed in the passed configuration is already running, and
// willing to accept RPC requests and new zmq clients.
func New(chainConn *chain.LightWalletConn, chainParams *chaincfg.Params,
	spendHintCache chainntnfs.SpendHintCache,
	confirmHintCache chainntnfs.ConfirmHintCache) *LightWalletNotifier {

	notifier := &LightWalletNotifier{
		chainParams: chainParams,

		notificationCancels:  make(chan interface{}),
		notificationRegistry: make(chan interface{}),

		blockEpochClients: make(map[uint64]*blockEpochRegistration),

		spendHintCache:   spendHintCache,
		confirmHintCache: confirmHintCache,

		quit: make(chan struct{}),
	}

	notifier.chainConn = chainConn.NewLightWalletClient()

	return notifier
}

// Start connects to the running bitcoind node over websockets, registers for
// block notifications, and finally launches all related helper goroutines.
func (b *LightWalletNotifier) Start() error {
	// Already started?
	if atomic.AddInt32(&b.started, 1) != 1 {
		return nil
	}

	// Connect to lightwallet, and register for notifications on connected,
	// and disconnected blocks.

	b.chainConn.Start()

	if err := b.chainConn.NotifyBlocks(); err != nil {
		return err
	}

	currentHash, currentHeight, err := b.chainConn.GetBestBlock()
	if err != nil {
		return err
	}

	b.txNotifier = chainntnfs.NewTxNotifier(
		uint32(currentHeight), chainntnfs.ReorgSafetyLimit,
		b.confirmHintCache, b.spendHintCache,
	)

	b.bestBlock = chainntnfs.BlockEpoch{
		Height: currentHeight,
		Hash:   currentHash,
	}

	b.wg.Add(1)
	go b.notificationDispatcher()

	return nil
}

// Stop shutsdown the LightWalletNotifier.
func (b *LightWalletNotifier) Stop() error {
	// Already shutting down?
	if atomic.AddInt32(&b.stopped, 1) != 1 {
		return nil
	}

	// Shutdown the rpc client, this gracefully disconnects from bitcoind,
	// and cleans up all related resources.
	b.chainConn.Stop()

	close(b.quit)
	b.wg.Wait()

	// Notify all pending clients of our shutdown by closing the related
	// notification channels.
	for _, epochClient := range b.blockEpochClients {
		close(epochClient.cancelChan)
		epochClient.wg.Wait()

		close(epochClient.epochChan)
	}
	b.txNotifier.TearDown()

	return nil
}

// Started returns true if this instance has been started, and false otherwise.
func (b *LightWalletNotifier) Started() bool {
	return atomic.LoadInt32(&b.started) != 0
}

// mock currently unused variables
func Mock(vals ...interface{}) {
	for _, val := range vals {
		_ = val
	}
}

// notificationDispatcher is the primary goroutine which handles client
// notification registrations, as well as notification dispatches.
func (b *LightWalletNotifier) notificationDispatcher() {
out:
	for {
		select {
		case cancelMsg := <-b.notificationCancels:
			switch msg := cancelMsg.(type) {
			case *epochCancel:
				chainntnfs.Log.Infof("Cancelling epoch "+
					"notification, epoch_id=%v", msg.epochID)

				// First, we'll lookup the original
				// registration in order to stop the active
				// queue goroutine.
				reg := b.blockEpochClients[msg.epochID]
				reg.epochQueue.Stop()

				// Next, close the cancel channel for this
				// specific client, and wait for the client to
				// exit.
				close(b.blockEpochClients[msg.epochID].cancelChan)
				b.blockEpochClients[msg.epochID].wg.Wait()

				// Once the client has exited, we can then
				// safely close the channel used to send epoch
				// notifications, in order to notify any
				// listeners that the intent has been
				// cancelled.
				close(b.blockEpochClients[msg.epochID].epochChan)
				delete(b.blockEpochClients, msg.epochID)

			}

		case registerMsg := <-b.notificationRegistry:
			switch msg := registerMsg.(type) {
			case *chainntnfs.HistoricalConfDispatch:
				// Look up whether the transaction is already
				// included in the active chain. We'll do this
				// in a goroutine to prevent blocking
				// potentially long rescans.
				b.wg.Add(1)
				go func() {
					defer b.wg.Done()

					confDetails, err := b.historicalConfDetails(
						msg.ConfRequest,
						msg.StartHeight, msg.EndHeight,
					)
					if err != nil {
						chainntnfs.Log.Errorf("Rescan to "+
							"determine the conf "+
							"details of %v within "+
							"range %d-%d failed: %v",
							msg.ConfRequest,
							msg.StartHeight,
							msg.EndHeight, err)
						return
					}

					// If the historical dispatch finished
					// without error, we will invoke
					// UpdateConfDetails even if none were
					// found. This allows the notifier to
					// begin safely updating the height hint
					// cache at tip, since any pending
					// rescans have now completed.
					err = b.txNotifier.UpdateConfDetails(
						msg.ConfRequest, confDetails,
					)
					if err != nil {
						chainntnfs.Log.Errorf("Unable "+
							"to update conf "+
							"details of %v: %v",
							msg.ConfRequest, err)
					}
				}()

			case *chainntnfs.HistoricalSpendDispatch:
				chainntnfs.Log.Infof("New HistoricalSpendDispatch received")
				b.wg.Add(1)
				go func() {
					defer b.wg.Done()

					spendDetails, err := b.historicalSpendDetails(
						msg.SpendRequest,
						msg.StartHeight, msg.EndHeight,
					)
					if err != nil {
						chainntnfs.Log.Errorf("Rescan to "+
							"determine the spend "+
							"details of %v within "+
							"range %d-%d failed: %v",
							msg.SpendRequest,
							msg.StartHeight,
							msg.EndHeight, err)
						return
					}

					// If the historical dispatch finished
					// without error, we will invoke
					// UpdateSpendDetails even if none were
					// found. This allows the notifier to
					// begin safely updating the height hint
					// cache at tip, since any pending
					// rescans have now completed.
					err = b.txNotifier.UpdateSpendDetails(
						msg.SpendRequest, spendDetails,
					)
					if err != nil {
						chainntnfs.Log.Errorf("Unable "+
							"to update spend "+
							"details of %v: %v",
							msg.SpendRequest, err)
					}
				}()

			case *blockEpochRegistration:
				chainntnfs.Log.Infof("New block epoch subscription")

				b.blockEpochClients[msg.epochID] = msg

				// If the client did not provide their best
				// known block, then we'll immediately dispatch
				// a notification for the current tip.
				if msg.bestBlock == nil {
					b.notifyBlockEpochClient(
						msg, b.bestBlock.Height,
						b.bestBlock.Hash,
					)

					msg.errorChan <- nil
					continue
				}

				// Otherwise, we'll attempt to deliver the
				// backlog of notifications from their best
				// known block.
				b.bestBlockMtx.Lock()
				bestHeight := b.bestBlock.Height
				b.bestBlockMtx.Unlock()

				missedBlocks, err := chainntnfs.GetClientMissedBlocks(
					b.chainConn, msg.bestBlock, bestHeight,
					false,
				)
				if err != nil {
					msg.errorChan <- err
					continue
				}

				for _, block := range missedBlocks {
					b.notifyBlockEpochClient(
						msg, block.Height, block.Hash,
					)
				}

				msg.errorChan <- nil
			}

		case ntfn := <-b.chainConn.Notifications():
			switch item := ntfn.(type) {
			case chain.BlockConnected:
				blockHeader, err :=
					b.chainConn.GetBlockHeader(&item.Hash)
				if err != nil {
					chainntnfs.Log.Errorf("Unable to fetch "+
						"block header: %v", err)
					continue
				}

				if blockHeader.PrevBlock != *b.bestBlock.Hash {
					// Handle the case where the notifier
					// missed some blocks from its chain
					// backend.
					chainntnfs.Log.Infof("Missed blocks, " +
						"attempting to catch up")
					newBestBlock, missedBlocks, err :=
						chainntnfs.HandleMissedBlocks(
							b.chainConn,
							b.txNotifier,
							b.bestBlock, item.Height,
							true,
						)

					if err != nil {
						// Set the bestBlock here in case
						// a catch up partially completed.
						b.bestBlock = newBestBlock
						chainntnfs.Log.Error(err)
						continue
					}

					for _, block := range missedBlocks {
						err := b.handleBlockConnected(block)
						if err != nil {
							chainntnfs.Log.Error(err)
							continue out
						}
					}
				}

				newBlock := chainntnfs.BlockEpoch{
					Height: item.Height,
					Hash:   &item.Hash,
				}
				if err := b.handleBlockConnected(newBlock); err != nil {
					chainntnfs.Log.Error(err)
				}

				continue

			case chain.BlockDisconnected:
				chainntnfs.Log.Infof("New BlockDisconnected received")

			case chain.RelevantTx:

				// We only care about notifying on confirmed
				// spends, so if this is a mempool spend, we can
				// ignore it and wait for the spend to appear in
				// on-chain.
				if item.Block == nil {
					continue
				}

				tx := btcutil.NewTx(&item.TxRecord.MsgTx)
				err := b.txNotifier.ProcessRelevantSpendTx(
					tx, uint32(item.Block.Height),
				)
				if err != nil {
					chainntnfs.Log.Errorf("Unable to "+
						"process transaction %v: %v",
						tx.Hash(), err)
				}

			}

		case <-b.quit:
			break out
		}
	}
	b.wg.Done()
}

// historicalConfDetails looks up whether a confirmation request (txid/output
// script) has already been included in a block in the active chain and, if so,
// returns details about said block.
func (b *LightWalletNotifier) historicalConfDetails(confRequest chainntnfs.ConfRequest,
	startHeight, endHeight uint32) (*chainntnfs.TxConfirmation, error) {

	chainntnfs.Log.Infof("Requested historicalConfDetails for txid=%v, startHeight=%v, endHeight=%v",
		confRequest.TxID.String(), startHeight, endHeight)

	txConf, blockHash, blockHeight, err := b.chainConn.GetRawTransactionVerbose(&confRequest.TxID)

	if err == nil {

		if confRequest.MatchesTx(txConf.MsgTx()) {
			return &chainntnfs.TxConfirmation{
				BlockHeight: blockHeight,
				BlockHash: blockHash,
				TxIndex: uint32(txConf.Index()),
				Tx: txConf.MsgTx(),
			}, nil
		}

	}

	// Starting from the height hint, we'll walk forwards in the chain to
	// see if this transaction/output script has already been confirmed.
	for scanHeight := endHeight; scanHeight >= startHeight && scanHeight >0; scanHeight-- {
		// Ensure we haven't been requested to shut down before
		// processing the next height.
		select {
		case <-b.quit:
			return nil, chainntnfs.ErrChainNotifierShuttingDown
		default:
		}

		// First, we'll fetch the block header for this height so we
		// can compute the current block hash.
		blockHash, err := b.chainConn.GetBlockHash(int64(scanHeight))
		if err != nil {
			return nil, fmt.Errorf("unable to get header for height=%v: %v",
				scanHeight, err)
		}

		//regFilter, err := b.chainConn.GetCFilter(blockHash)
		//
		//bytes, _ := regFilter.Bytes()
		//fmt.Printf("\nRegFilter %d\n", bytes)
		//
		//if err != nil {
		//	return nil, fmt.Errorf("unable to retrieve regular filter for "+
		//		"height=%v: %v", scanHeight, err)
		//}

		// If the block has no transactions other than the Coinbase
		// transaction, then the filter may be nil, so we'll continue
		// forward int that case.
		//if regFilter == nil {
		//	continue
		//}
		//
		//// In the case that the filter exists, we'll attempt to see if
		//// any element in it matches our target public key script.
		//key := builder.DeriveKey(blockHash)
		//match, err := regFilter.Match(key, confRequest.PkScript.Script())
		//if err != nil {
		//	return nil, fmt.Errorf("unable to query filter: %v", err)
		//}
		//
		//// If there's no match, then we can continue forward to the
		//// next block.
		//if !match {
		//	fmt.Printf("\n***************Filter Matched**************\n")
		//	continue
		//}

		// In the case that we do have a match, we'll fetch the block
		// from the network so we can find the positional data required
		// to send the proper response.
		block, err := b.chainConn.GetBlock(blockHash)
		if err != nil {
			return nil, fmt.Errorf("unable to get block from network: %v", err)
		}

		// For every transaction in the block, check which one matches
		// our request. If we find one that does, we can dispatch its
		// confirmation details.
		for i, tx := range block.Transactions {
			if !confRequest.MatchesTx(tx) {
				continue
			}

			return &chainntnfs.TxConfirmation{
				Tx:          tx,
				BlockHash:   blockHash,
				BlockHeight: scanHeight,
				TxIndex:     uint32(i),
			}, nil
		}
	}

	return nil, nil
}

// confDetailsManually looks up whether a transaction/output script has already
// been included in a block in the active chain by scanning the chain's blocks
// within the given range. If the transaction/output script is found, its
// confirmation details are returned. Otherwise, nil is returned.
func (b *LightWalletNotifier) confDetailsManually(confRequest chainntnfs.ConfRequest,
	heightHint, currentHeight uint32) (*chainntnfs.TxConfirmation,
	chainntnfs.TxConfStatus, error) {

	// Begin scanning blocks at every height to determine where the
	// transaction was included in.
	for height := currentHeight; height >= heightHint && height > 0; height-- {
		// Ensure we haven't been requested to shut down before
		// processing the next height.
		select {
		case <-b.quit:
			return nil, chainntnfs.TxNotFoundManually,
				chainntnfs.ErrChainNotifierShuttingDown
		default:
		}

		blockHash, err := b.chainConn.GetBlockHash(int64(height))
		if err != nil {
			return nil, chainntnfs.TxNotFoundManually,
				fmt.Errorf("unable to get hash from block "+
					"with height %d", height)
		}

		block, err := b.chainConn.GetBlock(blockHash)
		if err != nil {
			return nil, chainntnfs.TxNotFoundManually,
				fmt.Errorf("unable to get block with hash "+
					"%v: %v", blockHash, err)
		}

		// For every transaction in the block, check which one matches
		// our request. If we find one that does, we can dispatch its
		// confirmation details.
		for txIndex, tx := range block.Transactions {
			if !confRequest.MatchesTx(tx) {
				continue
			}

			return &chainntnfs.TxConfirmation{
				Tx:          tx,
				BlockHash:   blockHash,
				BlockHeight: height,
				TxIndex:     uint32(txIndex),
			}, chainntnfs.TxFoundManually, nil
		}
	}

	// If we reach here, then we were not able to find the transaction
	// within a block, so we avoid returning an error.
	return nil, chainntnfs.TxNotFoundManually, nil
}

// handleBlockConnected applies a chain update for a new block. Any watched
// transactions included this block will processed to either send notifications
// now or after numConfirmations confs.
func (b *LightWalletNotifier) handleBlockConnected(block chainntnfs.BlockEpoch) error {
	// First, we'll fetch the raw block as we'll need to gather all the
	// transactions to determine whether any are relevant to our registered
	// clients.
	rawBlock, err := b.chainConn.GetBlock(block.Hash)
	if err != nil {
		return fmt.Errorf("unable to get block transactions: %v", err)
	}

	txns := btcutil.NewBlock(rawBlock).Transactions()

	// We'll then extend the txNotifier's height with the information of
	// this new block, which will handle all of the notification logic for
	// us.
	err = b.txNotifier.ConnectTip(block.Hash, uint32(block.Height), txns)
	if err != nil {
		return fmt.Errorf("unable to connect tip: %v", err)
	}

	chainntnfs.Log.Infof("New block: height=%v, sha=%v", block.Height,
		block.Hash)

	// Now that we've guaranteed the new block extends the txNotifier's
	// current tip, we'll proceed to dispatch notifications to all of our
	// registered clients whom have had notifications fulfilled. Before
	// doing so, we'll make sure update our in memory state in order to
	// satisfy any client requests based upon the new block.
	b.bestBlock = block

	b.notifyBlockEpochs(block.Height, block.Hash)
	return b.txNotifier.NotifyHeight(uint32(block.Height))
}

// notifyBlockEpochs notifies all registered block epoch clients of the newly
// connected block to the main chain.
func (b *LightWalletNotifier) notifyBlockEpochs(newHeight int32, newSha *chainhash.Hash) {
	for _, client := range b.blockEpochClients {
		b.notifyBlockEpochClient(client, newHeight, newSha)
	}
}

// notifyBlockEpochClient sends a registered block epoch client a notification
// about a specific block.
func (b *LightWalletNotifier) notifyBlockEpochClient(epochClient *blockEpochRegistration,
	height int32, sha *chainhash.Hash) {

	epoch := &chainntnfs.BlockEpoch{
		Height: height,
		Hash:   sha,
	}

	select {
	case epochClient.epochQueue.ChanIn() <- epoch:
	case <-epochClient.cancelChan:
	case <-b.quit:
	}
}

// RegisterSpendNtfn registers an intent to be notified once the target
// outpoint/output script has been spent by a transaction on-chain. When
// intending to be notified of the spend of an output script, a nil outpoint
// must be used. The heightHint should represent the earliest height in the
// chain of the transaction that spent the outpoint/output script.
//
// Once a spend of has been detected, the details of the spending event will be
// sent across the 'Spend' channel.
func (b *LightWalletNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint,
	pkScript []byte, heightHint uint32) (*chainntnfs.SpendEvent, error) {

	ntfn, err := b.txNotifier.RegisterSpend(outpoint, pkScript, heightHint)
	if err != nil {
		return nil, err
	}

	// We'll then request the backend to notify us when it has detected the
	// outpoint/output script as spent.
	//
	// TODO(wilmer): use LoadFilter API instead.
	if outpoint == nil || *outpoint == chainntnfs.ZeroOutPoint {
		_, addrs, _, err := txscript.ExtractPkScriptAddrs(
			pkScript, b.chainParams,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to parse script: %v", err)
		}
		if err := b.chainConn.NotifyReceived(addrs); err != nil {
			return nil, err
		}
	} else {
		ops := []*wire.OutPoint{outpoint}
		if err := b.chainConn.NotifySpent(ops); err != nil {
			return nil, err
		}
	}

	if ntfn.HistoricalDispatch == nil {
		return ntfn.Event, nil
	}

	// When dispatching spends of outpoints, there are a number of checks we
	// can make to start our rescan from a better height or completely avoid
	// it.
	//
	// We'll start by checking the backend's UTXO set to determine whether
	// the outpoint has been spent. If it hasn't, we can return to the
	// caller as well.
	txOut, err := b.chainConn.GetUnspentOutput(
		&outpoint.Hash, outpoint.Index)
	if err != nil {
		return nil, err
	}
	if txOut != nil {
		// We'll let the txNotifier know the outpoint is still unspent
		// in order to begin updating its spend hint.

		err = b.txNotifier.UpdateSpendDetails(ntfn.HistoricalDispatch.SpendRequest, nil)
		if err != nil {
			return nil, err
		}

		return ntfn.Event, nil
	} else {
		tx, height, err := b.chainConn.GetSpendingDetails(&outpoint.Hash, outpoint.Index)
		if err != nil && strings.Contains(err.Error(), "output_unspent") {
			err = b.txNotifier.UpdateSpendDetails(ntfn.HistoricalDispatch.SpendRequest, nil)
			if err != nil {
				return nil, err
			}

			return ntfn.Event, nil
		}

		if err != nil && !strings.Contains(err.Error(), "output_unspent") {
			return nil, err
		}
		var inputIndex uint32
		for i, input := range tx.MsgTx().TxIn {
			if input.PreviousOutPoint.Hash.IsEqual(&outpoint.Hash) {
				inputIndex = uint32(i)
			}
		}

		details := &chainntnfs.SpendDetail{
			SpentOutPoint:     outpoint,
			SpenderTxHash:     tx.Hash(),
			SpendingTx:        tx.MsgTx(),
			SpenderInputIndex: inputIndex,
			SpendingHeight:    height,
		}

		err = b.txNotifier.UpdateSpendDetails(ntfn.HistoricalDispatch.SpendRequest, details)
		if err != nil {
			return nil, err
		}

		return ntfn.Event, nil
	}
}

// historicalSpendDetails attempts to manually scan the chain within the given
// height range for a transaction that spends the given outpoint/output script.
// If one is found, the spend details are assembled and returned to the caller.
// If the spend is not found, a nil spend detail will be returned.
func (b *LightWalletNotifier) historicalSpendDetails(
	spendRequest chainntnfs.SpendRequest, startHeight, endHeight uint32) (
	*chainntnfs.SpendDetail, error) {

	// Begin scanning blocks at every height to determine if the outpoint
	// was spent.
	for height := endHeight; height >= startHeight && height > 0; height-- {
		// Ensure we haven't been requested to shut down before
		// processing the next height.
		select {
		case <-b.quit:
			return nil, chainntnfs.ErrChainNotifierShuttingDown
		default:
		}

		blockHash, err := retryGetBlockHashAttempt(5, 3, func() (*chainhash.Hash, error) {

			// First, we'll fetch the block for the current height.
			blockHash, err := b.chainConn.GetBlockHash(int64(height))
			if err != nil {
				return nil, fmt.Errorf("unable to retrieve hash for "+
					"block with height %d: %v", height, err)
			}

			return blockHash, nil
		})
		if err != nil {
			return nil, err
		}

		block, err := retryGetBlockAttempt(5, 3, func() (*wire.MsgBlock, error) {

			block, err := b.chainConn.GetBlock(blockHash)
			if err != nil {
				return nil, fmt.Errorf("unable to retrieve block "+
					"with hash %v: %v", blockHash, err)
			}

			return block, nil
		})
		if err != nil {
			return nil, err
		}

		// Then, we'll manually go over every input in every transaction
		// in it and determine whether it spends the request in
		// question. If we find one, we'll dispatch the spend details.
		for _, tx := range block.Transactions {
			matches, inputIdx, err := spendRequest.MatchesTx(tx)
			if err != nil {
				return nil, err
			}
			if !matches {
				continue
			}

			txHash := tx.TxHash()
			return &chainntnfs.SpendDetail{
				SpentOutPoint:     &tx.TxIn[inputIdx].PreviousOutPoint,
				SpenderTxHash:     &txHash,
				SpendingTx:        tx,
				SpenderInputIndex: inputIdx,
				SpendingHeight:    int32(height),
			}, nil
		}
	}

	return nil, nil
}

// RegisterConfirmationsNtfn registers an intent to be notified once the target
// txid/output script has reached numConfs confirmations on-chain. When
// intending to be notified of the confirmation of an output script, a nil txid
// must be used. The heightHint should represent the earliest height at which
// the txid/output script could have been included in the chain.
//
// Progress on the number of confirmations left can be read from the 'Updates'
// channel. Once it has reached all of its confirmations, a notification will be
// sent across the 'Confirmed' channel.
func (b *LightWalletNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	pkScript []byte,
	numConfs, heightHint uint32) (*chainntnfs.ConfirmationEvent, error) {

	// Register the conf notification with the TxNotifier. A non-nil value
	// for `dispatch` will be returned if we are required to perform a
	// manual scan for the confirmation. Otherwise the notifier will begin
	// watching at tip for the transaction to confirm.
	ntfn, err := b.txNotifier.RegisterConf(
		txid, pkScript, numConfs, heightHint,
	)
	if err != nil {
		return nil, err
	}

	if ntfn.HistoricalDispatch == nil {
		return ntfn.Event, nil
	}

	txids := []chainhash.Hash{*txid}

	if err := b.chainConn.NotifyTx(txids); err != nil {
		return nil, err
	}

	select {
	case b.notificationRegistry <- ntfn.HistoricalDispatch:
		return ntfn.Event, nil
	case <-b.quit:
		return nil, chainntnfs.ErrChainNotifierShuttingDown
	}
}

// blockEpochRegistration represents a client's intent to receive a
// notification with each newly connected block.
type blockEpochRegistration struct {
	epochID uint64

	epochChan chan *chainntnfs.BlockEpoch

	epochQueue *queue.ConcurrentQueue

	bestBlock *chainntnfs.BlockEpoch

	errorChan chan error

	cancelChan chan struct{}

	wg sync.WaitGroup
}

// epochCancel is a message sent to the LightWalletNotifier when a client wishes
// to cancel an outstanding epoch notification that has yet to be dispatched.
type epochCancel struct {
	epochID uint64
}

// RegisterBlockEpochNtfn returns a BlockEpochEvent which subscribes the
// caller to receive notifications, of each new block connected to the main
// chain. Clients have the option of passing in their best known block, which
// the notifier uses to check if they are behind on blocks and catch them up. If
// they do not provide one, then a notification will be dispatched immediately
// for the current tip of the chain upon a successful registration.
func (b *LightWalletNotifier) RegisterBlockEpochNtfn(
	bestBlock *chainntnfs.BlockEpoch) (*chainntnfs.BlockEpochEvent, error) {

	reg := &blockEpochRegistration{
		epochQueue: queue.NewConcurrentQueue(20),
		epochChan:  make(chan *chainntnfs.BlockEpoch, 20),
		cancelChan: make(chan struct{}),
		epochID:    atomic.AddUint64(&b.epochClientCounter, 1),
		bestBlock:  bestBlock,
		errorChan:  make(chan error, 1),
	}
	reg.epochQueue.Start()

	// Before we send the request to the main goroutine, we'll launch a new
	// goroutine to proxy items added to our queue to the client itself.
	// This ensures that all notifications are received *in order*.
	reg.wg.Add(1)
	go func() {
		defer reg.wg.Done()

		for {
			select {
			case ntfn := <-reg.epochQueue.ChanOut():
				blockNtfn := ntfn.(*chainntnfs.BlockEpoch)
				select {
				case reg.epochChan <- blockNtfn:
				case <-reg.cancelChan:
					return

				case <-b.quit:
					return
				}

			case <-reg.cancelChan:
				return

			case <-b.quit:
				return
			}
		}
	}()

	select {
	case <-b.quit:
		// As we're exiting before the registration could be sent,
		// we'll stop the queue now ourselves.
		reg.epochQueue.Stop()
		return nil, errors.New("chainntnfs: system interrupt while " +
			"attempting to register for block epoch notification.")

	case b.notificationRegistry <- reg:
		return &chainntnfs.BlockEpochEvent {
			Epochs: reg.epochChan,
			Cancel: func() {
				cancel := &epochCancel{
					epochID: reg.epochID,
				}

				// Submit epoch cancellation to notification dispatcher.
				select {
				case b.notificationCancels <- cancel:
					// Cancellation is being handled, drain the epoch channel until it is
					// closed before yielding to caller.
					for {
						select {
						case _, ok := <-reg.epochChan:
							if !ok {
								return
							}
						case <-b.quit:
							return
						}
					}
				case <-b.quit:
				}
			},
		}, nil
	}
}

// retryGetBlockHashAttempt will try to retry function call for attempts times,
// useful for network calls which can fail due to internet connection.
func retryGetBlockHashAttempt(attempts int, sleep time.Duration, f func() (*chainhash.Hash, error)) (hash *chainhash.Hash, err error) {
	for i := 0; ; i++ {
		hash, err = f()
		if err == nil {
			return hash, nil
		}

		if i >= (attempts - 1) {
			break
		}

		time.Sleep(sleep)

	}
	return nil, fmt.Errorf("after %d attempts, last error: %s", attempts, err)
}

// retryGetBlockAttempt will try to retry function call for attempts times,
// useful for network calls which can fail due to internet connection.
func retryGetBlockAttempt(attempts int, sleep time.Duration, f func() (*wire.MsgBlock, error)) (block *wire.MsgBlock, err error) {
	for i := 0; ; i++ {
		block, err = f()
		if err == nil {
			return block, nil
		}

		if i >= (attempts - 1) {
			break
		}

		time.Sleep(sleep)

	}
	return nil, fmt.Errorf("after %d attempts, last error: %s", attempts, err)
}
