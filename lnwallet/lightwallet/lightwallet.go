package lightwallet

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"github.com/btcsuite/btcutil/hdkeychain"
	"github.com/btcsuite/btcutil/psbt"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/wallet/txauthor"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

type LightWalletController struct{
	// client is the RPC client to the bitcoind node.
	client *chain.LightWalletClient
	config btcwallet.Config
	keychain *keychain.LightWalletKeyRing
	lockedOutpoints map[wire.OutPoint]struct{}
}

type txSubscriptionClient struct {
	confirmed   chan *lnwallet.TransactionDetail
	unconfirmed chan *lnwallet.TransactionDetail

	lw 		  *chain.LightWalletClient
	netParams *chaincfg.Params

	wg   sync.WaitGroup
	quit chan struct{}
}


func (lw *LightWalletController) FetchInputInfo(prevOut *wire.OutPoint) (*lnwallet.Utxo, error) {
	utxo, err := lw.client.GetUnspentOutput(&prevOut.Hash, prevOut.Index)
	if err != nil {
		return nil, err
	}

	if utxo == nil {
		return nil, nil
	}

	pkScript, err := hex.DecodeString(utxo.ScriptPubKeyHex)
	if err != nil {
		return nil, err
	}

	amount := btcutil.Amount(utxo.Amount)

	return &lnwallet.Utxo{
		Value: amount,
		PkScript: pkScript,
	}, nil
}

func (lw *LightWalletController) ConfirmedBalance(confs int32, accountFilter string) (btcutil.Amount, error) {
	return lw.client.ChainConn.RPCClient().GetConfirmedBalance(confs)
}

func (lw *LightWalletController) NewAddress(addrType lnwallet.AddressType, change bool,
	accountName string) (btcutil.Address, error) {
	if addrType != lnwallet.WitnessPubKey {
		panic("implement me")
	}

	addrStr, err := lw.client.ChainConn.RPCClient().GetLastAddress(change)

	if err != nil {
		return nil, err
	}

	return btcutil.DecodeAddress(addrStr, lw.config.NetParams)
}

func (lw *LightWalletController) LastUnusedAddress(addrType lnwallet.AddressType, str string) (btcutil.Address, error) {
	panic("implement me")
}

func (lw *LightWalletController) IsOurAddress(a btcutil.Address) bool {
	return lw.client.ChainConn.RPCClient().IsOurAddress(a.EncodeAddress())
}

func (lw *LightWalletController) SendOutputs(outputs []*wire.TxOut,
	feeRate chainfee.SatPerKWeight, minconf int32, label string) (*wire.MsgTx, error) {
	panic("implement me SendOutputs")
}

func (lw *LightWalletController) CreateSimpleTx(outputs []*wire.TxOut, feeRate chainfee.SatPerKWeight,
	dryRun bool) (*txauthor.AuthoredTx, error) {
	panic("implement me CreateSimpleTx")
}

func (lw *LightWalletController) ListUnspentWitness(minconfirms, maxconfirms int32, accountFilter string) ([]*lnwallet.Utxo, error) {

	var addresses []string
	result, err := lw.client.ChainConn.RPCClient().ListUtxos( 2, 9999999, addresses)
	if err != nil {
		return nil, err
	}

	var utxos []*lnwallet.Utxo
	for _, utxo := range result  {


		pkScript, err := hex.DecodeString(utxo.PkScript)

		if err != nil {
			return nil, err
		}

		hash, _ := chainhash.NewHashFromStr(utxo.TxID)

		tmp := &lnwallet.Utxo {
			AddressType: lnwallet.WitnessPubKey,
			Confirmations: utxo.Confirmations,
			PkScript: pkScript,
			Value: btcutil.Amount(utxo.Amount),
			OutPoint: wire.OutPoint{
				Hash: *hash,
				Index: utxo.Vout,
			},
		}

		// Locked unspent outputs are skipped.
		if lw.LockedOutpoint(tmp.OutPoint) {
			continue
		}

		utxos = append(utxos, tmp)
	}

	return utxos, nil
}

