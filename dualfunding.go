package lnd

import (
	//"errors"
	"fmt"
	//"net"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/autopilot"
	"github.com/lightningnetwork/lnd/lnwire"
	//"github.com/lightningnetwork/lnd/tor"
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
	amt btcutil.Amount) error {

	// With the connection established, we'll now establish our connection
	// to the target peer, waiting for the first update before we exit.
	feePerKw, err := c.server.cc.feeEstimator.EstimateFeePerKW(
		6,
	)
	if err != nil {
		return err
	}

	// TODO(halseth): make configurable?
	minHtlc := lnwire.NewMSatFromSatoshis(1)

	// Construct the open channel request and send it to the server to begin
	// the funding workflow.
	req := &openChanReq{
		targetPubkey:    target,
		chainHash:       *activeNetParams.GenesisHash,
		subtractFees:    true,
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
		return err
	case <-updateStream:
		return nil
	case <-c.server.quit:
		return nil
	}

	return nil
}

func (c *chanManager) CloseChannel(chanPoint *wire.OutPoint) error {
	return nil
}
func (c *chanManager) SpliceIn(chanPoint *wire.OutPoint,
	amt btcutil.Amount) (*autopilot.Channel, error) {
	return nil, nil
}
func (c *chanManager) SpliceOut(chanPoint *wire.OutPoint,
	amt btcutil.Amount) (*autopilot.Channel, error) {
	return nil, nil
}

// A compile time assertion to ensure chanController meets the
// autopilot.ChannelController interface.
var _ dualfunding.ChannelManager = (*chanManager)(nil)

// initAutoPilot initializes a new autopilot.ManagerCfg to manage an
// autopilot.Agent instance based on the passed configuration struct. The agent
// and all interfaces needed to drive it won't be launched before the Manager's
// StartAgent method is called.
func initDualFunding(svr *server) (*dualfunding.DualChannelConfig, error) {

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
		ChanController: &chanController{
			server:     svr,
		},
		SubscribeTopology: svr.chanRouter.SubscribeTopology,
	}, nil

	//Graph:       autopilot.ChannelGraphFromDatabase(svr.chanDB.ChannelGraph()),
}
