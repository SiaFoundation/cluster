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

		SyncerAddress string `json:"syncerAddress"`

		WalletAddress types.Address `json:"walletAddress"`
	}

	// A Manager manages a set of nodes in the cluster.
	Manager struct {
		dir            string
		chain          *chain.Manager
		syncer         *syncer.Syncer
		log            *zap.Logger
		shareConsensus bool

		mu    sync.Mutex
		wg    sync.WaitGroup
		close chan struct{}
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

// incrementWaitGroup increments the manager's waitgroup. If the manager is
// closed, it returns an error.
func (m *Manager) incrementWaitGroup() (func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	select {
	case <-m.close:
		return nil, errors.New("manager closed")
	default:
		m.wg.Add(1)
		return m.wg.Done, nil
	}
}

// addNodeAndWait is a helper function for managing a node's lifetime. It adds
// the node to the manager, waits for the context to be canceled or Close to be
// called, and then removes the node from the manager.
func (m *Manager) addNodeAndWait(ctx context.Context, node Node, ready chan<- struct{}) {
	m.mu.Lock()
	// add the node to the manager
	m.nodes[node.ID] = node
	defer func() {
		// remove the node from the manager
		m.mu.Lock()
		delete(m.nodes, node.ID)
		m.mu.Unlock()
	}()

	if ready != nil {
		// signal that the node is ready
		ready <- struct{}{}
	}
	m.mu.Unlock()

	// wait for the context to be canceled or Close to be called
	select {
	case <-ctx.Done():
	case <-m.close:
	}
}

// Close closes the manager and all running nodes and waits for them to exit.
func (m *Manager) Close() error {
	m.mu.Lock()

	select {
	case <-m.close:
		m.mu.Unlock()
		return errors.New("manager already closed")
	default:
		close(m.close)
		m.mu.Unlock()
		m.wg.Wait()
		return nil
	}
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

// Renterd returns a slice of all running renterd nodes in the manager.
func (m *Manager) Renterd() []Node {
	m.mu.Lock()
	defer m.mu.Unlock()
	var nodes []Node
	for _, n := range m.nodes {
		if n.Type == NodeTypeRenterd {
			nodes = append(nodes, n)
		}
	}
	return nodes
}

// Hostd returns a slice of all running hostd nodes in the manager.
func (m *Manager) Hostd() []Node {
	m.mu.Lock()
	defer m.mu.Unlock()
	var nodes []Node
	for _, n := range m.nodes {
		if n.Type == NodeTypeHostd {
			nodes = append(nodes, n)
		}
	}
	return nodes
}

// Walletd returns a slice of all running walletd nodes in the manager.
func (m *Manager) Walletd() []Node {
	m.mu.Lock()
	defer m.mu.Unlock()
	var nodes []Node
	for _, n := range m.nodes {
		if n.Type == NodeTypeWalletd {
			nodes = append(nodes, n)
		}
	}
	return nodes
}

// MineBlocks mines n blocks to the provided reward address.
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

func createNodeDir(baseDir string, id NodeID) (dir string, err error) {
	dir = filepath.Join(baseDir, id.String())
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("failed to create node directory: %w", err)
	}
	return
}

// NewManager creates a new node manager.
func NewManager(dir string, cm *chain.Manager, s *syncer.Syncer, opts ...Option) *Manager {
	m := &Manager{
		dir:    dir,
		chain:  cm,
		syncer: s,
		log:    zap.NewNop(),

		nodes: make(map[NodeID]Node),

		close: make(chan struct{}),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}
