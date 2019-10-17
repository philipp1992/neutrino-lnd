package lightwallet

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/wallet/txauthor"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"sync"
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


func (lw *LightWalletController) FetchInputInfo(prevOut *wire.OutPoint) (*wire.TxOut, error) {
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

	return &wire.TxOut{
		Value: utxo.Amount,
		PkScript: pkScript,
	}, nil
}

func (lw *LightWalletController) ConfirmedBalance(confs int32) (btcutil.Amount, error) {
	return lw.client.ChainConn.RPCClient().GetConfirmedBalance(confs)
}

func (lw *LightWalletController) NewAddress(addrType lnwallet.AddressType, change bool) (btcutil.Address, error) {
	if addrType != lnwallet.WitnessPubKey {
		panic("implement me")
	}

	addrStr, err := lw.client.ChainConn.RPCClient().GetLastAddress(change)

	if err != nil {
		return nil, err
	}

	return btcutil.DecodeAddress(addrStr, lw.config.NetParams)
}

func (lw *LightWalletController) LastUnusedAddress(addrType lnwallet.AddressType) (btcutil.Address, error) {
	panic("implement me")
}

func (lw *LightWalletController) IsOurAddress(a btcutil.Address) bool {
	return lw.client.ChainConn.RPCClient().IsOurAddress(a.EncodeAddress())
}

func (lw *LightWalletController) SendOutputs(outputs []*wire.TxOut,
	feeRate lnwallet.SatPerKWeight) (*wire.MsgTx, error) {
	panic("implement me")
}

func (lw *LightWalletController) CreateSimpleTx(outputs []*wire.TxOut, feeRate lnwallet.SatPerKWeight,
	dryRun bool) (*txauthor.AuthoredTx, error) {
	panic("implement me")
}

func (lw *LightWalletController) ListUnspentWitness(minconfirms, maxconfirms int32) ([]*lnwallet.Utxo, error) {

	var addresses []string
	result, err := lw.client.ChainConn.RPCClient().ListUtxos(1, 9999999, addresses)
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

func (lw *LightWalletController) ListTransactionDetails() ([]*lnwallet.TransactionDetail, error) {
	panic("implement list")
	return nil, nil
}

// LockedOutpoint returns whether an outpoint has been marked as locked and
// should not be used as an input for created transactions.
func (lw *LightWalletController) LockedOutpoint(o wire.OutPoint) bool {
	_, locked := lw.lockedOutpoints[o]
	fmt.Printf("\nChecking %s \n Returning %f\n", o.Hash.String(), locked)
	return locked
}

func (lw *LightWalletController) LockOutpoint(o wire.OutPoint) {
	lw.lockedOutpoints[o] = struct{}{}
	fmt.Printf("\nLocking %s\n", o.Hash.String())

}

func (lw *LightWalletController) UnlockOutpoint(o wire.OutPoint) {
	delete(lw.lockedOutpoints, o)
}

func (lw *LightWalletController) PublishTransaction(tx *wire.MsgTx) error {

	txid, err := lw.client.SendRawTransaction(tx, true)
	if err != nil {
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

func New(cfg btcwallet.Config, 	client *chain.LightWalletClient, keychain *keychain.LightWalletKeyRing) (*LightWalletController, error) {
	return &LightWalletController{
		config: cfg,
		client: client,
		keychain: keychain,
		lockedOutpoints: map[wire.OutPoint]struct{}{},
	}, nil
}