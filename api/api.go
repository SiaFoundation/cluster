package api

import (
	"go.sia.tech/cluster/nodes"
	"go.sia.tech/core/types"
)

// MineRequest is the request type for [POST] /mine.
type MineRequest struct {
	Blocks  int           `json:"blocks"`
	Address types.Address `json:"address"`
}

// A ProxyResponse is the response for a proxied API request from a node.
type ProxyResponse struct {
	NodeID     nodes.NodeID    `json:"nodeID"`
	StatusCode int             `json:"statusCode"`
	Error      string          `json:"error,omitempty"`
	Data       nodes.ProxyData `json:"data"`
}
