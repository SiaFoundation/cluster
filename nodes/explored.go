package nodes

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.sia.tech/core/gateway"
	"go.sia.tech/coreutils"
	"go.sia.tech/coreutils/chain"
	"go.sia.tech/coreutils/syncer"
	"go.sia.tech/coreutils/testutil"
	"go.sia.tech/explored/api"
	"go.sia.tech/explored/config"
	"go.sia.tech/explored/exchangerates"
	"go.sia.tech/explored/explorer"
	"go.sia.tech/explored/persist/sqlite"
	"go.uber.org/zap"
	"lukechampine.com/frand"
)

// StartExplored starts a new explored node. It listens on random ports and registers
// itself with the Manager. This function blocks until the context is
// canceled. All restources will be cleaned up before the funcion returns.
func (m *Manager) StartExplored(ctx context.Context, ready chan<- struct{}) (err error) {
	defer func() {
		if r := recover(); r != nil && err == nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()

	done, err := m.incrementWaitGroup()
	if err != nil {
		return err
	}
	defer done()

	node := Node{
		ID:   NodeID(frand.Bytes(8)),
		Type: NodeTypeExplored,
	}
	log := m.log.Named("explored." + node.ID.String())

	dir, err := createNodeDir(m.dir, node.ID)
	if err != nil {
		return fmt.Errorf("failed to create node directory: %w", err)
	}
	defer os.RemoveAll(dir)

	httpListener, err := net.Listen("tcp", ":0")
	if err != nil {
		return fmt.Errorf("failed to listen on http address: %w", err)
	}
	defer httpListener.Close()

	network := m.chain.TipState().Network

	var cm *chain.Manager
	var s *syncer.Syncer
	if m.shareConsensus {
		cm = m.chain
		s = m.syncer
	} else {
		// start a chain manager
		genesisIndex, ok := m.chain.BestIndex(0)
		if !ok {
			return errors.New("failed to get genesis index")
		}
		genesis, ok := m.chain.Block(genesisIndex.ID)
		if !ok {
			return errors.New("failed to get genesis block")
		}
		bdb, err := coreutils.OpenBoltChainDB(filepath.Join(dir, "consensus.db"))
		if err != nil {
			return fmt.Errorf("failed to open bolt db: %w", err)
		}
		defer bdb.Close()
		dbstore, tipState, err := chain.NewDBStore(bdb, network, genesis)
		if err != nil {
			return fmt.Errorf("failed to create dbstore: %w", err)
		}

		cm = chain.NewManager(dbstore, tipState)

		syncerListener, err := net.Listen("tcp", ":0")
		if err != nil {
			return fmt.Errorf("failed to listen on syncer address: %w", err)
		}
		defer syncerListener.Close()

		// start a syncer
		_, port, err := net.SplitHostPort(syncerListener.Addr().String())
		if err != nil {
			return fmt.Errorf("failed to split syncer address: %w", err)
		}
		s = syncer.New(syncerListener, cm, testutil.NewEphemeralPeerStore(), gateway.Header{
			GenesisID:  genesisIndex.ID,
			UniqueID:   gateway.GenerateUniqueID(),
			NetAddress: "127.0.0.1" + port,
		}, syncer.WithLogger(log.Named("syncer")),
			syncer.WithPeerDiscoveryInterval(5*time.Second),
			syncer.WithSyncInterval(5*time.Second),
			syncer.WithMaxInboundPeers(10000),
			syncer.WithMaxOutboundPeers(10000))
		defer s.Close()
		go s.Run()

		node.SyncerAddress = syncerListener.Addr().String()
		// connect to the cluster syncer
		_, err = m.syncer.Connect(ctx, node.SyncerAddress)
		if err != nil {
			return fmt.Errorf("failed to connect to cluster syncer: %w", err)
		}
	}

	store, err := sqlite.OpenDatabase(filepath.Join(dir, "explored.sqlite3"), log.Named("sqlite3"))
	if err != nil {
		return fmt.Errorf("failed to open sqlite database: %w", err)
	}
	defer store.Close()

	e, err := explorer.NewExplorer(cm, store, 10, config.Scanner{
		BatchSize:           4,
		CheckAgainDelay:     1 * time.Second,
		Timeout:             10 * time.Second,
		MaxLastScan:         5 * time.Minute,
		MinLastAnnouncement: 365 * 24 * time.Hour,
	}, log.Named("explorer"))
	if err != nil {
		return fmt.Errorf("failed to create explorer: %w", err)
	}
	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer timeoutCancel()
	defer e.Shutdown(timeoutCtx)

	var sources []exchangerates.Source
	sources = append(sources, exchangerates.NewKraken(map[string]string{
		exchangerates.CurrencyUSD: exchangerates.KrakenPairSiacoinUSD,
		exchangerates.CurrencyEUR: exchangerates.KrakenPairSiacoinEUR,
		exchangerates.CurrencyBTC: exchangerates.KrakenPairSiacoinBTC,
	}, time.Minute))

	ex, err := exchangerates.NewAverager(true, sources...)
	if err != nil {
		return fmt.Errorf("failed to create exchange rate source: %w", err)
	}
	go ex.Start(ctx)

	api := api.NewServer(e, cm, s, ex)
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api") {
				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.Header().Set("Access-Control-Allow-Methods", "*")
				w.Header().Set("Access-Control-Allow-Headers", "*")
				w.Header().Set("Access-Control-Expose-Headers", "*")
				if r.Method == http.MethodOptions {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				r.URL.Path = strings.TrimPrefix(r.URL.Path, "/api")
				api.ServeHTTP(w, r)
				return
			}
			http.NotFound(w, r)
		}),
		ReadTimeout: 15 * time.Second,
	}
	defer server.Close()
	go server.Serve(httpListener)

	node.APIAddress = "http://" + httpListener.Addr().String()

	waitForSync := func() error {
		tip, err := e.Tip()
		if err != nil {
			return err
		}
		// wait for sync
		for m.chain.Tip() != tip {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
		return nil
	}

	if err := waitForSync(); err != nil {
		return fmt.Errorf("failed to wait for sync: %w", err)
	}

	log.Info("node started", zap.String("network", cm.TipState().Network.Name), zap.String("http", httpListener.Addr().String()))
	m.addNodeAndWait(ctx, node, ready)

	return nil
}
