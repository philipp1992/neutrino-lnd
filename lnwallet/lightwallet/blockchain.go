package lightwallet

import (
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
	panic("implement me")
}

func (lw *LightWalletController) GetBlockHash(blockHeight int64) (*chainhash.Hash, error) {
	return lw.client.GetBlockHash(blockHeight)
}

func (lw *LightWalletController) GetBlock(blockHash *chainhash.Hash) (*wire.MsgBlock, error) {
	return lw.client.GetBlock(blockHash)
}
