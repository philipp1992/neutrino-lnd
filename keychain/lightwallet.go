package keychain

import (
	"crypto/sha256"
	"encoding/hex"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/grpcclient"
)

// BtcWalletKeyRing is an implementation of both the KeyRing and SecretKeyRing
// interfaces backed by btcwallet's internal root waddrmgr. Internally, we'll
// be using a ScopedKeyManager to do all of our derivations, using the key
// scope and scope addr scehma defined above. Re-using the existing key scope
// construction means that all key derivation will be protected under the root
// seed of the wallet, making each derived key fully deterministic.
type LightWalletKeyRing struct {
	rpcClient *grpcclient.Client
}

// NewBtcWalletKeyRing creates a new implementation of the
// keychain.SecretKeyRing interface backed by btcwallet.
//
// NOTE: The passed waddrmgr.Manager MUST be unlocked in order for the keychain
// to function.
func NewLightWalletKeyRing(rpcClient *grpcclient.Client) *LightWalletKeyRing {

	return &LightWalletKeyRing {
		rpcClient: rpcClient,
	}
}

// DeriveNextKey attempts to derive the *next* key within the key family
// (account in BIP43) specified. This method should return the next external
// child within this branch.
//
// NOTE: This is part of the keychain.KeyRing interface.
func (b *LightWalletKeyRing) DeriveNextKey(keyFam KeyFamily) (KeyDescriptor, error) {
	var keyDesc KeyDescriptor

	res, err := b.rpcClient.DeriveNextKey(uint32(keyFam))

	if err != nil {
		return keyDesc, err
	}

	decodedBytes, _ := hex.DecodeString(res.HexPubKey)
	keyDesc.PubKey, err = btcec.ParsePubKey(decodedBytes, btcec.S256())

	if len(decodedBytes) == 0 || err != nil {
		return keyDesc, err
	}

	keyDesc.Index = res.Locator.Index
	keyDesc.Family = KeyFamily(res.Locator.Family)

	return keyDesc, nil
}

// DeriveKey attempts to derive an arbitrary key specified by the passed
// KeyLocator. This may be used in several recovery scenarios, or when manually
// rotating something like our current default node key.
//
// NOTE: This is part of the keychain.KeyRing interface.
func (b *LightWalletKeyRing) DeriveKey(keyLoc KeyLocator) (KeyDescriptor, error) {
	var keyDesc KeyDescriptor

	desc, err := b.rpcClient.DeriveKey(uint32(keyLoc.Family), keyLoc.Index)

	decodedBytes, _ := hex.DecodeString(desc.HexPubKey)

	if len(decodedBytes) == 0 || err != nil {
		return keyDesc, nil
	}

	keyDesc.PubKey, err = btcec.ParsePubKey(decodedBytes, btcec.S256())

	if err != nil {
		return keyDesc, err
	}

	keyDesc.KeyLocator = keyLoc

	return keyDesc, nil
}

// DerivePrivKey attempts to derive the private key that corresponds to the
// passed key descriptor.
//
// NOTE: This is part of the keychain.SecretKeyRing interface.
func (b *LightWalletKeyRing) DerivePrivKey(keyDesc KeyDescriptor) (*btcec.PrivateKey, error) {
	var key *btcec.PrivateKey

	var hexEncodedPubKey string
	if keyDesc.PubKey != nil {
		hexEncodedPubKey = hex.EncodeToString(keyDesc.PubKey.SerializeCompressed())
	}

	hexPrivKey, err := b.rpcClient.DerivePrivKey(uint32(keyDesc.Family), keyDesc.Index, hexEncodedPubKey)

	if err != nil {
		return nil, err
	}

	pkBytes, err := hex.DecodeString(*hexPrivKey)

	if len(pkBytes) == 0 || err != nil {
		return nil, err
	}

	key, _ = btcec.PrivKeyFromBytes(btcec.S256(), pkBytes)

	return key, nil
}

// ScalarMult performs a scalar multiplication (ECDH-like operation) between
// the target key descriptor and remote public key. The output returned will be
// the sha256 of the resulting shared point serialized in compressed format. If
// k is our private key, and P is the public key, we perform the following
// operation:
//
//  sx := k*P s := sha256(sx.SerializeCompressed())
//
// NOTE: This is part of the keychain.SecretKeyRing interface.
func (b *LightWalletKeyRing) ScalarMult(keyDesc KeyDescriptor,
	pub *btcec.PublicKey) ([]byte, error) {

	privKey, err := b.DerivePrivKey(keyDesc)
	if err != nil {
		return nil, err
	}

	s := &btcec.PublicKey{}
	x, y := btcec.S256().ScalarMult(pub.X, pub.Y, privKey.D.Bytes())
	s.X = x
	s.Y = y

	h := sha256.Sum256(s.SerializeCompressed())

	return h[:], nil
}
