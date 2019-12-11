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

type DualChannelConfig struct {

	Self *btcec.PublicKey
	dbPath string

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

type DualChannel struct {
	theirOutpoint wire.OutPoint
	ourOutpoint wire.OutPoint
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

	// pendingOpens tracks the channels that we've requested to be
	// initiated, but haven't yet been confirmed as being fully opened.
	// This state is required as otherwise, we may go over our allotted
	// channel limit, or open multiple channels to the same node.
	pendingOpens map[NodeID]DualChannel
	pendingMtx   sync.Mutex
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

///////////////////////////////////////////////////////////////////////////////
// newDualChannelManager creates and initializes a new instance of the
// dualChannelManager.
func NewDualChannelManager(cfg *DualChannelConfig) (*dualChannelManager, error) {
	chManager :=  &dualChannelManager {
		cfg: 			    		cfg,
		chanState:          		make(map[NodeID]DualChannel),
		dualFundingRequests:        make(chan interface{}, msgBufferSize),
		quit: 						make(chan struct{}),
	}

	var err error

	chManager.db, err = openDualFundingDb(cfg.dbPath)

	if err != nil {
		return nil, err
	}

	//for _, c := range cfg.Channels {
	//	chManager.chanState[NewNodeID(c.IdentityPub)] = DualChannel{}c.FundingOutpoint
	//}

	return chManager, nil
}

func putDualChannelInfo(nodeBucket *bbolt.Bucket, nodePub *btcec.PublicKey, theirOutpoint wire.OutPoint, ourOutpoint wire.OutPoint) error {
	compressed := nodePub.SerializeCompressed()
	var b bytes.Buffer

	if err := lnwire.WriteElements(&b, theirOutpoint, ourOutpoint); err != nil {
		return err
	}

	return nodeBucket.Put(compressed, b.Bytes())
}

func (dc *dualChannelManager) syncDualChannelInfo(nodePub *btcec.PublicKey, theirOutpoint wire.OutPoint, ourOutpoint wire.OutPoint) error {
	return dc.db.Update(func(tx *bbolt.Tx) error {
		dualChannelsBucket, err := tx.CreateBucketIfNotExists(openDualChannelsBucket)
		if err != nil {
			return err
		}

		return putDualChannelInfo(dualChannelsBucket, nodePub, theirOutpoint, ourOutpoint)
	})
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

					if edgeUpdate.ConnectingNode.IsEqual(dc.cfg.Self) {
						dc.handleNewDualChannelRequest(edgeUpdate)
						continue
					} else if edgeUpdate.AdvertisingNode.IsEqual(dc.cfg.Self) {

					}
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

func (dc *dualChannelManager) handleNewDualChannelRequest(edgeUpdate *routing.ChannelEdgeUpdate) {

	log.Infof("List of existing chans ", dc.chanState)

	if edgeUpdate.Disabled == true {
		return
	}

	dc.chanStateMtx.Lock()
	if _, ok := dc.chanState[edgeUpdate.ChanID]; ok {
		log.Infof("Such id exist in db ", dc.chanState[edgeUpdate.ChanID])
		dc.chanStateMtx.Unlock()
		return
	}
	dc.chanStateMtx.Unlock()

	nodeID := NewNodeID(edgeUpdate.AdvertisingNode)

	dc.pendingMtx.Lock()
	if _, ok := dc.pendingOpens[nodeID]; ok {
		log.Infof("Already have a pending channel to peer: %v", nodeID)
		dc.pendingMtx.Unlock()
		return
	}
	dc.pendingMtx.Unlock()


	pendingOutpoint, err := dc.cfg.ChanController.OpenChannel(edgeUpdate.AdvertisingNode, edgeUpdate.Capacity)
	if err == nil {
		return
	}

	// If we were successful, we'll track this peer in our set of pending
	// opens. We do this here to ensure we don't stall on selecting new
	// peers if the connection attempt happens to take too long.
	dc.pendingMtx.Lock()
	dc.pendingOpens[nodeID] = DualChannel{
		ourOutpoint: *pendingOutpoint,
		theirOutpoint: edgeUpdate.ChanPoint,
	}
	dc.pendingMtx.Unlock()
	log.Infof("Opening channel back to %s", nodeID)

	err = dc.syncDualChannelInfo(edgeUpdate.AdvertisingNode, edgeUpdate.ChanPoint, *pendingOutpoint)
	if err != nil {
			log.Warnf("Unable to write info into db for %v %v",
				nodeID, err)
	}

	log.Warnf("Unable to open channel to %x of %v: %v",
		nodeID, edgeUpdate.Capacity, err)

	// As the attempt failed, we'll clear the peer from the set of
	// pending opens and mark them as failed so we don't attempt to
	// open a channel to them again.
	dc.pendingMtx.Lock()
	delete(dc.pendingOpens, nodeID)
	dc.pendingMtx.Unlock()

	// Trigger the agent to re-evaluate everything and possibly
	// retry with a different node.
	//a.OnChannelOpenFailure()
}

func (dc *dualChannelManager) handleCloseRequest(id lnwire.ShortChannelID) {
	log.Infof("Closing channel back with id %d", id)
}

///////////////////////////////////////////////////////////////////////////////