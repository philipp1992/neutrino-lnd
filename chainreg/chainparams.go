package chainreg

import (
	"github.com/btcsuite/btcd/chaincfg"
	bitcoinCfg "github.com/btcsuite/btcd/chaincfg"
	xsncoinCfg "github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	bitcoinWire "github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/keychain"
	litecoinCfg "github.com/ltcsuite/ltcd/chaincfg"
	litecoinWire "github.com/ltcsuite/ltcd/wire"
)

// BitcoinNetParams couples the p2p parameters of a network with the
// corresponding RPC port of a daemon running on the particular network.
type BitcoinNetParams struct {
	*bitcoinCfg.Params
	RPCPort  string
	CoinType uint32
}

// LitecoinNetParams couples the p2p parameters of a network with the
// corresponding RPC port of a daemon running on the particular network.
type LitecoinNetParams struct {
	*litecoinCfg.Params
	RPCPort  string
	CoinType uint32
}

type XsncoinNetParams struct {
	*xsncoinCfg.Params
	RPCPort  string
	CoinType uint32
}

// BitcoinTestNetParams contains parameters specific to the 3rd version of the
// test network.
var BitcoinTestNetParams = BitcoinNetParams{
	Params:   &bitcoinCfg.TestNet3Params,
	RPCPort:  "18334",
	CoinType: keychain.CoinTypeTestnet,
}

// BitcoinMainNetParams contains parameters specific to the current Bitcoin
// mainnet.
var BitcoinMainNetParams = BitcoinNetParams{
	Params:   &bitcoinCfg.MainNetParams,
	RPCPort:  "8334",
	CoinType: keychain.CoinTypeBitcoin,
}

// BitcoinSimNetParams contains parameters specific to the simulation test
// network.
var BitcoinSimNetParams = BitcoinNetParams{
	Params:   &bitcoinCfg.SimNetParams,
	RPCPort:  "18556",
	CoinType: keychain.CoinTypeTestnet,
}

// bitcoinLightWalletParams contains parameters specific to the LW connection
var LtcLightWalletParams = LitecoinNetParams{
	Params:   &litecoinCfg.LitecoinLWParams,
	RPCPort:  "12347",
	CoinType: keychain.CoinTypeLitecoin,
}

// bitcoinLightWalletParams contains parameters specific to the LW connection
var LtcLightWalletRegtestParams = LitecoinNetParams{
	Params:   &litecoinCfg.LitecoinLWRegTestParams,
	RPCPort:  "12347",
	CoinType: keychain.CoinTypeLitecoin,
}

// bitcoinLightWalletParams contains parameters specific to the LW connection
var BtcLightWalletParams = BitcoinNetParams{
	Params:   &bitcoinCfg.BitcoinLWParams,
	RPCPort:  "12347",
	CoinType: keychain.CoinTypeBitcoin,
}

// bitcoinLightWalletParams contains parameters specific to the LW connection
var BtcLightWalletRegtestParams = BitcoinNetParams{
	Params:   &bitcoinCfg.BitcoinLWRegTestParams,
	RPCPort:  "12347",
	CoinType: keychain.CoinTypeBitcoin,
}

// xsnLightWalletParams contains parameters specific to the LW connection
var XsnLightWalletParams = XsncoinNetParams {
	Params:   &xsncoinCfg.XsncoinLWParams,
	RPCPort:  "12347",
	CoinType: keychain.CoinTypeStakenet,
}

// xsnLightWalletRegtestParams contains parameters specific to the LW connection
var XsnLightWalletRegtestParams = XsncoinNetParams {
	Params:   &xsncoinCfg.XsncoinLWRegTestParams,
	RPCPort:  "12347",
	CoinType: keychain.CoinTypeStakenet,
}

// LitecoinSimNetParams contains parameters specific to the simulation test
// network.
var LitecoinSimNetParams = LitecoinNetParams{
	Params:   &litecoinCfg.TestNet4Params,
	RPCPort:  "18556",
	CoinType: keychain.CoinTypeTestnet,
}

// LitecoinTestNetParams contains parameters specific to the 4th version of the
// test network.
var LitecoinTestNetParams = LitecoinNetParams{
	Params:   &litecoinCfg.TestNet4Params,
	RPCPort:  "19334",
	CoinType: keychain.CoinTypeTestnet,
}

// LitecoinMainNetParams contains the parameters specific to the current
// Litecoin mainnet.
var LitecoinMainNetParams = LitecoinNetParams{
	Params:   &litecoinCfg.MainNetParams,
	RPCPort:  "9334",
	CoinType: keychain.CoinTypeLitecoin,
}

// LitecoinRegTestNetParams contains parameters specific to a local litecoin
// regtest network.
var LitecoinRegTestNetParams = LitecoinNetParams{
	Params:   &litecoinCfg.RegressionNetParams,
	RPCPort:  "18334",
	CoinType: keychain.CoinTypeTestnet,
}

// BitcoinRegTestNetParams contains parameters specific to a local bitcoin
// regtest network.
var BitcoinRegTestNetParams = BitcoinNetParams{
	Params:   &bitcoinCfg.RegressionNetParams,
	RPCPort:  "18334",
	CoinType: keychain.CoinTypeTestnet,
}