func (lw *LightWalletController) ListTransactionDetails(startHeight, endHeight int32,
	accountFilter string) ([]*lnwallet.TransactionDetail, error) {
	panic("implement me ListTransactionDetails")
	return nil, nil
}

// LockedOutpoint returns whether an outpoint has been marked as locked and
// should not be used as an input for created transactions.
func (lw *LightWalletController) LockedOutpoint(o wire.OutPoint) bool {
	_, locked := lw.lockedOutpoints[o]
	return locked
}

func (lw *LightWalletController) LockOutpoint(o wire.OutPoint) {
	lw.lockedOutpoints[o] = struct{}{}
	lw.client.LockOutpoint(o)
}

func (lw *LightWalletController) UnlockOutpoint(o wire.OutPoint) {
	delete(lw.lockedOutpoints, o)
	lw.client.UnlockOutpoint(o)
}

func (lw *LightWalletController) PublishTransaction(tx *wire.MsgTx, label string) error {
	txid, err := lw.client.SendRawTransaction(tx, true)
	if err != nil {
		if strings.Contains(err.Error(), "missing inputs") {
			return lnwallet.ErrDoubleSpend
		}
		return err
	}

	fmt.Printf("Published transaction with txid: %v", txid)

	return err
}

func (lw *LightWalletController) SubscribeTransactions() (lnwallet.TransactionSubscription, error) {

	txClient := &txSubscriptionClient{
		confirmed:   make(chan *lnwallet.TransactionDetail),
		unconfirmed: make(chan *lnwallet.TransactionDetail),
		lw:          lw.client,
		netParams:   lw.config.NetParams,
		quit:        make(chan struct{}),
	}

	txClient.wg.Add(1)
	go txClient.notificationProxier()

	return txClient, nil
}

// ConfirmedTransactions returns a channel which will be sent on as new
// relevant transactions are confirmed.
//
// This is part of the TransactionSubscription interface.
func (t *txSubscriptionClient) ConfirmedTransactions() chan *lnwallet.TransactionDetail {
	return t.confirmed
}

// UnconfirmedTransactions returns a channel which will be sent on as
// new relevant transactions are seen within the network.
//
// This is part of the TransactionSubscription interface.
func (t *txSubscriptionClient) UnconfirmedTransactions() chan *lnwallet.TransactionDetail {
	return t.unconfirmed
}

// Cancel finalizes the subscription, cleaning up any resources allocated.
//
// This is part of the TransactionSubscription interface.
func (t *txSubscriptionClient) Cancel() {
	close(t.quit)
	t.wg.Wait()
}

// minedTransactionsToDetails is a helper function which converts a summary
// information about mined transactions to a TransactionDetail.
func minedTransactionsToDetails(currentHeight int32, blockHeader *btcjson.GetBlockHeaderVerboseResult,
	filterBlock []*wire.MsgTx) ([]*lnwallet.TransactionDetail, error) {

	details := make([]*lnwallet.TransactionDetail, 0, len(filterBlock))
	for _, tx := range filterBlock {
		txHash := tx.TxHash()
		blockHash, _ := chainhash.NewHashFromStr(blockHeader.Hash)

		var rawTx bytes.Buffer
		tx.Serialize(&rawTx)

		txDetail := &lnwallet.TransactionDetail{
			Hash:             txHash,
			NumConfirmations: currentHeight - blockHeader.Height + 1,
			BlockHash:        blockHash,
			BlockHeight:      blockHeader.Height,
			Timestamp:        blockHeader.Time,
			RawTx:            rawTx.Bytes(),
		}

		details = append(details, txDetail)
	}

	return details, nil
}

