package mock

import (
	"context"
	"time"

	"go.sia.tech/core/gateway"
	"go.sia.tech/core/types"
	"go.sia.tech/coreutils/syncer"
	"lukechampine.com/frand"
)

// A Syncer mocks the Syncer interface for testing purposes.
type Syncer struct{}

// Addr returns the address of the Syncer.
func (s *Syncer) Addr() string {
	return ""
}

// BroadcastHeader broadcasts a block header to the network.
func (s *Syncer) BroadcastHeader(h types.BlockHeader) error { return nil }

// BroadcastV2BlockOutline broadcasts a V2 block outline to the network.
func (s *Syncer) BroadcastV2BlockOutline(bo gateway.V2BlockOutline) error { return nil }

// BroadcastTransactionSet broadcasts a set of transactions to the network.
func (s *Syncer) BroadcastTransactionSet([]types.Transaction) error { return nil }

// BroadcastV2TransactionSet broadcasts a set of V2 transactions to the network.
func (s *Syncer) BroadcastV2TransactionSet(index types.ChainIndex, txns []types.V2Transaction) error {
	return nil
}

// PeerInfo returns information about the Syncer.
func (s *Syncer) PeerInfo(addr string) (syncer.PeerInfo, error) {
	return syncer.PeerInfo{
		Address:      addr,
		FirstSeen:    time.Now(),
		LastConnect:  time.Now(),
		SyncedBlocks: frand.Uint64n(10000),
		SyncDuration: time.Duration(frand.Intn(10000)) * time.Millisecond,
	}, nil
}

// Connect connects to a peer at the given address.
func (s *Syncer) Connect(ctx context.Context, addr string) (*syncer.Peer, error) {
	return new(syncer.Peer), nil
}

// Peers returns a list of 10 peers.
func (s *Syncer) Peers() (peers []*syncer.Peer) {
	for i := 0; i < 10; i++ {
		peers = append(peers, &syncer.Peer{
			ConnAddr: "",
			Inbound:  false,
		})
	}
	return
}

// NewSyncer creates a new mock Syncer.
func NewSyncer() *Syncer {
	return &Syncer{}
}
