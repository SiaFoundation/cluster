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

const (
	NodeTypeRenterd = NodeType("renterd")
	NodeTypeHostd   = NodeType("hostd")
	NodeTypeWalletd = NodeType("walletd")
)

type NodeType string

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

type Node struct {
	ID   NodeID   `json:"id"`
	Type NodeType `json:"type"`

	APIAddress string `json:"apiAddress"`
	Password   string `json:"password"`

	WalletAddress types.Address `json:"walletAddress"`
}

type Manager struct {
	mu    sync.Mutex
	nodes map[NodeID]Node
}

func (m *Manager) Put(node Node) {
	m.mu.Lock()
	m.nodes[node.ID] = node
	m.mu.Unlock()
}

func (m *Manager) Delete(id NodeID) {
	m.mu.Lock()
	delete(m.nodes, id)
	m.mu.Unlock()
}

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

func NewManager() *Manager {
	return &Manager{
		nodes: make(map[NodeID]Node),
	}
}
