package lightwallet

import (
	"encoding/hex"
	"fmt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/wallet/txauthor"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
)

type LightWalletController struct{
	// client is the RPC client to the bitcoind node.
	client *chain.LightWalletClient
	config btcwallet.Config
	keychain *keychain.LightWalletKeyRing
}

func (lw *LightWalletController) FetchInputInfo(prevOut *wire.OutPoint) (*wire.TxOut, error) {
	utxo, err := lw.client.GetUnspentOutput(&prevOut.Hash, prevOut.Index)

	if err != nil {
		return nil, err
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
	panic("implement me")
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
	panic("implement me")
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

	result, err := lw.client.ChainConn.RPCClient().ListUtxos()

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

		utxos = append(utxos, tmp)
	}

	return utxos, nil
}

func (lw *LightWalletController) ListTransactionDetails() ([]*lnwallet.TransactionDetail, error) {
	panic("implement me")
}

func (lw *LightWalletController) LockOutpoint(o wire.OutPoint) {
	//panic("implement me")
}

func (lw *LightWalletController) UnlockOutpoint(o wire.OutPoint) {
	//panic("implement me")
}

func (lw *LightWalletController) PublishTransaction(tx *wire.MsgTx) error {
	txid, err := lw.client.SendRawTransaction(tx, true)

	fmt.Printf("Published transaction with txid: %v", txid)

	return err
}

func (lw *LightWalletController) SubscribeTransactions() (lnwallet.TransactionSubscription, error) {
	panic("implement me")
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
	}, nil
}