// xsnTestNetParams contains parameters specific to the 3rd version of the
// test network.
var XsnTestNetParams = XsncoinNetParams{
	Params:   &xsncoinCfg.TestNet3Params,
	RPCPort:  "20000",
	CoinType: keychain.CoinTypeStakenet,
}

// xsnMainNetParams contains the parameters specific to the current
// Stakenet mainnet.
var XsnMainNetParams = XsncoinNetParams{
	Params:   &xsncoinCfg.MainNetParams,
	RPCPort:  "51475",
	CoinType: keychain.CoinTypeStakenet,
}

// xsnRegTestNetParams contains parameters specific to a local regtest network.
var XsnRegTestNetParams = XsncoinNetParams{
	Params:   &xsncoinCfg.RegressionNetParams,
	RPCPort:  "18334",
	CoinType: keychain.CoinTypeStakenet,
}

// ApplyLitecoinParams applies the relevant chain configuration parameters that
// differ for litecoin to the chain parameters typed for btcsuite derivation.
// This function is used in place of using something like interface{} to
// abstract over _which_ chain (or fork) the parameters are for.
func ApplyLitecoinParams(params *BitcoinNetParams,
	litecoinParams *LitecoinNetParams) {

	params.Name = litecoinParams.Name
	params.Net = bitcoinWire.BitcoinNet(litecoinParams.Net)
	params.DefaultPort = litecoinParams.DefaultPort
	params.CoinbaseMaturity = litecoinParams.CoinbaseMaturity

	copy(params.GenesisHash[:], litecoinParams.GenesisHash[:])

	// Address encoding magics
	params.PubKeyHashAddrID = litecoinParams.PubKeyHashAddrID
	params.ScriptHashAddrID = litecoinParams.ScriptHashAddrID
	params.PrivateKeyID = litecoinParams.PrivateKeyID
	params.WitnessPubKeyHashAddrID = litecoinParams.WitnessPubKeyHashAddrID
	params.WitnessScriptHashAddrID = litecoinParams.WitnessScriptHashAddrID
	params.Bech32HRPSegwit = litecoinParams.Bech32HRPSegwit

	copy(params.HDPrivateKeyID[:], litecoinParams.HDPrivateKeyID[:])
	copy(params.HDPublicKeyID[:], litecoinParams.HDPublicKeyID[:])

	params.HDCoinType = litecoinParams.HDCoinType

	checkPoints := make([]chaincfg.Checkpoint, len(litecoinParams.Checkpoints))
	for i := 0; i < len(litecoinParams.Checkpoints); i++ {
		var chainHash chainhash.Hash
		copy(chainHash[:], litecoinParams.Checkpoints[i].Hash[:])

		checkPoints[i] = chaincfg.Checkpoint{
			Height: litecoinParams.Checkpoints[i].Height,
			Hash:   &chainHash,
		}
	}
	params.Checkpoints = checkPoints

	params.RPCPort = litecoinParams.RPCPort
	params.CoinType = litecoinParams.CoinType
}

// applyStakenetParams applies the relevant chain configuration parameters that
// differ for xsncoin to the chain parameters typed for btcsuite derivation.
// This function is used in place of using something like interface{} to
// abstract over _which_ chain (or fork) the parameters are for.
func ApplyStakenetParams(params *BitcoinNetParams, xsnParams *XsncoinNetParams) {
	params.Name = xsnParams.Name
	params.Net = bitcoinWire.BitcoinNet(xsnParams.Net)
	params.DefaultPort = xsnParams.DefaultPort
	params.CoinbaseMaturity = xsnParams.CoinbaseMaturity

	copy(params.GenesisHash[:], xsnParams.GenesisHash[:])

	// Address encoding magics
	params.PubKeyHashAddrID = xsnParams.PubKeyHashAddrID
	params.ScriptHashAddrID = xsnParams.ScriptHashAddrID
	params.PrivateKeyID = xsnParams.PrivateKeyID
	params.WitnessPubKeyHashAddrID = xsnParams.WitnessPubKeyHashAddrID
	params.WitnessScriptHashAddrID = xsnParams.WitnessScriptHashAddrID
	params.Bech32HRPSegwit = xsnParams.Bech32HRPSegwit

	copy(params.HDPrivateKeyID[:], xsnParams.HDPrivateKeyID[:])
	copy(params.HDPublicKeyID[:], xsnParams.HDPublicKeyID[:])

	params.HDCoinType = xsnParams.HDCoinType

	checkPoints := make([]chaincfg.Checkpoint, len(xsnParams.Checkpoints))
	for i := 0; i < len(xsnParams.Checkpoints); i++ {
		var chainHash chainhash.Hash
		copy(chainHash[:], xsnParams.Checkpoints[i].Hash[:])

		checkPoints[i] = chaincfg.Checkpoint{
			Height: xsnParams.Checkpoints[i].Height,
			Hash:   &chainHash,
		}
	}
	params.Checkpoints = checkPoints

	params.RPCPort = xsnParams.RPCPort
	params.CoinType = xsnParams.CoinType
}

// IsTestnet tests if the givern params correspond to a testnet
// parameter configuration.
func IsTestnet(params *BitcoinNetParams) bool {
	switch params.Params.Net {
	case bitcoinWire.TestNet3, bitcoinWire.BitcoinNet(litecoinWire.TestNet4):
		return true
	default:
		return false
	}
}
