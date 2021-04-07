package peer

import (
	"bytes"
	crand "crypto/rand"
	"encoding/binary"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"os"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntest/channels"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/netann"
	"github.com/lightningnetwork/lnd/queue"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/lightningnetwork/lnd/ticker"
	"github.com/stretchr/testify/require"
)

const (
	broadcastHeight = 100

	// timeout is a timeout value to use for tests which need to wait for
	// a return value on a channel.
	timeout = time.Second * 5
)

var (
	// Just use some arbitrary bytes as delivery script.
	dummyDeliveryScript = channels.AlicesPrivKey
)

// noUpdate is a function which can be used as a parameter in createTestPeer to
// call the setup code with no custom values on the channels set up.
var noUpdate = func(a, b *channeldb.OpenChannel) {}

type mockSigner struct {
	key *btcec.PrivateKey
}

func (m *mockSigner) SignOutputRaw(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (input.Signature, error) {
	amt := signDesc.Output.Value
	witnessScript := signDesc.WitnessScript
	privKey := m.key

	if !privKey.PubKey().IsEqual(signDesc.KeyDesc.PubKey) {
		return nil, fmt.Errorf("incorrect key passed")
	}

	switch {
	case signDesc.SingleTweak != nil:
		privKey = input.TweakPrivKey(privKey,
			signDesc.SingleTweak)
	case signDesc.DoubleTweak != nil:
		privKey = input.DeriveRevocationPrivKey(privKey,
			signDesc.DoubleTweak)
	}

	sig, err := txscript.RawTxInWitnessSignature(tx, signDesc.SigHashes,
		signDesc.InputIndex, amt, witnessScript, signDesc.HashType,
		privKey)
	if err != nil {
		return nil, err
	}

	return btcec.ParseDERSignature(sig[:len(sig)-1], btcec.S256())
}

func (m *mockSigner) ComputeInputScript(tx *wire.MsgTx,
	signDesc *input.SignDescriptor) (*input.Script, error) {

	// TODO(roasbeef): expose tweaked signer from lnwallet so don't need to
	// duplicate this code?

	privKey := m.key

	switch {
	case signDesc.SingleTweak != nil:
		privKey = input.TweakPrivKey(privKey,
			signDesc.SingleTweak)
	case signDesc.DoubleTweak != nil:
		privKey = input.DeriveRevocationPrivKey(privKey,
			signDesc.DoubleTweak)
	}

	witnessScript, err := txscript.WitnessSignature(tx, signDesc.SigHashes,
		signDesc.InputIndex, signDesc.Output.Value, signDesc.Output.PkScript,
		signDesc.HashType, privKey, true)
	if err != nil {
		return nil, err
	}

	return &input.Script{
		Witness: witnessScript,
	}, nil
}

var _ input.Signer = (*mockSigner)(nil)

type mockChainIO struct {
	bestHeight int32
}

func (m *mockChainIO) GetBestBlock() (*chainhash.Hash, int32, error) {
	return nil, m.bestHeight, nil
}

func (*mockChainIO) GetUtxo(op *wire.OutPoint, _ []byte,
	heightHint uint32, _ <-chan struct{}) (*wire.TxOut, error) {
	return nil, nil
}

func (*mockChainIO) GetBlockHash(blockHeight int64) (*chainhash.Hash, error) {
	return nil, nil
}

func (*mockChainIO) GetBlock(blockHash *chainhash.Hash) (*wire.MsgBlock, error) {
	return nil, nil
}

func (m *mockChainIO) LoadCache(startHeight uint32) (bool, error) {
	return true, nil
}

func (m *mockChainIO) GetRawTxByIndex(blockHeight int64, txIndex uint32) (*wire.MsgTx, error) {
	return nil, nil
}

// FreeCache releases second layer headers cache on response
func (m *mockChainIO) FreeCache() error {
	return nil
}
var _ lnwallet.BlockChainIO = (*mockChainIO)(nil)

type mockWalletController struct {
	rootKey       *btcec.PrivateKey
	publishedTxns chan *wire.MsgTx
}

func (*mockWalletController) FetchInputInfo(prevOut *wire.OutPoint) (
	*lnwallet.Utxo, error) {

	return nil, nil
}

func (*mockWalletController) ConfirmedBalance(confs int32) (btcutil.Amount,
	error) {

	return 0, nil
}

func (m *mockWalletController) NewAddress(addrType lnwallet.AddressType,
	change bool) (btcutil.Address, error) {

	addr, _ := btcutil.NewAddressPubKey(
		m.rootKey.PubKey().SerializeCompressed(), &chaincfg.MainNetParams,
	)
	return addr, nil
}

func (*mockWalletController) LastUnusedAddress(addrType lnwallet.AddressType) (
	btcutil.Address, error) {

	return nil, nil
}

func (*mockWalletController) IsOurAddress(a btcutil.Address) bool {
	return false
}

func (*mockWalletController) SendOutputs(outputs []*wire.TxOut,
	feeRate chainfee.SatPerKWeight, label string) (*wire.MsgTx, error) {

	return nil, nil
}

func (*mockWalletController) CreateSimpleTx(outputs []*wire.TxOut,
	feeRate chainfee.SatPerKWeight, dryRun bool) (*txauthor.AuthoredTx, error) {

	return nil, nil
}

func (*mockWalletController) ListUnspentWitness(minconfirms,
	maxconfirms int32) ([]*lnwallet.Utxo, error) {

	return nil, nil
}

func (*mockWalletController) ListTransactionDetails(startHeight,
	endHeight int32) ([]*lnwallet.TransactionDetail, error) {

	return nil, nil
}

func (*mockWalletController) LockOutpoint(o wire.OutPoint) {}

func (*mockWalletController) UnlockOutpoint(o wire.OutPoint) {}

func (m *mockWalletController) PublishTransaction(tx *wire.MsgTx,
	label string) error {
	m.publishedTxns <- tx
	return nil
}

func (*mockWalletController) LabelTransaction(hash chainhash.Hash,
	label string, overwrite bool) error {

	return nil
}

func (*mockWalletController) SubscribeTransactions() (
	lnwallet.TransactionSubscription, error) {

	return nil, nil
}

func (*mockWalletController) IsSynced() (bool, int64, error) {
	return false, 0, nil
}

func (*mockWalletController) Start() error {
	return nil
}

func (*mockWalletController) Stop() error {
	return nil
}

func (*mockWalletController) BackEnd() string {
	return ""
}

func (*mockWalletController) LeaseOutput(wtxmgr.LockID,
	wire.OutPoint) (time.Time, error) {

	return time.Now(), nil
}

func (*mockWalletController) ReleaseOutput(wtxmgr.LockID, wire.OutPoint) error {
	return nil
}

func (*mockWalletController) GetRecoveryInfo() (bool, float64, error) {
	return false, 0, nil
}

var _ lnwallet.WalletController = (*mockWalletController)(nil)

type mockNotifier struct {
	confChannel chan *chainntnfs.TxConfirmation
}

func (m *mockNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash,
	_ []byte, numConfs, heightHint uint32) (*chainntnfs.ConfirmationEvent,
	error) {

	return &chainntnfs.ConfirmationEvent{
		Confirmed: m.confChannel,
	}, nil
}

func (m *mockNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint, _ []byte,
	heightHint uint32) (*chainntnfs.SpendEvent, error) {

	return &chainntnfs.SpendEvent{
		Spend:  make(chan *chainntnfs.SpendDetail),
		Cancel: func() {},
	}, nil
}

func (m *mockNotifier) RegisterBlockEpochNtfn(
	bestBlock *chainntnfs.BlockEpoch) (*chainntnfs.BlockEpochEvent, error) {

	return &chainntnfs.BlockEpochEvent{
		Epochs: make(chan *chainntnfs.BlockEpoch),
		Cancel: func() {},
	}, nil
}

func (m *mockNotifier) Start() error {
	return nil
}

func (m *mockNotifier) Stop() error {
	return nil
}

func (m *mockNotifier) Started() bool {
	return true
}

var _ chainntnfs.ChainNotifier = (*mockNotifier)(nil)

// createTestPeer creates a channel between two nodes, and returns a peer for
// one of the nodes, together with the channel seen from both nodes. It takes
// an updateChan function which can be used to modify the default values on
// the channel states for each peer.
func createTestPeer(notifier chainntnfs.ChainNotifier,
	publTx chan *wire.MsgTx, updateChan func(a, b *channeldb.OpenChannel)) (
	*Brontide, *lnwallet.LightningChannel, func(), error) {

	aliceKeyPriv, aliceKeyPub := btcec.PrivKeyFromBytes(
		btcec.S256(), channels.AlicesPrivKey,
	)
	aliceKeySigner := &keychain.PrivKeyDigestSigner{PrivKey: aliceKeyPriv}
	bobKeyPriv, bobKeyPub := btcec.PrivKeyFromBytes(
		btcec.S256(), channels.BobsPrivKey,
	)

	channelCapacity := btcutil.Amount(10 * 1e8)
	channelBal := channelCapacity / 2
	aliceDustLimit := btcutil.Amount(200)
	bobDustLimit := btcutil.Amount(1300)
	csvTimeoutAlice := uint32(5)
	csvTimeoutBob := uint32(4)

	prevOut := &wire.OutPoint{
		Hash:  channels.TestHdSeed,
		Index: 0,
	}
	fundingTxIn := wire.NewTxIn(prevOut, nil, nil)

	aliceCfg := channeldb.ChannelConfig{
		ChannelConstraints: channeldb.ChannelConstraints{
			DustLimit:        aliceDustLimit,
			MaxPendingAmount: lnwire.MilliSatoshi(rand.Int63()),
			ChanReserve:      btcutil.Amount(rand.Int63()),
			MinHTLC:          lnwire.MilliSatoshi(rand.Int63()),
			MaxAcceptedHtlcs: uint16(rand.Int31()),
			CsvDelay:         uint16(csvTimeoutAlice),
		},
		MultiSigKey: keychain.KeyDescriptor{
			PubKey: aliceKeyPub,
		},
		RevocationBasePoint: keychain.KeyDescriptor{
			PubKey: aliceKeyPub,
		},
		PaymentBasePoint: keychain.KeyDescriptor{
			PubKey: aliceKeyPub,
		},
		DelayBasePoint: keychain.KeyDescriptor{
			PubKey: aliceKeyPub,
		},
		HtlcBasePoint: keychain.KeyDescriptor{
			PubKey: aliceKeyPub,
		},
	}
	bobCfg := channeldb.ChannelConfig{
		ChannelConstraints: channeldb.ChannelConstraints{
			DustLimit:        bobDustLimit,
			MaxPendingAmount: lnwire.MilliSatoshi(rand.Int63()),
			ChanReserve:      btcutil.Amount(rand.Int63()),
			MinHTLC:          lnwire.MilliSatoshi(rand.Int63()),
			MaxAcceptedHtlcs: uint16(rand.Int31()),
			CsvDelay:         uint16(csvTimeoutBob),
		},
		MultiSigKey: keychain.KeyDescriptor{
			PubKey: bobKeyPub,
		},
		RevocationBasePoint: keychain.KeyDescriptor{
			PubKey: bobKeyPub,
		},
		PaymentBasePoint: keychain.KeyDescriptor{
			PubKey: bobKeyPub,
		},
		DelayBasePoint: keychain.KeyDescriptor{
			PubKey: bobKeyPub,
		},
		HtlcBasePoint: keychain.KeyDescriptor{
			PubKey: bobKeyPub,
		},
	}

	bobRoot, err := chainhash.NewHash(bobKeyPriv.Serialize())
	if err != nil {
		return nil, nil, nil, err
	}
	bobPreimageProducer := shachain.NewRevocationProducer(*bobRoot)
	bobFirstRevoke, err := bobPreimageProducer.AtIndex(0)
	if err != nil {
		return nil, nil, nil, err
	}
	bobCommitPoint := input.ComputeCommitmentPoint(bobFirstRevoke[:])

	aliceRoot, err := chainhash.NewHash(aliceKeyPriv.Serialize())
	if err != nil {
		return nil, nil, nil, err
	}
	alicePreimageProducer := shachain.NewRevocationProducer(*aliceRoot)
	aliceFirstRevoke, err := alicePreimageProducer.AtIndex(0)
	if err != nil {
		return nil, nil, nil, err
	}
	aliceCommitPoint := input.ComputeCommitmentPoint(aliceFirstRevoke[:])

	aliceCommitTx, bobCommitTx, err := lnwallet.CreateCommitmentTxns(
		channelBal, channelBal, &aliceCfg, &bobCfg, aliceCommitPoint,
		bobCommitPoint, *fundingTxIn, channeldb.SingleFunderTweaklessBit,
	)
	if err != nil {
		return nil, nil, nil, err
	}

	alicePath, err := ioutil.TempDir("", "alicedb")
	if err != nil {
		return nil, nil, nil, err
	}

	dbAlice, err := channeldb.Open(alicePath)
	if err != nil {
		return nil, nil, nil, err
	}

	bobPath, err := ioutil.TempDir("", "bobdb")
	if err != nil {
		return nil, nil, nil, err
	}

	dbBob, err := channeldb.Open(bobPath)
	if err != nil {
		return nil, nil, nil, err
	}

	estimator := chainfee.NewStaticEstimator(12500, 0)
	feePerKw, err := estimator.EstimateFeePerKW(1)
	if err != nil {
		return nil, nil, nil, err
	}

	// TODO(roasbeef): need to factor in commit fee?
	aliceCommit := channeldb.ChannelCommitment{
		CommitHeight:  0,
		LocalBalance:  lnwire.NewMSatFromSatoshis(channelBal),
		RemoteBalance: lnwire.NewMSatFromSatoshis(channelBal),
		FeePerKw:      btcutil.Amount(feePerKw),
		CommitFee:     feePerKw.FeeForWeight(input.CommitWeight),
		CommitTx:      aliceCommitTx,
		CommitSig:     bytes.Repeat([]byte{1}, 71),
	}
	bobCommit := channeldb.ChannelCommitment{
		CommitHeight:  0,
		LocalBalance:  lnwire.NewMSatFromSatoshis(channelBal),
		RemoteBalance: lnwire.NewMSatFromSatoshis(channelBal),
		FeePerKw:      btcutil.Amount(feePerKw),
		CommitFee:     feePerKw.FeeForWeight(input.CommitWeight),
		CommitTx:      bobCommitTx,
		CommitSig:     bytes.Repeat([]byte{1}, 71),
	}

	var chanIDBytes [8]byte
	if _, err := io.ReadFull(crand.Reader, chanIDBytes[:]); err != nil {
		return nil, nil, nil, err
	}

	shortChanID := lnwire.NewShortChanIDFromInt(
		binary.BigEndian.Uint64(chanIDBytes[:]),
	)

	aliceChannelState := &channeldb.OpenChannel{
		LocalChanCfg:            aliceCfg,
		RemoteChanCfg:           bobCfg,
		IdentityPub:             aliceKeyPub,
		FundingOutpoint:         *prevOut,
		ShortChannelID:          shortChanID,
		ChanType:                channeldb.SingleFunderTweaklessBit,
		IsInitiator:             true,
		Capacity:                channelCapacity,
		RemoteCurrentRevocation: bobCommitPoint,
		RevocationProducer:      alicePreimageProducer,
		RevocationStore:         shachain.NewRevocationStore(),
		LocalCommitment:         aliceCommit,
		RemoteCommitment:        aliceCommit,
		Db:                      dbAlice,
		Packager:                channeldb.NewChannelPackager(shortChanID),
		FundingTxn:              channels.TestFundingTx,
	}
	bobChannelState := &channeldb.OpenChannel{
		LocalChanCfg:            bobCfg,
		RemoteChanCfg:           aliceCfg,
		IdentityPub:             bobKeyPub,
		FundingOutpoint:         *prevOut,
		ChanType:                channeldb.SingleFunderTweaklessBit,
		IsInitiator:             false,
		Capacity:                channelCapacity,
		RemoteCurrentRevocation: aliceCommitPoint,
		RevocationProducer:      bobPreimageProducer,
		RevocationStore:         shachain.NewRevocationStore(),
		LocalCommitment:         bobCommit,
		RemoteCommitment:        bobCommit,
		Db:                      dbBob,
		Packager:                channeldb.NewChannelPackager(shortChanID),
	}

	// Set custom values on the channel states.
	updateChan(aliceChannelState, bobChannelState)

	aliceAddr := &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 18555,
	}

	if err := aliceChannelState.SyncPending(aliceAddr, 0); err != nil {
		return nil, nil, nil, err
	}

	bobAddr := &net.TCPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 18556,
	}

	if err := bobChannelState.SyncPending(bobAddr, 0); err != nil {
		return nil, nil, nil, err
	}

	cleanUpFunc := func() {
		os.RemoveAll(bobPath)
		os.RemoveAll(alicePath)
	}

	aliceSigner := &mock.SingleSigner{Privkey: aliceKeyPriv}
	bobSigner := &mock.SingleSigner{Privkey: bobKeyPriv}

	alicePool := lnwallet.NewSigPool(1, aliceSigner)
	channelAlice, err := lnwallet.NewLightningChannel(
		aliceSigner, aliceChannelState, alicePool,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	_ = alicePool.Start()

	bobPool := lnwallet.NewSigPool(1, bobSigner)
	channelBob, err := lnwallet.NewLightningChannel(
		bobSigner, bobChannelState, bobPool,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	_ = bobPool.Start()

	chainIO := &mock.ChainIO{
		BestHeight: broadcastHeight,
	}
	wallet := &lnwallet.LightningWallet{
		WalletController: &mock.WalletController{
			RootKey:               aliceKeyPriv,
			PublishedTransactions: publTx,
		},
	}

	_, currentHeight, err := chainIO.GetBestBlock()
	if err != nil {
		return nil, nil, nil, err
	}

	htlcSwitch, err := htlcswitch.New(htlcswitch.Config{
		DB:             dbAlice,
		SwitchPackager: channeldb.NewSwitchPackager(),
		Notifier:       notifier,
		FwdEventTicker: ticker.New(
			htlcswitch.DefaultFwdEventInterval),
		LogEventTicker: ticker.New(
			htlcswitch.DefaultLogInterval),
		AckEventTicker: ticker.New(
			htlcswitch.DefaultAckInterval),
	}, uint32(currentHeight))
	if err != nil {
		return nil, nil, nil, err
	}
	if err = htlcSwitch.Start(); err != nil {
		return nil, nil, nil, err
	}

	nodeSignerAlice := netann.NewNodeSigner(aliceKeySigner)

	const chanActiveTimeout = time.Minute

	chanStatusMgr, err := netann.NewChanStatusManager(&netann.ChanStatusConfig{
		ChanStatusSampleInterval: 30 * time.Second,
		ChanEnableTimeout:        chanActiveTimeout,
		ChanDisableTimeout:       2 * time.Minute,
		DB:                       dbAlice,
		Graph:                    dbAlice.ChannelGraph(),
		MessageSigner:            nodeSignerAlice,
		OurPubKey:                aliceKeyPub,
		IsChannelActive:          htlcSwitch.HasActiveLink,
		ApplyChannelUpdate:       func(*lnwire.ChannelUpdate) error { return nil },
	})
	if err != nil {
		return nil, nil, nil, err
	}
	if err = chanStatusMgr.Start(); err != nil {
		return nil, nil, nil, err
	}

	errBuffer, err := queue.NewCircularBuffer(ErrorBufferSize)
	if err != nil {
		return nil, nil, nil, err
	}

	var pubKey [33]byte
	copy(pubKey[:], aliceKeyPub.SerializeCompressed())

	cfgAddr := &lnwire.NetAddress{
		IdentityKey: aliceKeyPub,
		Address:     aliceAddr,
		ChainNet:    wire.SimNet,
	}

	cfg := &Config{
		Addr:        cfgAddr,
		PubKeyBytes: pubKey,
		ErrorBuffer: errBuffer,
		ChainIO:     chainIO,
		Switch:      htlcSwitch,

		ChanActiveTimeout: chanActiveTimeout,
		InterceptSwitch:   htlcswitch.NewInterceptableSwitch(htlcSwitch),

		ChannelDB:      dbAlice,
		FeeEstimator:   estimator,
		Wallet:         wallet,
		ChainNotifier:  notifier,
		ChanStatusMgr:  chanStatusMgr,
		DisconnectPeer: func(b *btcec.PublicKey) error { return nil },
	}

	alicePeer := NewBrontide(*cfg)

	chanID := lnwire.NewChanIDFromOutPoint(channelAlice.ChannelPoint())
	alicePeer.activeChannels[chanID] = channelAlice

	alicePeer.wg.Add(1)
	go alicePeer.channelManager()

	return alicePeer, channelBob, cleanUpFunc, nil
}

type mockMessageConn struct {
	t *testing.T

	// MessageConn embeds our interface so that the mock does not need to
	// implement every function. The mock will panic if an unspecified function
	// is called.
	MessageConn

	// writtenMessages is a channel that our mock pushes written messages into.
	writtenMessages chan []byte
}

func newMockConn(t *testing.T, expectedMessages int) *mockMessageConn {
	return &mockMessageConn{
		t:               t,
		writtenMessages: make(chan []byte, expectedMessages),
	}
}

// SetWriteDeadline mocks setting write deadline for our conn.
func (m *mockMessageConn) SetWriteDeadline(time.Time) error {
	return nil
}

// Flush mocks a message conn flush.
func (m *mockMessageConn) Flush() (int, error) {
	return 0, nil
}

// WriteMessage mocks sending of a message on our connection. It will push
// the bytes sent into the mock's writtenMessages channel.
func (m *mockMessageConn) WriteMessage(msg []byte) error {
	select {
	case m.writtenMessages <- msg:
	case <-time.After(timeout):
		m.t.Fatalf("timeout sending message: %v", msg)
	}

	return nil
}

// assertWrite asserts that our mock as had WriteMessage called with the byte
// slice we expect.
func (m *mockMessageConn) assertWrite(expected []byte) {
	select {
	case actual := <-m.writtenMessages:
		require.Equal(m.t, expected, actual)

	case <-time.After(timeout):
		m.t.Fatalf("timeout waiting for write: %v", expected)
	}
}
