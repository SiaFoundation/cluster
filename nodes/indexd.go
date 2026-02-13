package nodes

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.sia.tech/cluster/internal/mock"
	"go.sia.tech/core/gateway"
	"go.sia.tech/core/types"
	"go.sia.tech/coreutils"
	"go.sia.tech/coreutils/chain"
	"go.sia.tech/coreutils/syncer"
	"go.sia.tech/coreutils/testutil"
	"go.sia.tech/coreutils/wallet"
	"go.sia.tech/indexd/accounts"
	"go.sia.tech/indexd/alerts"
	"go.sia.tech/indexd/api/admin"
	"go.sia.tech/indexd/api/app"
	"go.sia.tech/indexd/client"
	client2 "go.sia.tech/indexd/client/v2"
	"go.sia.tech/indexd/contracts"
	"go.sia.tech/indexd/geoip"
	"go.sia.tech/indexd/hosts"
	"go.sia.tech/indexd/keys"
	"go.sia.tech/indexd/persist/postgres"
	"go.sia.tech/indexd/pins"
	"go.sia.tech/indexd/slabs"
	"go.sia.tech/indexd/subscriber"
	"go.sia.tech/jape"
	"go.uber.org/zap"
	"lukechampine.com/frand"
)

// StartIndexd starts a new indexd node. It listens on random ports and registers
// itself with the Manager. This function blocks until the context is
// canceled. All resources will be cleaned up before the function returns.
func (m *Manager) StartIndexd(ctx context.Context, sk types.PrivateKey, pgPort int, ready chan<- struct{}) (err error) {
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

	pk := sk.PublicKey()
	node := Node{
		ID:            NodeID(pk[:8]),
		Type:          NodeTypeIndexd,
		WalletAddress: types.StandardUnlockHash(pk),
	}
	log := m.log.Named("indexd." + node.ID.String())

	dir, err := createNodeDir(m.dir, node.ID)
	if err != nil {
		return fmt.Errorf("failed to create node directory: %w", err)
	}
	defer os.RemoveAll(dir)

	adminListener, err := net.Listen("tcp", ":0")
	if err != nil {
		return fmt.Errorf("failed to listen on admin address: %w", err)
	}
	defer adminListener.Close()

	appListener, err := net.Listen("tcp", ":0")
	if err != nil {
		return fmt.Errorf("failed to listen on app address: %w", err)
	}
	defer appListener.Close()

	network := m.chain.TipState().Network
	genesisIndex, ok := m.chain.BestIndex(0)
	if !ok {
		return errors.New("failed to get genesis index")
	}

	var cm *chain.Manager
	if m.shareConsensus {
		cm = m.chain
	} else {
		// start a chain manager
		genesis, ok := m.chain.Block(genesisIndex.ID)
		if !ok {
			return errors.New("failed to get genesis block")
		}
		bdb, err := coreutils.OpenBoltChainDB(filepath.Join(dir, "consensus.db"))
		if err != nil {
			return fmt.Errorf("failed to open bolt db: %w", err)
		}
		defer bdb.Close()
		dbstore, tipState, err := chain.NewDBStore(bdb, network, genesis, nil)
		if err != nil {
			return fmt.Errorf("failed to create dbstore: %w", err)
		}

		cm = chain.NewManager(dbstore, tipState)
	}

	// start a syncer
	syncerListener, err := net.Listen("tcp", ":0")
	if err != nil {
		return fmt.Errorf("failed to listen on syncer address: %w", err)
	}
	defer syncerListener.Close()

	s := syncer.New(syncerListener, cm, testutil.NewEphemeralPeerStore(), gateway.Header{
		GenesisID:  genesisIndex.ID,
		UniqueID:   gateway.GenerateUniqueID(),
		NetAddress: syncerListener.Addr().String(),
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

	// open the postgres store
	store, err := postgres.NewStore(ctx, postgres.ConnectionInfo{
		Host:     "127.0.0.1",
		Port:     pgPort,
		User:     "postgres",
		Password: "postgres",
		Database: node.ID.String(),
		SSLMode:  "disable",
	}, contracts.MaintenanceSettings{
		Enabled:         true,
		Period:          144,
		RenewWindow:     72,
		WantedContracts: 12,
	}, hosts.UsabilitySettings{}, log.Named("postgres"))
	if err != nil {
		return fmt.Errorf("failed to create postgres store: %w", err)
	}
	defer store.Close()

	wm, err := wallet.NewSingleAddressWallet(sk, cm, store, s, wallet.WithLogger(log.Named("wallet")), wallet.WithReservationDuration(3*time.Hour))
	if err != nil {
		return fmt.Errorf("failed to create wallet: %w", err)
	}
	defer wm.Close()

	locator, err := geoip.NewMaxMindLocator("")
	if err != nil {
		return fmt.Errorf("failed to create geoip locator: %w", err)
	}

	hc2 := client2.New(client2.NewProvider(hosts.NewHostStore(store)))
	alerter := alerts.NewManager()

	hm, err := hosts.NewManager(s, locator, hc2, store, alerter,
		hosts.WithLogger(log.Named("hosts")),
		hosts.WithScanFrequency(200*time.Millisecond),
		hosts.WithScanInterval(time.Second))
	if err != nil {
		return fmt.Errorf("failed to create host manager: %w", err)
	}
	defer hm.Close()

	signer := contracts.NewFormContractSigner(wm, sk)
	dialer := client.NewDialer(cm, signer, store, log, client.WithRevisionSubmissionBuffer(1))

	am, err := accounts.NewManager(store,
		accounts.WithPruneAccountsInterval(100*time.Millisecond),
		accounts.WithLogger(log.Named("accounts")))
	if err != nil {
		return fmt.Errorf("failed to create accounts manager: %w", err)
	}
	defer am.Close()

	f := contracts.NewFunder(hc2, signer, cm, store, log, contracts.WithRevisionSubmissionBuffer(1))

	contractsMgr, err := contracts.NewManager(sk, am, f, cm, store, dialer, hm, s, wm,
		contracts.WithLogger(log.Named("contracts")),
		contracts.WithMaintenanceFrequency(500*time.Millisecond),
		contracts.WithMinHostDistance(0),
		contracts.WithSyncPollInterval(500*time.Millisecond),
		contracts.WithSectorRootsBatchSize(5))
	if err != nil {
		return fmt.Errorf("failed to create contract manager: %w", err)
	}
	defer contractsMgr.Close()

	slabsMgr, err := slabs.NewManager(cm, am, contractsMgr, hm, store, hc2, alerter,
		keys.DerivePrivateKey(sk, "migration"),
		keys.DerivePrivateKey(sk, "integrity"),
		slabs.WithLogger(log.Named("slabs")),
		slabs.WithHealthCheckInterval(500*time.Millisecond),
		slabs.WithMinHostDistance(0))
	if err != nil {
		return fmt.Errorf("failed to create slab manager: %w", err)
	}
	defer slabsMgr.Close()

	sub, err := subscriber.New(cm, hm, contractsMgr, wm, store, subscriber.WithLogger(log.Named("subscriber")))
	if err != nil {
		return fmt.Errorf("failed to create subscriber: %w", err)
	}
	defer sub.Close()

	explorer := mock.NewIndexdExplorer()

	pm, err := pins.NewManager(explorer, hm, store)
	if err != nil {
		return fmt.Errorf("failed to create pin manager: %w", err)
	}
	defer pm.Close()

	// start admin API
	password := hex.EncodeToString(frand.Bytes(16))
	adminHandler := jape.BasicAuth(password)(admin.NewAPI(cm, am, contractsMgr, hm, pm, s, wm, store, alerter,
		admin.WithDebug(),
		admin.WithLogger(log.Named("api.admin")),
		admin.WithExplorer(explorer)))

	adminServer := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "*")
			w.Header().Set("Access-Control-Allow-Headers", "*")
			w.Header().Set("Access-Control-Expose-Headers", "*")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			} else if strings.HasPrefix(r.URL.Path, "/api") {
				r.URL.Path = strings.TrimPrefix(r.URL.Path, "/api")
				adminHandler.ServeHTTP(w, r)
				return
			}
			http.NotFound(w, r)
		}),
		ReadTimeout: 15 * time.Second,
	}
	defer adminServer.Close()
	go adminServer.Serve(adminListener)

	// start app API
	appAPIAddr := fmt.Sprintf("http://%s", appListener.Addr().String())
	appHandler, err := app.NewAPI(appAPIAddr, store, am, contractsMgr, slabsMgr,
		app.WithLogger(log.Named("api.app")))
	if err != nil {
		return fmt.Errorf("failed to create app API: %w", err)
	}

	appServer := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "*")
			w.Header().Set("Access-Control-Allow-Headers", "*")
			w.Header().Set("Access-Control-Expose-Headers", "*")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			appHandler.ServeHTTP(w, r)
		}),
		ReadTimeout: 15 * time.Second,
	}
	defer appServer.Close()
	go appServer.Serve(appListener)

	node.APIAddress = "http://" + adminListener.Addr().String()
	node.Password = password

	log.Debug("node setup complete")

	// mine blocks to fund the wallet
	walletAddress := types.StandardUnlockHash(sk.PublicKey())
	if err := m.MineBlocks(ctx, int(network.MaturityDelay)+20, walletAddress); err != nil {
		return fmt.Errorf("failed to mine blocks: %w", err)
	}

	// wait for the subscriber to process all mined blocks
	for {
		idx, err := store.LastScannedIndex()
		if err != nil {
			return fmt.Errorf("failed to get last scanned index: %w", err)
		}
		if idx == cm.Tip() {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}

	log.Info("node started",
		zap.String("network", cm.TipState().Network.Name),
		zap.String("admin", adminListener.Addr().String()),
		zap.String("app", appListener.Addr().String()),
		zap.String("p2p", string(s.Addr())))
	m.addNodeAndWait(ctx, node, ready)
	return nil
}
