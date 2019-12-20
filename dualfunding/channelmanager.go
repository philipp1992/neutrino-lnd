package dualfunding

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"github.com/btcsuite/btcd/wire"
	"github.com/coreos/bbolt"
	"os"
	"path/filepath"
	"sync"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
)

///////////////////////////////////////////////////////////////////////////////

const (
	dbName           = "dualfunding.db"
	dbFilePermission = 0600
)

var (
	openDualChannelsBucket = []byte("open-dual-chan")
	byteOrder = binary.BigEndian
)

type PendingChannelClient struct {
	ChannelOpened <-chan *channeldb.OpenChannel
	Cancel func()
}

type DualChannelConfig struct {

	Self *btcec.PublicKey
	DbPath string

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

	SubscribePendingChannels func() (*PendingChannelClient, error)
}

///////////////////////////////////////////////////////////////////////////////

type DualChannel struct {
	theirOutpoint wire.OutPoint
	ourOutpoint wire.OutPoint
}

type PendingDualChannel struct {
	DualChannel
	opening bool
}

type dualChannelManager struct {
	started sync.Once
	stopped sync.Once
	db *bbolt.DB
	cfg *DualChannelConfig
	dualFundingRequests chan interface{}
	quit chan struct{}
	wg   sync.WaitGroup

	// chanState tracks the current set of open channels for given peer
	chanState    map[NodeID]DualChannel
	chanStateMtx sync.Mutex

	// pendingOpenCloses tracks the channels that we've requested to be
	// initiated, but haven't yet been confirmed as being fully opened.
	// This state is required as otherwise, we may go over our allotted
	// channel limit, or open multiple channels to the same node.
	pendingOpenCloses map[NodeID]PendingDualChannel
	pendingMtx        sync.Mutex
}

// fileExists returns true if the file exists, and false otherwise.
func fileExists(path string) bool {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}

	return true
}

func createDualFundingDB(dbPath string) error {
	if !fileExists(dbPath) {
		if err := os.MkdirAll(dbPath, 0700); err != nil {
			return err
		}
	}

	path := filepath.Join(dbPath, dbName)
	bdb, err := bbolt.Open(path, dbFilePermission, nil)
	if err != nil {
		return err
	}


	err = bdb.Update(func(tx *bbolt.Tx) error {
		if _, err := tx.CreateBucket(openDualChannelsBucket); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("unable to create new channeldb")
	}

	return bdb.Close()
}

func openDualFundingDb(dbPath string) (*bbolt.DB, error) {
	path := filepath.Join(dbPath, dbName)

	if !fileExists(path) {
		if err := createDualFundingDB(dbPath); err != nil {
			return nil, err
		}
	}

	// Specify bbolt freelist options to reduce heap pressure in case the
	// freelist grows to be very large.
	options := &bbolt.Options{
		NoFreelistSync: true,
		FreelistType:   bbolt.FreelistMapType,
	}

	bdb, err := bbolt.Open(path, dbFilePermission, options)
	if err != nil {
		return nil, err
	}

	return bdb, nil
}

func putDualChannelInfo(nodeBucket *bbolt.Bucket, nodeID NodeID, theirOutpoint wire.OutPoint, ourOutpoint wire.OutPoint) error {
	var b bytes.Buffer

	if err := lnwire.WriteElements(&b, theirOutpoint, ourOutpoint); err != nil {
		return err
	}

	return nodeBucket.Put(nodeID[:], b.Bytes())
}

func (dc *dualChannelManager) deleteDualChannelInfo(nodeID NodeID) error {
	return dc.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(openDualChannelsBucket)
		return bucket.Delete(nodeID[:])
	})
}

func (dc *dualChannelManager) syncDualChannelInfo(nodeID NodeID, theirOutpoint wire.OutPoint, ourOutpoint wire.OutPoint) error {
	return dc.db.Update(func(tx *bbolt.Tx) error {
		dualChannelsBucket, err := tx.CreateBucketIfNotExists(openDualChannelsBucket)
		if err != nil {
			return err
		}

		return putDualChannelInfo(dualChannelsBucket, nodeID, theirOutpoint, ourOutpoint)
	})
}

func (dc *dualChannelManager) fetchDualChannelInfo() map[NodeID]DualChannel {
	channels := make(map[NodeID]DualChannel)
	err := dc.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(openDualChannelsBucket).ForEach(func(k, v []byte) error {
			var dualChannel DualChannel
			err := lnwire.ReadElements(bytes.NewReader(v), &dualChannel.theirOutpoint, &dualChannel.ourOutpoint)
			if err != nil {
				return err
			}

			var n NodeID
			copy(n[:], k)
			channels[n] = dualChannel

			return nil
		})
	})

	if err != nil {
		return channels
	}

	return channels
}

