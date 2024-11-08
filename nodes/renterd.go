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
	"go.sia.tech/core/types"
	"go.sia.tech/coreutils"
	"go.sia.tech/coreutils/chain"
	"go.sia.tech/coreutils/syncer"
	"go.sia.tech/coreutils/testutil"
	"go.sia.tech/coreutils/wallet"
	"go.sia.tech/jape"
	"go.sia.tech/renterd/alerts"
	"go.sia.tech/renterd/api"
	"go.sia.tech/renterd/autopilot"
	"go.sia.tech/renterd/bus"
	"go.sia.tech/renterd/config"
	"go.sia.tech/renterd/stores"
	"go.sia.tech/renterd/stores/sql/sqlite"
	"go.sia.tech/renterd/webhooks"
	"go.sia.tech/renterd/worker"
	"go.uber.org/zap"
)

// StartRenterd starts a new renterd node and adds it to the manager.
// This function blocks until the context is canceled. All resources will be
// cleaned up before the function returns.
func (m *Manager) StartRenterd(ctx context.Context, sk types.PrivateKey, ready chan<- struct{}) (err error) {
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
		ID:            NodeID(pk[:]),
		Type:          NodeTypeRenterd,
		WalletAddress: types.StandardUnlockHash(pk),
	}
	log := m.log.Named("renterd." + node.ID.String())

	dir, err := createNodeDir(m.dir, node.ID)
	if err != nil {
		return fmt.Errorf("failed to create node directory: %w", err)
	}
	defer os.RemoveAll(dir)

	syncerListener, err := net.Listen("tcp", ":0")
	if err != nil {
		return fmt.Errorf("failed to listen on syncer address: %w", err)
	}
	defer syncerListener.Close()

	apiListener, err := net.Listen("tcp", ":0")
	if err != nil {
		return fmt.Errorf("failed to listen on http address: %w", err)
	}
	defer apiListener.Close()

	// start a chain manager
	network := m.chain.TipState().Network
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
	cm := chain.NewManager(dbstore, tipState)

	// start a syncer
	_, port, err := net.SplitHostPort(syncerListener.Addr().String())
	if err != nil {
		return fmt.Errorf("failed to split syncer address: %w", err)
	}
	s := syncer.New(syncerListener, cm, testutil.NewMemPeerStore(), gateway.Header{
		GenesisID:  genesisIndex.ID,
		UniqueID:   gateway.GenerateUniqueID(),
		NetAddress: "127.0.0.1:" + port,
	}, syncer.WithLogger(log.Named("syncer")), syncer.WithPeerDiscoveryInterval(5*time.Second), syncer.WithSyncInterval(5*time.Second), syncer.WithMaxOutboundPeers(10000), syncer.WithMaxInboundPeers(10000))
	defer s.Close()
	go s.Run(ctx)
	node.SyncerAddress = syncerListener.Addr().String()
	// connect to the cluster syncer
	_, err = m.syncer.Connect(ctx, node.SyncerAddress)
	if err != nil {
		return fmt.Errorf("failed to connect to cluster syncer: %w", err)
	}
	// connect to other nodes in the cluster
	for _, n := range m.Nodes() {
		_, err = s.Connect(ctx, n.SyncerAddress)
		if err != nil {
			log.Debug("failed to connect to peer syncer", zap.Stringer("peer", n.ID), zap.Error(err))
		}
	}

	db, err := sqlite.Open(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		return fmt.Errorf("failed to open SQLite database: %w", err)
	}
	defer db.Close()

	dbMain, err := sqlite.NewMainDatabase(db, log.Named("sqlite"), time.Second, time.Second)
	if err != nil {
		return fmt.Errorf("failed to create SQLite database: %w", err)
	}

	dbm, err := sqlite.Open(filepath.Join(dir, "metrics.sqlite"))
	if err != nil {
		return fmt.Errorf("failed to open SQLite metrics database: %w", err)
	}
	defer dbm.Close()

	dbMetrics, err := sqlite.NewMetricsDatabase(dbm, log.Named("sqlite.metrics"), time.Second, time.Second)
	if err != nil {
		return fmt.Errorf("failed to create SQLite metrics database: %w", err)
	}

	am := alerts.NewManager()
	store, err := stores.NewSQLStore(stores.Config{
		Alerts:                        alerts.WithOrigin(am, "bus"),
		DB:                            dbMain,
		DBMetrics:                     dbMetrics,
		PartialSlabDir:                filepath.Join(dir, "partial_slabs"),
		Migrate:                       true,
		SlabBufferCompletionThreshold: 1 << 12,
		Logger:                        log.Named("store"),
		WalletAddress:                 types.StandardUnlockHash(pk),
		LongQueryDuration:             time.Second,
		LongTxDuration:                time.Second,
	})
	if err != nil {
		return fmt.Errorf("failed to create store: %w", err)
	}
	defer store.Close()

	wh, err := webhooks.NewManager(store, log.Named("webhooks"))
	if err != nil {
		return fmt.Errorf("failed to create webhook manager: %w", err)
	}

	am.RegisterWebhookBroadcaster(wh)

	var workerHandler, busHandler, autopilotHandler http.Handler
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "*")
			w.Header().Set("Access-Control-Allow-Headers", "*")
			w.Header().Set("Access-Control-Expose-Headers", "*")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			} else if strings.HasPrefix(r.URL.Path, "/api/worker") {
				r.URL.Path = strings.TrimPrefix(r.URL.Path, "/api/worker")
				workerHandler.ServeHTTP(w, r)
				return
			} else if strings.HasPrefix(r.URL.Path, "/api/bus") {
				r.URL.Path = strings.TrimPrefix(r.URL.Path, "/api/bus")
				busHandler.ServeHTTP(w, r)
				return
			} else if strings.HasPrefix(r.URL.Path, "/api/autopilot") {
				r.URL.Path = strings.TrimPrefix(r.URL.Path, "/api/autopilot")
				autopilotHandler.ServeHTTP(w, r)
				return
			}
			http.NotFound(w, r)
		}),
		ReadTimeout: 15 * time.Second,
	}
	defer server.Close()
	go server.Serve(apiListener)

	wm, err := wallet.NewSingleAddressWallet(sk, cm, store)
	if err != nil {
		return fmt.Errorf("failed to create wallet: %w", err)
	}
	defer wm.Close()

	explorerURL := "https://api.siascan.com/exchange-rate/siacoin"
	b, err := bus.New(ctx, config.Bus{
		AllowPrivateIPs:               true,
		AnnouncementMaxAgeHours:       90 * 24,
		Bootstrap:                     true,
		GatewayAddr:                   s.Addr(),
		UsedUTXOExpiry:                time.Hour,
		SlabBufferCompletionThreshold: 1 << 12,
	}, ([32]byte)(sk[:32]), am, wh, cm, s, wm, store, explorerURL, log.Named("bus"))
	if err != nil {
		return fmt.Errorf("failed to create bus: %w", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		if err := b.Shutdown(ctx); err != nil {
			log.Error("failed to shutdown bus", zap.Error(err))
		}
	}()
	busHandler = jape.BasicAuth("sia is cool")(b.Handler())

	apiAddr := apiListener.Addr().String()
	busClient := bus.NewClient(fmt.Sprintf("http://%s/api/bus", apiAddr), "sia is cool")
	workerClient := worker.NewClient(fmt.Sprintf("http://%s/api/worker", apiAddr), "sia is cool")
	autopilotClient := autopilot.NewClient(fmt.Sprintf("http://%s/api/autopilot", apiAddr), "sia is cool")

	w, err := worker.New(config.Worker{
		AccountsRefillInterval:   time.Second,
		ID:                       "worker",
		BusFlushInterval:         100 * time.Millisecond,
		DownloadOverdriveTimeout: 500 * time.Millisecond,
		UploadOverdriveTimeout:   500 * time.Millisecond,
		DownloadMaxMemory:        1 << 28, // 256 MiB
		UploadMaxMemory:          1 << 28, // 256 MiB
		UploadMaxOverdrive:       5,
	}, ([32]byte)(sk[:32]), busClient, log.Named("worker"))
	if err != nil {
		return fmt.Errorf("failed to create worker: %w", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		if err := w.Shutdown(ctx); err != nil {
			log.Error("failed to shutdown worker", zap.Error(err))
		}
	}()
	workerHandler = jape.BasicAuth("sia is cool")(w.Handler())
	if _, err := workerClient.Account(context.Background(), types.PublicKey{}); err != nil {
		panic(err)
	}

	ap, err := autopilot.New(config.Autopilot{
		Heartbeat:                      time.Second,
		ID:                             api.DefaultAutopilotID,
		MigrationHealthCutoff:          0.99,
		MigratorParallelSlabsPerWorker: 1,
		RevisionSubmissionBuffer:       0,
		ScannerInterval:                time.Second,
		ScannerBatchSize:               10,
		ScannerNumThreads:              1,
	}, busClient, []autopilot.Worker{workerClient}, log.Named("autopilot"))
	if err != nil {
		return fmt.Errorf("failed to create autopilot: %w", err)
	}
	defer ap.Shutdown(ctx)
	go ap.Run()
	autopilotHandler = jape.BasicAuth("sia is cool")(ap.Handler())

	log.Info("node started", zap.Stringer("http", apiListener.Addr()))
	node.APIAddress = "http://" + apiListener.Addr().String()
	node.Password = "sia is cool"

	err = autopilotClient.UpdateConfig(api.AutopilotConfig{
		Contracts: api.ContractsConfig{
			Set:         "autopilot",
			Amount:      1000,
			Period:      4320,
			RenewWindow: 144 * 7,
			Download:    1 << 30,
			Upload:      1 << 30,
			Storage:     1 << 30,
			Prune:       false,
		},
		Hosts: api.HostsConfig{
			AllowRedundantIPs:          true,
			MaxDowntimeHours:           1440,
			MinProtocolVersion:         "1.6.0",
			MaxConsecutiveScanFailures: 100,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to update autopilot config: %w", err)
	}

	// Finish worker setup.
	if err := w.Setup(ctx, node.APIAddress+"/api/worker", "sia is cool"); err != nil {
		return fmt.Errorf("failed to setup worker: %w", err)
	}

	err = busClient.UpdateAutopilot(ctx, api.Autopilot{
		ID: api.DefaultAutopilotID,
		Config: api.AutopilotConfig{
			Contracts: api.ContractsConfig{
				Set:         "autopilot",
				Amount:      1000,
				Period:      4320,
				RenewWindow: 144 * 7,
				Download:    1 << 30,
				Upload:      1 << 30,
				Storage:     1 << 30,
				Prune:       false,
			},
			Hosts: api.HostsConfig{
				AllowRedundantIPs:          true,
				MaxDowntimeHours:           1440,
				MinProtocolVersion:         "1.6.0",
				MaxConsecutiveScanFailures: 100,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to update autopilot: %w", err)
	}

	err = busClient.UpdateGougingSettings(ctx, api.GougingSettings{
		MaxRPCPrice:      types.Siacoins(1).Div64(1000),        // 1mS per RPC
		MaxContractPrice: types.Siacoins(10),                   // 10 SC per contract
		MaxDownloadPrice: types.Siacoins(1).Mul64(1000),        // 1000 SC per 1 TiB
		MaxUploadPrice:   types.Siacoins(1).Mul64(1000),        // 1000 SC per 1 TiB
		MaxStoragePrice:  types.Siacoins(1000).Div64(144 * 30), // 1000 SC per month

		HostBlockHeightLeeway: 240, // amount of leeway given to host block height

		MinPriceTableValidity:         10 * time.Second,  // minimum value for price table validity
		MinAccountExpiry:              time.Hour,         // minimum value for account expiry
		MinMaxEphemeralAccountBalance: types.Siacoins(1), // 1SC
	})
	if err != nil {
		return fmt.Errorf("failed to update setting: %w", err)
	}
	if err != nil {
		return fmt.Errorf("failed to update setting: %w", err)
	}
	err = busClient.UpdatePinnedSettings(ctx, api.PinnedSettings{
		Currency:  "usd",
		Threshold: 0.05,
	})
	if err != nil {
		return fmt.Errorf("failed to update setting: %w", err)
	}
	err = busClient.UpdateUploadSettings(ctx, api.UploadSettings{
		DefaultContractSet: "autopilot",
		Redundancy: api.RedundancySettings{
			MinShards:   2,
			TotalShards: 3,
		},
		Packing: api.UploadPackingSettings{
			Enabled:               true,
			SlabBufferMaxSizeSoft: 1 << 20,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to update setting: %w", err)
	}
	err = busClient.UpdateS3Settings(ctx, api.S3Settings{
		Authentication: api.S3AuthenticationSettings{
			V4Keypairs: map[string]string{
				"TESTINGYNHUWCPKOPSYQ": "Rh30BNyj+qNI4ftYRteoZbHJ3X4Ln71QtZkRXzJ9",
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to update setting: %w", err)
	}

	waitForSync := func() error {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
				state, err := busClient.ConsensusState(ctx)
				if err != nil {
					return fmt.Errorf("failed to get consensus state: %w", err)
				} else if state.BlockHeight == m.chain.Tip().Height {
					return nil
				}
			}
		}
	}

	if err := waitForSync(); err != nil {
		return fmt.Errorf("failed to wait for sync: %w", err)
	}

	// mine blocks to fund the wallet
	walletAddress := types.StandardUnlockHash(sk.PublicKey())
	if err := m.MineBlocks(ctx, int(network.MaturityDelay)+20, walletAddress); err != nil {
		return fmt.Errorf("failed to mine blocks: %w", err)
	}

	if err := waitForSync(); err != nil {
		return fmt.Errorf("failed to wait for sync: %w", err)
	}

	if _, err := autopilotClient.Trigger(true); err != nil {
		return fmt.Errorf("failed to trigger autopilot: %w", err)
	}
	m.addNodeAndWait(ctx, node, ready)
	return nil
}
