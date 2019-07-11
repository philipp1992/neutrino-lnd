package keychain

import (
	"crypto/sha256"
	"encoding/hex"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/walletdb"
	"strconv"
)

var (
	// lightningAddrSchema is the scope addr schema for all keys that we
	// derive. We'll treat them all as p2wkh addresses, as atm we must
	// specify a particular type.
	lightningAddrSchema = waddrmgr.ScopeAddrSchema{
		ExternalAddrType: waddrmgr.WitnessPubKey,
		InternalAddrType: waddrmgr.WitnessPubKey,
	}

	// waddrmgrNamespaceKey is the namespace key that the waddrmgr state is
	// stored within the top-level waleltdb buckets of btcwallet.
	waddrmgrNamespaceKey = []byte("waddrmgr")
)

// BtcWalletKeyRing is an implementation of both the KeyRing and SecretKeyRing
// interfaces backed by btcwallet's internal root waddrmgr. Internally, we'll
// be using a ScopedKeyManager to do all of our derivations, using the key
// scope and scope addr scehma defined above. Re-using the existing key scope
// construction means that all key derivation will be protected under the root
// seed of the wallet, making each derived key fully deterministic.
type LightWalletKeyRing struct {
	// wallet is a pointer to the active instance of the btcwallet core.
	// This is required as we'll need to manually open database
	// transactions in order to derive addresses and lookup relevant keys
	wallet interface{}//*wallet.Wallet

	// chainKeyScope defines the purpose and coin type to be used when generating
	// keys for this keyring.
	chainKeyScope waddrmgr.KeyScope

	// lightningScope is a pointer to the scope that we'll be using as a
	// sub key manager to derive all the keys that we require.
	lightningScope *waddrmgr.ScopedKeyManager

	rpcPort int // TODO(yuraolex): remove this, temporary only for providing more entropy
}

// NewBtcWalletKeyRing creates a new implementation of the
// keychain.SecretKeyRing interface backed by btcwallet.
//
// NOTE: The passed waddrmgr.Manager MUST be unlocked in order for the keychain
// to function.
func NewLightWalletKeyRing(netDir string, w interface{}, rpcPort int, coinType uint32) SecretKeyRing {
	// Construct the key scope that will be used within the waddrmgr to
	// create an HD chain for deriving all of our required keys. A different
	// scope is used for each specific coin type.
	chainKeyScope := waddrmgr.KeyScope{
		Purpose: BIP0043Purpose,
		Coin:    coinType,
	}

	return &LightWalletKeyRing{
		wallet:        w,
		chainKeyScope: chainKeyScope,
		rpcPort: rpcPort,
	}
}

// keyScope attempts to return the key scope that we'll use to derive all of
// our keys. If the scope has already been fetched from the database, then a
// cached version will be returned. Otherwise, we'll fetch it from the database
// and cache it for subsequent accesses.
func (b *LightWalletKeyRing) keyScope() (*waddrmgr.ScopedKeyManager, error) {
	// If the scope has already been populated, then we'll return it
	// directly.
	if b.lightningScope != nil {
		return b.lightningScope, nil
	}

	return nil, nil
}

// createAccountIfNotExists will create the corresponding account for a key
// family if it doesn't already exist in the database.
func (b *LightWalletKeyRing) createAccountIfNotExists(
	addrmgrNs walletdb.ReadWriteBucket, keyFam KeyFamily,
	scope *waddrmgr.ScopedKeyManager) error {

	// If this is the multi-sig key family, then we can return early as
	// this is the default account that's created.
	if keyFam == KeyFamilyMultiSig {
		return nil
	}

	// Otherwise, we'll check if the account already exists, if so, we can
	// once again bail early.
	_, err := scope.AccountName(addrmgrNs, uint32(keyFam))
	if err == nil {
		return nil
	}

	// If we reach this point, then the account hasn't yet been created, so
	// we'll need to create it before we can proceed.
	return scope.NewRawAccount(addrmgrNs, uint32(keyFam))
}

// DeriveNextKey attempts to derive the *next* key within the key family
// (account in BIP43) specified. This method should return the next external
// child within this branch.
//
// NOTE: This is part of the keychain.KeyRing interface.
func (b *LightWalletKeyRing) DeriveNextKey(keyFam KeyFamily) (KeyDescriptor, error) {
	var (
		pubKey *btcec.PublicKey
		keyLoc KeyLocator
	)

	return KeyDescriptor{
		PubKey:     pubKey,
		KeyLocator: keyLoc,
	}, nil
}

// DeriveKey attempts to derive an arbitrary key specified by the passed
// KeyLocator. This may be used in several recovery scenarios, or when manually
// rotating something like our current default node key.
//
// NOTE: This is part of the keychain.KeyRing interface.
func (b *LightWalletKeyRing) DeriveKey(keyLoc KeyLocator) (KeyDescriptor, error) {
	var keyDesc KeyDescriptor

	privKey, err := b.DerivePrivKey(KeyDescriptor{keyLoc, nil })

	if err == nil {
		keyDesc.KeyLocator = keyLoc
		keyDesc.PubKey = privKey.PubKey()
	}

	return keyDesc, nil
}

// DerivePrivKey attempts to derive the private key that corresponds to the
// passed key descriptor.
//
// NOTE: This is part of the keychain.SecretKeyRing interface.
func (b *LightWalletKeyRing) DerivePrivKey(keyDesc KeyDescriptor) (*btcec.PrivateKey, error) {
	var key *btcec.PrivateKey

	pkBytes, err := hex.DecodeString("a11b0a4e1a132305652ee7a8eb7848f6ad" +
		"5ea381e3ce20a2c086a2e388230811" + hex.EncodeToString([]byte(strconv.Itoa(b.rpcPort))))
	if err != nil {
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
