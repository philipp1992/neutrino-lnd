package lnd

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcutil"
	//"github.com/lightningnetwork/lnd/autopilot"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/htlcswitch"
	//"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/sweep"
	"github.com/lightningnetwork/lnd/dualfunding"

)

// chanController is an implementation of the autopilot.ChannelController
// interface that's backed by a running lnd instance.
type chanManager struct {
	server     *server
}

// OpenChannel opens a channel to a target peer, with a capacity of the
// specified amount. This function should un-block immediately after the
// funding transaction that marks the channel open has been broadcast.
func (c *chanManager) OpenChannel(target *btcec.PublicKey,
	amt btcutil.Amount) (*wire.OutPoint, error) {

	// With the connection established, we'll now establish our connection
	// to the target peer, waiting for the first update before we exit.
	feePerKw, err := c.server.cc.feeEstimator.EstimateFeePerKW(
		6,
	)
	if err != nil {
		return nil, err
	}

	// TODO(halseth): make configurable?
	minHtlc := lnwire.NewMSatFromSatoshis(1)

	// Construct the open channel request and send it to the server to begin
	// the funding workflow.
	req := &openChanReq{
		targetPubkey:    target,
		chainHash:       *activeNetParams.GenesisHash,
		subtractFees:    false,
		localFundingAmt: amt,
		pushAmt:         0,
		minHtlc:         minHtlc,
		fundingFeePerKw: feePerKw,
		private:         false,
		remoteCsvDelay:  0,
		minConfs:        1,
	}

	updateStream, errChan := c.server.OpenChannel(req)
	select {
	case err := <-errChan:
		return nil, err
	case update := <-updateStream:

		pendingChan := update.GetChanPending()
		txHash, _ := chainhash.NewHash(pendingChan.Txid)

		chanPoint := wire.OutPoint{
			Hash: *txHash,
			Index: pendingChan.OutputIndex,

		}

		return &chanPoint, nil

	case <-c.server.quit:
		return nil, nil
	}
}

func (c *chanManager) CloseChannel(chanPoint *wire.OutPoint) error {

	var (
		updateChan chan interface{}
		errChan    chan error
	)

	feeRate, err := sweep.DetermineFeePerKw(
		c.server.cc.feeEstimator, sweep.FeePreference{
			ConfTarget: 6,
		},
	)
	if err != nil {
		return err
	}

	dchnLog.Debugf("Target sat/kw for closing transaction: %v",
		int64(feeRate))

	// Otherwise, the caller has requested a regular interactive
	// cooperative channel closure. So we'll forward the request to
	// the htlc switch which will handle the negotiation and
	// broadcast details.
	updateChan, errChan = c.server.htlcSwitch.CloseLink(
		chanPoint, htlcswitch.CloseRegular, feeRate,
	)

out:
	for {
		select {
		case err := <-errChan:
			dchnLog.Errorf("[closechannel] unable to close "+
				"ChannelPoint(%v): %v", chanPoint, err)
			return err
		case closingUpdate := <-updateChan:
			dchnLog.Infof("Closing update", closingUpdate)
			break out

		}
	}

	return nil

}

// A compile time assertion to ensure chanController meets the
// autopilot.ChannelController interface.
var _ dualfunding.ChannelManager = (*chanManager)(nil)

// initAutoPilot initializes a new autopilot.ManagerCfg to manage an
// autopilot.Agent instance based on the passed configuration struct. The agent
// and all interfaces needed to drive it won't be launched before the Manager's
// StartAgent method is called.
func initDualFunding(svr *server, graphDir string) (*dualfunding.DualChannelConfig, error) {

	// With the heuristic itself created, we can now populate the remainder
	// of the items that the autopilot agent needs to perform its duties.
	self := svr.identityPriv.PubKey()

	// add active flag check
	activeChannels, err := svr.chanDB.FetchAllChannels()
	if err != nil {
		err := fmt.Errorf("Unable to fetch active channels: %v", err)
		ltndLog.Error(err)
		return nil, err
	}

	return &dualfunding.DualChannelConfig {
		Self: self,
		Channels: activeChannels,
		ChanController: &chanManager{
			server:     svr,
		},
		SubscribeTopology: svr.chanRouter.SubscribeTopology,
		DbPath: graphDir,
	}, nil

	//Graph:       autopilot.ChannelGraphFromDatabase(svr.chanDB.ChannelGraph()),
}
