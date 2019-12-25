package lnd

import (
	"errors"
	"fmt"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"sync"
	"sync/atomic"

	//"github.com/lightningnetwork/lnd/autopilot"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/dualfunding"
	//"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/sweep"
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

	minHtlc := lnwire.NewMSatFromSatoshis(1)

	// Construct the open channel request and send it to the server to begin
	// the funding workflow.
	req := &openChanReq{
		targetPubkey:    target,
		chainHash:       *activeNetParams.GenesisHash,
		subtractFees:    false,
		localFundingAmt: amt,
		pushAmt:         0,
		minHtlcIn:       minHtlc,
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
		chanPoint, htlcswitch.CloseRegular, feeRate, nil)

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

type updateClient struct {
	// ntfnChan is a send-only channel that's used to propagate
	// notification s from the channel router to an instance of a
	// topologyClient client.
	ntfnChan chan<- *channeldb.OpenChannel

	// exit is a channel that is used internally by the channel router to
	// cancel any active un-consumed goroutine notifications.
	exit chan struct{}

	wg sync.WaitGroup
}

// clientUpdate is a message sent to the channel router to either
// register a new topology client or re-register an existing client.
type clientUpdate struct {
	// cancel indicates if the update to the client is cancelling an
	// existing client's notifications. If not then this update will be to
	// register a new set of notifications.
	cancel bool

	// clientID is the unique identifier for this client. Any further
	// updates (deleting or adding) to this notification client will be
	// dispatched according to the target clientID.
	clientID uint64

	// ntfnChan is a *send-only* channel in which notifications should be
	// sent over from router -> client.
	ntfnChan chan<- *channeldb.OpenChannel
}

type PendingChannelsEventSource struct {
	pendingChannelsSource chan *channeldb.OpenChannel
	ntfnClientUpdates chan *clientUpdate
	updateClients map[uint64]*updateClient

	ntfnClientCounter uint64 // To be used atomically.

	started uint32 // To be used atomically.
	stopped uint32 // To be used atomically.

	sync.RWMutex

	quit chan struct{}
	wg   sync.WaitGroup
}

func New(pendingChannelsChan chan *channeldb.OpenChannel) (*PendingChannelsEventSource, error) {
	return &PendingChannelsEventSource{
		pendingChannelsSource: pendingChannelsChan,
		ntfnClientUpdates: make(chan *clientUpdate),
		updateClients: make(map[uint64]*updateClient),
		quit: make(chan struct{}),
	}, nil
}

func (ev *PendingChannelsEventSource) Start() {
	if !atomic.CompareAndSwapUint32(&ev.started, 0, 1) {
		return
	}

	ev.wg.Add(1)
	go ev.serveEvents()
}

func (ev *PendingChannelsEventSource) Stop() {
	if !atomic.CompareAndSwapUint32(&ev.stopped, 0, 1) {
		return
	}

	close(ev.quit)
	ev.wg.Wait()
}

func (ev *PendingChannelsEventSource) serveEvents() {
	defer ev.wg.Done()

	for {
		select {
		case pendingChan := <-ev.pendingChannelsSource:
			ev.RLock()
			for _, client := range ev.updateClients {
				client.wg.Add(1)
				go func(c *updateClient) {
					defer client.wg.Done()
					select {

					// In this case we'll try to send the notification
					// directly to the upstream client consumer.
					case c.ntfnChan <- pendingChan:

					// If the client cancels the notifications, then we'll
					// exit early.
					case <-c.exit:

					// Similarly, if the ChannelRouter itself exists early,
					// then we'll also exit ourselves.
					case <-ev.quit:

					}
				}(client)
			}
			ev.RUnlock()

		case ntfnUpdate := <-ev.ntfnClientUpdates:
			clientID := ntfnUpdate.clientID

			if ntfnUpdate.cancel {
				ev.RLock()
				client, ok := ev.updateClients[ntfnUpdate.clientID]
				ev.RUnlock()
				if ok {
					ev.Lock()
					delete(ev.updateClients, clientID)
					ev.Unlock()

					close(client.exit)
					client.wg.Wait()

					close(client.ntfnChan)
				}

				continue
			}

			ev.Lock()
			ev.updateClients[ntfnUpdate.clientID] = &updateClient{
				ntfnChan: ntfnUpdate.ntfnChan,
				exit:     make(chan struct{}),
			}
			ev.Unlock()
		case <- ev.quit:
			return
		}

	}
}

func (ev *PendingChannelsEventSource) SubscribePendingChannels() (*dualfunding.PendingChannelClient, error) {
	// If the router is not yet started, return an error to avoid a
	// deadlock waiting for it to handle the subscription request.
	if atomic.LoadUint32(&ev.started) == 0 {
		return nil, fmt.Errorf("PendingChannelsEventSource not started")
	}

	// We'll first atomically obtain the next ID for this client from the
	// incrementing client ID counter.
	clientID := atomic.AddUint64(&ev.ntfnClientCounter, 1)

	ntfnChan := make(chan *channeldb.OpenChannel, 10)

	select {
	case ev.ntfnClientUpdates <- &clientUpdate{
		cancel:   false,
		clientID: clientID,
		ntfnChan: ntfnChan,
	}:
	case <-ev.quit:
		return nil, errors.New("PendingChannelsEventSource shutting down")
	}

	return &dualfunding.PendingChannelClient{
		ChannelOpened: ntfnChan,
		Cancel: func() {
			select {
			case ev.ntfnClientUpdates <- &clientUpdate{
				cancel:   true,
				clientID: clientID,
			}:
			case <-ev.quit:
				return
			}
		},
	}, nil
}

	// A compile time assertion to ensure chanController meets the
// autopilot.ChannelController interface.
var _ dualfunding.ChannelManager = (*chanManager)(nil)

// initAutoPilot initializes a new autopilot.ManagerCfg to manage an
// autopilot.Agent instance based on the passed configuration struct. The agent
// and all interfaces needed to drive it won't be launched before the Manager's
// StartAgent method is called.
func initDualFunding(svr *server, pendingChannelsEv *PendingChannelsEventSource, graphDir string) (*dualfunding.DualChannelConfig, error) {

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
		SubscribePendingChannels: pendingChannelsEv.SubscribePendingChannels,
		DbPath: graphDir,
	}, nil

	//Graph:       autopilot.ChannelGraphFromDatabase(svr.chanDB.ChannelGraph()),
}