func (t *txSubscriptionClient) notificationProxier() {
out:
	for {
		select {
		case ntfn := <-t.lw.Notifications():
			switch update := ntfn.(type) {
			case chain.BlockConnected:
				// Launch a goroutine to re-package and send
				// notifications for any newly confirmed transactions.
				filterBlock, err := t.lw.GetFilterBlock(&update.Hash)
				if err != nil {
					return
				}

				blockHeader, err := t.lw.GetBlockHeaderVerbose(&update.Hash)
				if err != nil {
					return
				}

				go func() {
					details, err := minedTransactionsToDetails(update.Height, blockHeader, filterBlock)
					if err != nil {
						return
					}

					for _, d := range details {
						select {
						case t.confirmed <- d:
						case <-t.quit:
							return
						}
					}
				}()

				// Launch a goroutine to re-package and send
				// notifications for any newly unconfirmed transactions.
				//go func() {
				//	for _, tx := range txNtfn.UnminedTransactions {
				//		detail, err := unminedTransactionsToDetail(tx)
				//		if err != nil {
				//			continue
				//		}
				//
				//		select {
				//		case t.unconfirmed <- detail:
				//		case <-t.quit:
				//			return
				//		}
				//	}
				//}()
			}
		case <-t.quit:
			break out
		}
	}

	t.wg.Done()
}

func (lw *LightWalletController) IsSynced() (bool, int64, error) {

	bestBlockHash, _, err := lw.client.GetBestBlock()
	if err != nil {
		return false, 0, err
	}

	currentStamp, err := lw.client.BlockStamp()

	if err != nil {
		return false, 0, err
	}

	return *bestBlockHash == currentStamp.Hash, currentStamp.Timestamp.Unix(), nil
}

func (lw *LightWalletController) Start() error {
	lw.client.NotifyBlocks()
	return nil
}

func (lw *LightWalletController) Stop() error {
	return nil
}

func (lw *LightWalletController) BackEnd() string {
	panic("implement me")
}

// GetRecoveryInfo returns a boolean indicating whether the wallet is started
// in recovery mode. It also returns a float64, ranging from 0 to 1,
// representing the recovery progress made so far.
//
// This is a part of the WalletController interface.
func (b *LightWalletController) GetRecoveryInfo() (bool, float64, error) {
	//isRecoveryMode := true
	//progress := float64(0)
	//
	//// A zero value in RecoveryWindow indicates there is no trigger of
	//// recovery mode.
	//if b.cfg.RecoveryWindow == 0 {
	//	isRecoveryMode = false
	//	return isRecoveryMode, progress, nil
	//}
	//
	//// Query the wallet's birthday block height from db.
	//var birthdayBlock waddrmgr.BlockStamp
	//err := walletdb.View(b.db, func(tx walletdb.ReadTx) error {
	//	var err error
	//	addrmgrNs := tx.ReadBucket(waddrmgrNamespaceKey)
	//	birthdayBlock, _, err = b.wallet.Manager.BirthdayBlock(addrmgrNs)
	//	if err != nil {
	//		return err
	//	}
	//	return nil
	//})
	//
	//if err != nil {
	//	// The wallet won't start until the backend is synced, thus the birthday
	//	// block won't be set and this particular error will be returned. We'll
	//	// catch this error and return a progress of 0 instead.
	//	if waddrmgr.IsError(err, waddrmgr.ErrBirthdayBlockNotSet) {
	//		return isRecoveryMode, progress, nil
	//	}
	//
	//	return isRecoveryMode, progress, err
	//}
	//
	//// Grab the best chain state the wallet is currently aware of.
	//syncState := b.wallet.Manager.SyncedTo()
	//
	//// Next, query the chain backend to grab the info about the tip of the
	//// main chain.
	////
	//// NOTE: The actual recovery process is handled by the btcsuite/btcwallet.
	//// The process purposefully doesn't update the best height. It might create
	//// a small difference between the height queried here and the height used
	//// in the recovery process, ie, the bestHeight used here might be greater,
	//// showing the recovery being unfinished while it's actually done. However,
	//// during a wallet rescan after the recovery, the wallet's synced height
	//// will catch up and this won't be an issue.
	//_, bestHeight, err := b.cfg.ChainSource.GetBestBlock()
	//if err != nil {
	//	return isRecoveryMode, progress, err
	//}
	//
	//// The birthday block height might be greater than the current synced height
	//// in a newly restored wallet, and might be greater than the chain tip if a
	//// rollback happens. In that case, we will return zero progress here.
	//if syncState.Height < birthdayBlock.Height ||
	//	bestHeight < birthdayBlock.Height {
	//	return isRecoveryMode, progress, nil
	//}
	//
	//// progress is the ratio of the [number of blocks processed] over the [total
	//// number of blocks] needed in a recovery mode, ranging from 0 to 1, in
	//// which,
	//// - total number of blocks is the current chain's best height minus the
	////   wallet's birthday height plus 1.
	//// - number of blocks processed is the wallet's synced height minus its
	////   birthday height plus 1.
	//// - If the wallet is born very recently, the bestHeight can be equal to
	////   the birthdayBlock.Height, and it will recovery instantly.
	//progress = float64(syncState.Height-birthdayBlock.Height+1) /
	//	float64(bestHeight-birthdayBlock.Height+1)
	//
	//return isRecoveryMode, progress, nil
	panic("implement me")
}

