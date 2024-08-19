package nodes

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"go.sia.tech/core/gateway"
	"go.sia.tech/core/types"
	"go.sia.tech/coreutils/chain"
	"go.sia.tech/coreutils/syncer"
	"go.uber.org/zap"
)

// Types for the supported nodes.
const (
	NodeTypeRenterd = NodeType("renterd")
	NodeTypeHostd   = NodeType("hostd")
	NodeTypeWalletd = NodeType("walletd")
)

type (
	// A NodeType represents the type of a node.
	NodeType string

	// A NodeID is a unique identifier for a node.
	NodeID [8]byte

	// A Node represents a running node in the cluster.
	Node struct {
		ID   NodeID   `json:"id"`
		Type NodeType `json:"type"`

		APIAddress string `json:"apiAddress"`
		Password   string `json:"password"`

		WalletAddress types.Address `json:"walletAddress"`
	}

	// A Manager manages a set of nodes in the cluster.
	Manager struct {
		dir    string
		chain  *chain.Manager
		syncer *syncer.Syncer
		log    *zap.Logger

		mu    sync.Mutex
		nodes map[NodeID]Node
	}
)

// String returns the hexadecimal representation of the NodeID.
func (id NodeID) String() string {
	return hex.EncodeToString(id[:])
}

// MarshalText implements encoding.TextMarshaler.
func (id NodeID) MarshalText() ([]byte, error) {
	return []byte(id.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (id *NodeID) UnmarshalText(text []byte) error {
	if len(text) != 16 {
		return errors.New("invalid NodeID length")
	}
	_, err := hex.Decode(id[:], text)
	return err
}

// Put adds a node to the manager.
func (m *Manager) Put(node Node) {
	m.mu.Lock()
	m.nodes[node.ID] = node
	m.mu.Unlock()
}

// Delete removes a node from the manager.
// The node is not stopped.
func (m *Manager) Delete(id NodeID) {
	m.mu.Lock()
	delete(m.nodes, id)
	m.mu.Unlock()
}

// Nodes returns a slice of all running nodes in the manager.
func (m *Manager) Nodes() []Node {
	m.mu.Lock()
	defer m.mu.Unlock()
	nodes := make([]Node, 0, len(m.nodes))
	for _, v := range m.nodes {
		nodes = append(nodes, v)
	}
	return nodes
}

func createNodeDir(baseDir string, id NodeID) (dir string, err error) {
	dir = filepath.Join(baseDir, id.String())
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("failed to create node directory: %w", err)
	}
	return
}

func (m *Manager) MineBlocks(ctx context.Context, n int, rewardAddress types.Address) error {
	log := m.log.Named("mine")

	for n > 0 {
		b, err := mineBlock(ctx, m.chain, rewardAddress)
		if errors.Is(err, context.Canceled) {
			return err
		} else if err != nil {
			log.Warn("failed to mine block", zap.Error(err)) // failure to mine a block is not a critical error
			continue
		} else if err := m.chain.AddBlocks([]types.Block{b}); err != nil {
			log.Warn("failed to add block", zap.Error(err)) // failure to add a block is not *necessarily* a critical error
			continue
		}

		if b.V2 == nil {
			m.syncer.BroadcastHeader(gateway.BlockHeader{
				ParentID:   b.ParentID,
				Nonce:      b.Nonce,
				Timestamp:  b.Timestamp,
				MerkleRoot: b.MerkleRoot(),
			})
		} else {
			m.syncer.BroadcastV2BlockOutline(gateway.OutlineBlock(b, m.chain.PoolTransactions(), m.chain.V2PoolTransactions()))
		}

		log.Debug("mined block", zap.Stringer("blockID", b.ID()))
		n--
	}
	return nil
}

// NewManager creates a new node manager.
func NewManager(dir string, cm *chain.Manager, s *syncer.Syncer, log *zap.Logger) *Manager {
	return &Manager{
		dir:    dir,
		chain:  cm,
		syncer: s,
		log:    log,

		nodes: make(map[NodeID]Node),
	}
}
