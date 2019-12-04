package dualfunding

import (
	"sync"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
)

///////////////////////////////////////////////////////////////////////////////

type DualChannelConfig struct {

	Self *btcec.PublicKey

	// ChannelState is a function closure that returns the current set of
	// channels managed by this node.
	// channels managed by this node.
	Channels []*channeldb.OpenChannel

	// ChanController is an interface that is able to directly manage the
	// creation, closing and update of channels within the network.
	ChanController ChannelManager

	// SubscribeTopology is used to get a subscription for topology changes
	// on the network.
	SubscribeTopology func() (*routing.TopologyClient, error)
}

///////////////////////////////////////////////////////////////////////////////

type dualChannelManager struct {
	started sync.Once
	stopped sync.Once

	chanState    channelState

	cfg *DualChannelConfig

	dualFundingRequests chan interface{}

	quit chan struct{}

	wg   sync.WaitGroup
}

///////////////////////////////////////////////////////////////////////////////
// newDualChannelManager creates and initializes a new instance of the
// dualChannelManager.
func NewDualChannelManager(cfg *DualChannelConfig) (*dualChannelManager, error) {
	chManager :=  &dualChannelManager {
		cfg: 			    		cfg,
		chanState:          		make(map[uint64]Channel),
		dualFundingRequests:        make(chan interface{}, msgBufferSize),
		quit: 						make(chan struct{}),
	}

	for _, c := range cfg.Channels {
		chManager.chanState[c.ShortChannelID.ToUint64()] = Channel{
			ChanID: c.ShortChannelID.ToUint64(),
			Capacity: c.Capacity,
			Node: NewNodeID(c.IdentityPub),
		}
	}

	return chManager, nil
}

///////////////////////////////////////////////////////////////////////////////
// Start launches all helper goroutines required for handling requests sent
// to the funding manager.
func (dc *dualChannelManager) Start() error {
	var err error
	dc.started.Do(func() {
		err = dc.start()
	})
	return err
}

///////////////////////////////////////////////////////////////////////////////

func (dc *dualChannelManager) start() error {

	log.Infof("Dual channel controller running")


	graphSubscription, err := dc.cfg.SubscribeTopology()
	if err != nil {
		return err
	}

	// start listening to channel changes notifications
	dc.wg.Add(1)

	go func() {
		defer graphSubscription.Cancel()
		defer dc.wg.Done()

		for {
			select {
			case topChange, ok := <-graphSubscription.TopologyChanges:
				// If the router is shutting down, then we will
				// as well.
				if !ok {
					return
				}

				for _, edgeUpdate := range topChange.ChannelEdgeUpdates {
					// If this isn't an advertisement by
					// the backing lnd node, then we'll
					// continue as we only want to add
					// channels that we've created
					// ourselves.

					//if !edgeUpdate.AdvertisingNode.IsEqual(m.cfg.Self) {
					//	continue
					//}

					dc.handleOpenRequest(edgeUpdate)
				}

				// For each closed channel, we'll obtain
				// the chanID of the closed channel and send it
				// to the pilot.
				for _, chanClose := range topChange.ClosedChannels {
					chanID := lnwire.NewShortChanIDFromInt(
						chanClose.ChanID,
					)

					dc.handleCloseRequest(chanID)
				}

			case <-dc.quit:
				return
			}
		}
	}()

	return nil
}

///////////////////////////////////////////////////////////////////////////////
// Stop signals all helper goroutines to execute a graceful shutdown. This
// method will block until all goroutines have exited.
func (dc *dualChannelManager) Stop() error {
	var err error
	dc.stopped.Do(func() {
		err = dc.stop()
	})
	return err
}

///////////////////////////////////////////////////////////////////////////////

func (dc *dualChannelManager) stop() error {
	log.Infof("Dual channel controller shutting down")

	close(dc.quit)
	dc.wg.Wait()

	return nil
}

func (dc *dualChannelManager) handleOpenRequest(edgeUpdate *routing.ChannelEdgeUpdate) {

	if _, ok := dc.chanState[edgeUpdate.ChanID]; ok {
		return
	}

	log.Infof("Opening channel back to %s", edgeUpdate.AdvertisingNode)


	if edgeUpdate.AdvertisingNode != dc.cfg.Self {
		err := dc.cfg.ChanController.OpenChannel(edgeUpdate.AdvertisingNode, edgeUpdate.Capacity)
		if err != nil {
			log.Errorf("Error %v", err)
			return
		}
	}

	//	log.Warnf("Unable to open channel to %x of %v: %v",
	//		pub.SerializeCompressed(), directive.ChanAmt, err)
	//
	//	// As the attempt failed, we'll clear the peer from the set of
	//	// pending opens and mark them as failed so we don't attempt to
	//	// open a channel to them again.
	//	a.pendingMtx.Lock()
	//	delete(a.pendingOpens, nodeID)
	//	a.failedNodes[nodeID] = struct{}{}
	//	a.pendingMtx.Unlock()
	//
	//	// Trigger the agent to re-evaluate everything and possibly
	//	// retry with a different node.
	//	a.OnChannelOpenFailure()
	//
	//	// Finally, we should also disconnect the peer if we weren't
	//	// already connected to them beforehand by an external
	//	// subsystem.
	//	if alreadyConnected {
	//		return
	//	}
	//
	//	err = a.cfg.DisconnectPeer(pub)
	//	if err != nil {
	//		log.Warnf("Unable to disconnect peer %x: %v",
	//			pub.SerializeCompressed(), err)
	//}

}

func (dc *dualChannelManager) handleCloseRequest(id lnwire.ShortChannelID) {log.Infof("Closing channel back with id %d", id)

}

///////////////////////////////////////////////////////////////////////////////