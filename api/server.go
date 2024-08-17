package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"

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

	Nodes interface {
		Nodes() []nodes.Node
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

	log := srv.log.Named("miner")
	ctx := jc.Request.Context()

	for n := req.Blocks; n > 0; {
		b, err := mineBlock(ctx, srv.chain, req.Address)
		if errors.Is(err, context.Canceled) {
			log.Error("mining canceled")
			return
		} else if err != nil {
			log.Warn("failed to mine block", zap.Error(err))
		} else if err := srv.chain.AddBlocks([]types.Block{b}); err != nil {
			log.Warn("failed to add block", zap.Error(err))
		}

		if b.V2 == nil {
			srv.syncer.BroadcastHeader(gateway.BlockHeader{
				ParentID:   b.ParentID,
				Nonce:      b.Nonce,
				Timestamp:  b.Timestamp,
				MerkleRoot: b.MerkleRoot(),
			})
		} else {
			srv.syncer.BroadcastV2BlockOutline(gateway.OutlineBlock(b, srv.chain.PoolTransactions(), srv.chain.V2PoolTransactions()))
		}

		log.Debug("mined block", zap.Stringer("blockID", b.ID()))
		n--
	}
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

	log := srv.log.Named("proxy").With(zap.String("path", path), zap.String("method", jc.Request.Method))
	active := srv.nodes.Nodes()
	requestReaders := make([]io.ReadCloser, 0, len(active))
	requestWriters := make([]*io.PipeWriter, 0, len(active))
	writers := make([]io.Writer, 0, len(active))
	backends := active[:0]
	for _, node := range active {
		if node.Type == nodes.NodeType(nodeID) || node.ID.String() == nodeID {
			r, w := io.Pipe()
			requestReaders = append(requestReaders, r)
			requestWriters = append(requestWriters, w)
			writers = append(writers, w)
			backends = append(backends, node)
			log.Debug("matched node", zap.Stringer("node", node.ID), zap.String("type", string(node.Type)), zap.String("apiURL", node.APIAddress))
		}
	}

	// pipe the request body to all backends
	mw := io.MultiWriter(writers...)
	tr := io.TeeReader(jc.Request.Body, mw)

	go func() {
		io.Copy(io.Discard, tr)
		for _, rw := range requestWriters {
			rw.Close()
		}
	}()

	var wg sync.WaitGroup
	responseCh := make(chan ProxyResponse, len(backends))

	ctx := jc.Request.Context()
	r := jc.Request

	for i, node := range backends {
		proxyReq := r.Clone(ctx)
		u, err := url.Parse(fmt.Sprintf("%s%s", node.APIAddress, path))
		if err != nil {
			jc.Error(err, http.StatusInternalServerError)
			return
		}
		proxyReq.URL = u
		proxyReq.URL.RawQuery = r.URL.RawQuery
		proxyReq.Body = requestReaders[i]
		proxyReq.SetBasicAuth("", node.Password)
		srv.log.Debug("proxying request", zap.String("node", node.ID.String()), zap.String("apiURL", node.APIAddress), zap.String("path", path), zap.Stringer("url", proxyReq.URL))

		wg.Add(1)
		go func(node nodes.Node, req *http.Request) {
			defer wg.Done()

			resp, err := http.DefaultTransport.RoundTrip(proxyReq)
			if err != nil {
				log.Debug("failed to send request", zap.Error(err))
				responseCh <- ProxyResponse{NodeID: node.ID, Error: err.Error()}
				return
			}
			defer resp.Body.Close()

			data, err := io.ReadAll(resp.Body)
			if err != nil {
				log.Debug("failed to read response body", zap.Error(err))
				responseCh <- ProxyResponse{NodeID: node.ID, Error: err.Error()}
				return
			}
			log.Debug("received response", zap.String("status", resp.Status), zap.Int("size", len(data)))
			responseCh <- ProxyResponse{NodeID: node.ID, StatusCode: resp.StatusCode, Data: data}
		}(node, proxyReq)
	}

	go func() {
		wg.Wait()
		close(responseCh)
	}()

	var responses []ProxyResponse
	for resp := range responseCh {
		log.Debug("adding response", zap.Stringer("node", resp.NodeID), zap.Int("status", resp.StatusCode), zap.Int("size", len(resp.Data)))
		responses = append(responses, resp)
	}
	log.Debug("all responses received", zap.Int("count", len(responses)))
	jc.Encode(responses)
}

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
