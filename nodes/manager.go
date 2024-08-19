package nodes

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"go.sia.tech/core/types"
)

// Types for the supported nodes.
const (
	NodeTypeRenterd = NodeType("renterd")
	NodeTypeHostd   = NodeType("hostd")
	NodeTypeWalletd = NodeType("walletd")
)

// A NodeType represents the type of a node.
type NodeType string

// A NodeID is a unique identifier for a node.
type NodeID [8]byte

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

// A Node represents a running node in the cluster.
type Node struct {
	ID   NodeID   `json:"id"`
	Type NodeType `json:"type"`

	APIAddress string `json:"apiAddress"`
	Password   string `json:"password"`

	WalletAddress types.Address `json:"walletAddress"`
}

// A Manager manages a set of nodes in the cluster.
type Manager struct {
	mu    sync.Mutex
	nodes map[NodeID]Node
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

// NewManager creates a new node manager.
func NewManager() *Manager {
	return &Manager{
		nodes: make(map[NodeID]Node),
	}
}
