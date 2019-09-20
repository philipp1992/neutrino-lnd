package lightwallet

import (
	"encoding/hex"
	"fmt"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// maybeTweakPrivKey examines the single and double tweak parameters on the
// passed sign descriptor and may perform a mapping on the passed private key
// in order to utilize the tweaks, if populated.
func maybeTweakPrivKey(signDesc *input.SignDescriptor,
	privKey *btcec.PrivateKey) (*btcec.PrivateKey, error) {

	var retPriv *btcec.PrivateKey
	switch {

	case signDesc.SingleTweak != nil:
		retPriv = input.TweakPrivKey(privKey,
			signDesc.SingleTweak)

	case signDesc.DoubleTweak != nil:
		retPriv = input.DeriveRevocationPrivKey(privKey,
			signDesc.DoubleTweak)

	default:
		retPriv = privKey
	}

	return retPriv, nil
}

func (lw *LightWalletController) privateKeyForScript(pkScript []byte, signDesc *input.SignDescriptor) (*btcec.PrivateKey, error) {
	encodedKey, err := lw.client.ChainConn.RPCClient().LWDumpPrivKey(hex.EncodeToString(pkScript))

	if err != nil {
		return nil, err
	}

	if len(*encodedKey) == 0 {
		encodedKey, err = lw.client.ChainConn.RPCClient().DerivePrivKey(uint32(signDesc.KeyDesc.Family),
			signDesc.KeyDesc.Index, "")

		if err != nil {
			return nil, err
		}
	}

	_, addresses, _, _ := txscript.ExtractPkScriptAddrs(pkScript, lw.config.NetParams)

	fmt.Printf("Requesting priv key for address: %v\n", addresses)

	bytes, err := hex.DecodeString(*encodedKey)

	if err != nil {
		return nil, err
	}

	privKey, _ := btcec.PrivKeyFromBytes(btcec.S256(), bytes)

	return privKey, nil
}

func (lw *LightWalletController) SignOutputRaw(tx *wire.MsgTx, signDesc *input.SignDescriptor) ([]byte, error) {
	witnessScript := signDesc.WitnessScript


	privKey, err := lw.keychain.DerivePrivKey(signDesc.KeyDesc)
	if err != nil {
		return nil, err
	}
	
	// If a tweak (single or double) is specified, then we'll need to use
	// this tweak to derive the final private key to be used for signing
	// this output.
	privKey, err = maybeTweakPrivKey(signDesc, privKey)
	if err != nil {
		return nil, err
	}

	// TODO(roasbeef): generate sighash midstate if not present?

	amt := signDesc.Output.Value
	sig, err := txscript.RawTxInWitnessSignature(
		tx, signDesc.SigHashes, signDesc.InputIndex, amt,
		witnessScript, signDesc.HashType, privKey,
	)
	if err != nil {
		return nil, err
	}

	// Chop off the sighash flag at the end of the signature.
	return sig[:len(sig)-1], nil
}

// ComputeInputScript generates a complete InputIndex for the passed
// transaction with the signature as defined within the passed
// SignDescriptor. This method should be capable of generating the
// proper input script for both regular p2wkh output and p2wkh outputs
// nested within a regular p2sh output.
//
// NOTE: This method will ignore any tweak parameters set within the
// passed SignDescriptor as it assumes a set of typical script
// templates (p2wkh, np2wkh, etc).
func (lw *LightWalletController) ComputeInputScript(tx *wire.MsgTx, signDesc *input.SignDescriptor) (*input.Script, error) {

	outputScript := signDesc.Output.PkScript

	privKey, err := lw.privateKeyForScript(outputScript, signDesc)

	if err != nil {
		return nil, err
	}

	addrType, _, _, _ := txscript.ExtractPkScriptAddrs(outputScript, lw.config.NetParams)

	var witnessProgram []byte
	inputScript := &input.Script{}
	switch {

	// If we're spending p2wkh output nested within a p2sh output, then
	// we'll need to attach a sigScript in addition to witness data.
	case addrType == txscript.WitnessV0ScriptHashTy:
		pubKey := privKey.PubKey()
		pubKeyHash := btcutil.Hash160(pubKey.SerializeCompressed())

		// Next, we'll generate a valid sigScript that will allow us to
		// spend the p2sh output. The sigScript will contain only a
		// single push of the p2wkh witness program corresponding to
		// the matching public key of this address.
		p2wkhAddr, err := btcutil.NewAddressWitnessPubKeyHash(
			pubKeyHash, lw.config.NetParams,
		)
		if err != nil {
			return nil, err
		}
		witnessProgram, err = txscript.PayToAddrScript(p2wkhAddr)
		if err != nil {
			return nil, err
		}

		bldr := txscript.NewScriptBuilder()
		bldr.AddData(witnessProgram)
		sigScript, err := bldr.Script()
		if err != nil {
			return nil, err
		}

		inputScript.SigScript = sigScript

	// Otherwise, this is a regular p2wkh output, so we include the
	// witness program itself as the subscript to generate the proper
	// sighash digest. As part of the new sighash digest algorithm, the
	// p2wkh witness program will be expanded into a regular p2kh
	// script.
	default:
		witnessProgram = outputScript
	}

	// If a tweak (single or double) is specified, then we'll need to use
	// this tweak to derive the final private key to be used for signing
	// this output.
	privKey, err = maybeTweakPrivKey(signDesc, privKey)
	if err != nil {
		return nil, err
	}

	// Generate a valid witness stack for the input.
	// TODO(roasbeef): adhere to passed HashType
	witnessScript, err := txscript.WitnessSignature(tx, signDesc.SigHashes,
		signDesc.InputIndex, signDesc.Output.Value, witnessProgram,
		signDesc.HashType, privKey, true,
	)
	if err != nil {
		return nil, err
	}

	inputScript.Witness = witnessScript

	return inputScript, nil
}

// A compile time check to ensure that BtcWallet implements the Signer
// interface.
var _ input.Signer = (*LightWalletController)(nil)

func (lw *LightWalletController) SignMessage(pubKey *btcec.PublicKey, msg []byte) (*btcec.Signature, error) {

	privKey, err := lw.keychain.DerivePrivKey(keychain.KeyDescriptor{
		PubKey: pubKey,
	})

	if err != nil {
		return nil, err
	}

	// Double hash and sign the data.
	msgDigest := chainhash.DoubleHashB(msg)

	signature, err := privKey.Sign(msgDigest)

	if err != nil {
		return nil, err
	}

	pubKey.Curve = btcec.S256()

	return signature, nil
}


// A compile time check to ensure that BtcWallet implements the MessageSigner
// interface.
var _ lnwallet.MessageSigner = (*LightWalletController)(nil)