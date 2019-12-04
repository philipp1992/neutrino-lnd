package dualfunding

import (
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
)

// channelState is a type that represents the set of active channels of the
// backing LN node that the Agent should be aware of. This type contains a few
// helper utility methods.
type channelState map[uint64]Channel

// Channel is a simple struct which contains relevant details of a particular
// channel within the channel graph. The fields in this struct may be used a
// signals for various AttachmentHeuristic implementations.
type Channel struct {
	// ChanID is the short channel ID for this channel as defined within
	// BOLT-0007.
	ChanID uint64

	// Capacity is the capacity of the channel expressed in satoshis.
	Capacity btcutil.Amount

	FundedAmt btcutil.Amount

	// Node is the peer that this channel has been established with.
	Node NodeID
}

// NodeID is a simple type that holds an EC public key serialized in compressed
// format.
type NodeID [33]byte

// NewNodeID creates a new nodeID from a passed public key.
func NewNodeID(pub *btcec.PublicKey) NodeID {
	var n NodeID
	copy(n[:], pub.SerializeCompressed())
	return n
}

///////////////////////////////////////////////////////////////////////////////

const (
	msgBufferSize = 50
)

// ChannelController is a simple interface that allows an auto-pilot agent to
// open a channel within the graph to a target peer, close targeted channels,
// or add/remove funds from existing channels via a splice in/out mechanisms.
type ChannelManager interface {
	// OpenChannel opens a channel to a target peer, using at most amt
	// funds. This means that the resulting channel capacity might be
	// slightly less to account for fees. This function should un-block
	// immediately after the funding transaction that marks the channel
	// open has been broadcast.
	OpenChannel(target *btcec.PublicKey, amt btcutil.Amount) error

	// CloseChannel attempts to close out the target channel.
	CloseChannel(chanPoint *wire.OutPoint) error
}