///////////////////////////////////////////////////////////////////////////////
// newDualChannelManager creates and initializes a new instance of the
// dualChannelManager.
func NewDualChannelManager(cfg *DualChannelConfig) (*dualChannelManager, error) {
	chManager :=  &dualChannelManager {
		cfg:                 cfg,
		chanState:           make(map[NodeID]DualChannel),
		dualFundingRequests: make(chan interface{}, msgBufferSize),
		quit:                make(chan struct{}),
		pendingOpenCloses:   make(map[NodeID]PendingDualChannel),
	}

	var err error

	chManager.db, err = openDualFundingDb(cfg.DbPath)

	if err != nil {
		return nil, err
	}

	fetchedChannels := chManager.fetchDualChannelInfo()

	for _, c := range cfg.Channels {
		nodeID := NewNodeID(c.IdentityPub)
		var dc DualChannel
		var ok bool
		if dc, ok = fetchedChannels[nodeID]; !ok {
			continue
		}

		if c.IsPending {
			if dc.ourOutpoint == c.FundingOutpoint {
				// it has to be our pending opening channel
				chManager.pendingOpenCloses[nodeID] = PendingDualChannel{
					dc,
					true,
				}
			}
		} else {
			if dc.ourOutpoint == c.FundingOutpoint {
				ok := func() bool {
					for _, ch := range cfg.Channels {
						if ch.FundingOutpoint == dc.theirOutpoint && !ch.IsPending {
							// here we are sure that both channels were found
							chManager.chanState[nodeID] = dc
							return true
						}
					}
					return false
				}()

				if !ok {
					// means we didn't find their channel, which is bad
					// TODO(yuraolex): maybe handle this case
					log.Errorf("Didn't find open channel for our dual channel: %v %v",
						dc.theirOutpoint, dc.ourOutpoint)
				}
			}
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

	pendingChannelsSubscription, err := dc.cfg.SubscribePendingChannels()

	// start listening to channel changes notifications
	dc.wg.Add(2)

	go func() {
		defer pendingChannelsSubscription.Cancel()
		defer dc.wg.Done()

		for {
			select {
			case pendingChannel, ok := <-pendingChannelsSubscription.ChannelOpened:
				if !ok {
					return
				}

				if !pendingChannel.IsPending || pendingChannel.IsInitiator {
					return
				}

				dc.handleNewDualChannelRequest(pendingChannel)
			case <-dc.quit:
				return
			}
		}

	}()

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

					if edgeUpdate.AdvertisingNode.IsEqual(dc.cfg.Self) {
						// means state one of our channels has changed
						dc.handleOurChannelOpened(edgeUpdate)
					}
				}

				// For each closed channel, we'll obtain
				// the chanID of the closed channel and send it
				// to the pilot.
				for _, chanClose := range topChange.ClosedChannels {
					dc.handleDualChannelCloseRequest(chanClose)
					dc.handleOurChannelClosed(chanClose)
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

func (dc *dualChannelManager) handleNewDualChannelRequest(channel *channeldb.OpenChannel) {

	log.Infof("List of existing chans ", dc.chanState)


	nodeID := NewNodeID(channel.IdentityPub)

	dc.chanStateMtx.Lock()
	if _, ok := dc.chanState[nodeID]; ok {
		log.Infof("Such id exist in db %v", nodeID)
		dc.chanStateMtx.Unlock()
		return
	}
	dc.chanStateMtx.Unlock()


	dc.pendingMtx.Lock()
	if _, ok := dc.pendingOpenCloses[nodeID]; ok {
		log.Infof("Already have a pending channel to peer: %v", nodeID)
		dc.pendingMtx.Unlock()
		return
	}
	dc.pendingMtx.Unlock()


	pendingOutpoint, err := dc.cfg.ChanController.OpenChannel(channel.IdentityPub, channel.Capacity)

	if err == nil {
		// If we were successful, we'll track this peer in our set of pending
		// opens. We do this here to ensure we don't stall on selecting new
		// peers if the connection attempt happens to take too long.
		dc.pendingMtx.Lock()
		dc.pendingOpenCloses[nodeID] = PendingDualChannel{
			DualChannel: DualChannel {
				ourOutpoint:   *pendingOutpoint,
				theirOutpoint: channel.FundingOutpoint,
			},
			opening: true,
		}
		dc.pendingMtx.Unlock()
		log.Infof("Opening channel back to %s", nodeID)

		err = dc.syncDualChannelInfo(nodeID, channel.FundingOutpoint, *pendingOutpoint)
		if err != nil {
			log.Warnf("Unable to write info into db for %v %v",
				nodeID, err)
		}

	} else {
		log.Warnf("Unable to open channel to %x of %v: %v",
			nodeID, channel.Capacity, err)

	}
}

func (dc *dualChannelManager) handleDualChannelCloseRequest(summary *routing.ClosedChanSummary) {

	var (
		nodeID NodeID
		ok bool
		dualChannel DualChannel
	)

	dc.chanStateMtx.Lock()
	
	// mark it as opened channel
	nodeID, ok = func() (NodeID, bool) {
		for nodeID, dcn := range dc.chanState {
			if dcn.theirOutpoint == summary.ChanPoint || dcn.ourOutpoint == summary.ChanPoint {
				dualChannel = dcn
				return nodeID, true
			}
		}

		return NodeID{}, false
	}()

	delete(dc.chanState, nodeID)
	dc.chanStateMtx.Unlock()

	if !ok {
		return
	}

	// check in case we have this channel in open channels,
	// and peer has closed our dual channel
	if dualChannel.ourOutpoint == summary.ChanPoint {
		// means that peer closed our dual channel, remove from db
		if err := dc.deleteDualChannelInfo(nodeID); err != nil {
			log.Errorf("Failed to remove info from db for dual channel with node %v %v", nodeID, err)
		}
		
		return
	}

	// it means that peer has closed his channel, we need to close our channel as well.
	log.Infof("Closing channel back with node %v %v", nodeID, dualChannel.ourOutpoint.String())

	err := dc.cfg.ChanController.CloseChannel(&dualChannel.ourOutpoint)

	if err != nil {
		dc.chanStateMtx.Lock()
		dc.chanState[nodeID] = dualChannel
		dc.chanStateMtx.Unlock()

		log.Infof("Failed to close channel with node %v %v", nodeID, summary.ChanPoint.String(), err)
	} else {
		dc.pendingMtx.Lock()
		dc.pendingOpenCloses[nodeID] = PendingDualChannel{
			DualChannel: dualChannel,
			opening: false,
		}
		dc.pendingMtx.Unlock()
		log.Infof("Made closing channel request with node %v %v", nodeID, summary.ChanPoint.String())
	}
}

func (dc *dualChannelManager) handleOurChannelOpened(update *routing.ChannelEdgeUpdate) {
	log.Infof("Our channel changed state %d %v", update.ChanPoint.String())

	nodeID := NewNodeID(update.ConnectingNode)

	var (
		pendingChannel PendingDualChannel
		ok             bool
	)

	dc.pendingMtx.Lock()
	if pendingChannel, ok = dc.pendingOpenCloses[nodeID]; !ok {
		// means that our channel is not pending as dual funded,
		// we don't know anything about it
		dc.pendingMtx.Unlock()
		return
	}

	// means that update is regarding closing channel, so just skip this
	if pendingChannel.opening == false {
		dc.pendingMtx.Unlock()
		return
	}

	delete(dc.pendingOpenCloses, nodeID)
	dc.pendingMtx.Unlock()

	dc.chanStateMtx.Lock()
	// mark it as opened channel
	dc.chanState[nodeID] = pendingChannel.DualChannel
	dc.chanStateMtx.Unlock()
}

func (dc *dualChannelManager) handleOurChannelClosed(summary *routing.ClosedChanSummary) {
	var (
		nodeID NodeID
		ok bool
		dualChannel DualChannel
	)

	dc.pendingMtx.Lock()

	nodeID, ok = func() (NodeID, bool) {
		for nodeID, dcn := range dc.pendingOpenCloses {
			if !dcn.opening && dcn.ourOutpoint == summary.ChanPoint {
				dualChannel = dcn.DualChannel
				return nodeID, true
			}
		}

		return NodeID{}, false
	}()

	if !ok {
		dc.pendingMtx.Unlock()
		return
	}

	delete(dc.pendingOpenCloses, nodeID)
	dc.pendingMtx.Unlock()

	// it means that our channel was closed, at this point we can
	// safely delete everything regarding this peer
	if err := dc.deleteDualChannelInfo(nodeID); err != nil {
		log.Errorf("Failed to remove info from db for dual channel with node %v %v", nodeID, err)
	}
}

///////////////////////////////////////////////////////////////////////////////