package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"go.sia.tech/cluster/nodes"
	"go.sia.tech/core/consensus"
	"go.sia.tech/core/gateway"
	"go.sia.tech/core/types"
	"go.sia.tech/coreutils/chain"
	"go.sia.tech/jape"
	"go.uber.org/zap"
)

type (
	// A ChainManager manages blockchain and txpool state.
	ChainManager interface {
		UpdatesSince(types.ChainIndex, int) ([]chain.RevertUpdate, []chain.ApplyUpdate, error)

		Tip() types.ChainIndex
		TipState() consensus.State
		AddBlocks([]types.Block) error
		PoolTransactions() []types.Transaction
		V2PoolTransactions() []types.V2Transaction
	}

	// A Syncer can connect to other peers and synchronize the blockchain.
	Syncer interface {
		BroadcastHeader(bh gateway.BlockHeader)
		BroadcastTransactionSet(txns []types.Transaction)
		BroadcastV2TransactionSet(index types.ChainIndex, txns []types.V2Transaction)
		BroadcastV2BlockOutline(bo gateway.V2BlockOutline)
	}

	// Nodes manages the set of nodes in the cluster.
	Nodes interface {
		Nodes() []nodes.Node

		MineBlocks(ctx context.Context, n int, rewardAddress types.Address) error
		ProxyRequest(ctx context.Context, filter, httpMethod, path string, r io.Reader) ([]nodes.ProxyResponse, error)
	}

	server struct {
		chain  ChainManager
		syncer Syncer
		nodes  Nodes
		log    *zap.Logger
	}
)

func (srv *server) getNodes(jc jape.Context) {
	jc.Encode(srv.nodes.Nodes())
}

func (srv *server) postMine(jc jape.Context) {
	var req MineRequest
	if err := jc.Decode(&req); err != nil {
		return
	}
	jc.Check("failed to mine", srv.nodes.MineBlocks(jc.Request.Context(), req.Blocks, req.Address))
}

func (srv *server) proxyNodeAPI(jc jape.Context) {
	var nodeID string
	if err := jc.DecodeParam("id", &nodeID); err != nil {
		return
	}

	var path string
	if err := jc.DecodeParam("path", &path); err != nil {
		return
	}

	responses, err := srv.nodes.ProxyRequest(jc.Request.Context(), nodeID, jc.Request.Method, path, jc.Request.Body)
	if jc.Check("failed to proxy request", err) != nil {
		return
	}

	resp := make([]ProxyResponse, 0, len(responses))
	for _, r := range responses {
		var errStr string
		if r.Error != nil {
			errStr = r.Error.Error()
		}

		resp = append(resp, ProxyResponse{
			NodeID:     r.NodeID,
			StatusCode: r.StatusCode,
			Error:      errStr, // errors are not JSON-serializable
			Data:       r.Data,
		})
	}

	enc := json.NewEncoder(jc.ResponseWriter)
	enc.SetIndent("", "  ")
	if err := enc.Encode(resp); err != nil {
		srv.log.Error("failed to encode response", zap.Error(err), zap.String("data", string(resp[0].Data)))
	}
}

// Handler returns an http.Handler that serves the API.
func Handler(cm ChainManager, s Syncer, n Nodes, log *zap.Logger) http.Handler {
	srv := &server{
		chain:  cm,
		syncer: s,
		nodes:  n,
		log:    log,
	}

	return jape.Mux(map[string]jape.Handler{
		"GET /nodes": srv.getNodes,

		// proxyNodeAPI is a catch-all route that proxies requests to a specific node or a type of node's API.
		"GET /nodes/proxy/:id/*path":    srv.proxyNodeAPI,
		"PUT /nodes/proxy/:id/*path":    srv.proxyNodeAPI,
		"POST /nodes/proxy/:id/*path":   srv.proxyNodeAPI,
		"PATCH /nodes/proxy/:id/*path":  srv.proxyNodeAPI,
		"DELETE /nodes/proxy/:id/*path": srv.proxyNodeAPI,

		"POST /mine": srv.postMine,
	})
}