// LabelTransaction adds a label to a transaction. If the tx already
// has a label, this call will fail unless the overwrite parameter
// is set. Labels must not be empty, and they are limited to 500 chars.
//
// Note: it is part of the WalletController interface.
func (b *LightWalletController) LabelTransaction(hash chainhash.Hash, label string,
	overwrite bool) error {

	panic("implement me")
	//return b.wallet.LabelTransaction(hash, label, overwrite)
}

// LeaseOutput locks an output to the given ID, preventing it from being
// available for any future coin selection attempts. The absolute time of the
// lock's expiration is returned. The expiration of the lock can be extended by
// successive invocations of this call. Outputs can be unlocked before their
// expiration through `ReleaseOutput`.
//
// If the output is not known, wtxmgr.ErrUnknownOutput is returned. If the
// output has already been locked to a different ID, then
// wtxmgr.ErrOutputAlreadyLocked is returned.
//
// NOTE: This method requires the global coin selection lock to be held.
func (b *LightWalletController) LeaseOutput(id wtxmgr.LockID, op wire.OutPoint, time time.Duration) (time.Time,
	error) {

	// Make sure we don't attempt to double lock an output that's been
	// locked by the in-memory implementation.
	//if b.wallet.LockedOutpoint(op) {
	//	return time.Time{}, wtxmgr.ErrOutputAlreadyLocked
	//}
	//
	//return b.wallet.LeaseOutput(id, op)
	panic("implement me LeaseOutput")
}

// ReleaseOutput unlocks an output, allowing it to be available for coin
// selection if it remains unspent. The ID should match the one used to
// originally lock the output.
//
// NOTE: This method requires the global coin selection lock to be held.
func (b *LightWalletController) ReleaseOutput(id wtxmgr.LockID, op wire.OutPoint) error {
	panic("implement me ReleaseOutput")
	//return b.wallet.ReleaseOutput(id, op)
}

func (b *LightWalletController) ListAccounts(name string,
	keyScope *waddrmgr.KeyScope) ([]*waddrmgr.AccountProperties, error) {
	panic("implement me ListAccounts")
}

// ListLeasedOutputs returns a list of all currently locked outputs.
func (b *LightWalletController) ListLeasedOutputs() ([]*wtxmgr.LockedOutput, error) {
	panic("implement me ListLeasedOutputs")
}

func (b *LightWalletController) ImportPublicKey(pubKey *btcec.PublicKey,
	addrType waddrmgr.AddressType) error {
        panic("implement me ImportPublicKey")
}

func (b *LightWalletController) ImportAccount(name string, accountPubKey *hdkeychain.ExtendedKey,
	masterKeyFingerprint uint32, addrType *waddrmgr.AddressType) error {
	panic("implement me ImportAccount")
}

func (b *LightWalletController) FundPsbt(packet *psbt.Packet,
	feeRate chainfee.SatPerKWeight, accountName string) (int32, error) {
	panic("implement me FundPsbt")
}

func (b *LightWalletController) FinalizePsbt(packet *psbt.Packet, accountName string) error {
	panic("implement me FinalizePsbt")
}

func New(cfg btcwallet.Config, 	client *chain.LightWalletClient, keychain *keychain.LightWalletKeyRing) (*LightWalletController, error) {
	return &LightWalletController{
		config: cfg,
		client: client,
		keychain: keychain,
		lockedOutpoints: map[wire.OutPoint]struct{}{},
	}, nil
}
