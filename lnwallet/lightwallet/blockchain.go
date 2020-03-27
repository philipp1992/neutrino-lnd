package lightwallet

import (
	"encoding/hex"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

func (lw *LightWalletController) GetBestBlock() (*chainhash.Hash, int32, error) {
	blockStamp, err := lw.client.BlockStamp()

	if err != nil {
		return nil, 0, err
	}

	return &blockStamp.Hash, blockStamp.Height, nil
}

func (lw *LightWalletController) GetUtxo(op *wire.OutPoint, pkScript []byte, heightHint uint32,
	cancel <-chan struct{}) (*wire.TxOut, error) {

	utxo, err := lw.client.GetUnspentOutput(&op.Hash, op.Index)
	if err != nil {
		return nil, err
	}

	// if pkscript empty, output is spent
	if utxo == nil {
		return nil, nil
	}

	// otherwise decode usnpent output
	parsedScript, err := hex.DecodeString(utxo.ScriptPubKeyHex)
	if err != nil {
		return nil, err
	}

	return &wire.TxOut{
		Value: utxo.Amount,
		PkScript: parsedScript,

	}, nil
}

func (lw *LightWalletController) GetBlockHash(blockHeight int64) (*chainhash.Hash, error) {
	return lw.client.GetBlockHash(blockHeight)
}

func (lw *LightWalletController) GetBlock(blockHash *chainhash.Hash) (*wire.MsgBlock, error) {
	return lw.client.GetBlock(blockHash)
}

func (lw *LightWalletController) GetRawTxByIndex(blockHeight int64, txIndex uint32) (*wire.MsgTx, error) {
	return lw.client.GetRawTxByIndex(blockHeight, txIndex)
}

func (lw *LightWalletController) LoadCache(startHeight uint32) (bool, error) {
	return lw.client.LoadSecondLayerCache(startHeight)
}

func (lw *LightWalletController) FreeCache() error {
	return lw.client.FreeSecondLayerCache()
}

