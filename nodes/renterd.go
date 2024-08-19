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

	"github.com/minio/blake2b-simd"
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
	"lukechampine.com/frand"
)

// StartRenterd starts a new renterd node and adds it to the manager.
// This function blocks until the context is canceled. All resources will be
// cleaned up before the function returns.
func (m *Manager) StartRenterd(ctx context.Context, ready chan<- struct{}) error {
	pk := types.GeneratePrivateKey()
	node := Node{
		ID:            NodeID(frand.Bytes(8)),
		Type:          NodeTypeRenterd,
		WalletAddress: types.StandardUnlockHash(pk.PublicKey()),
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
	})
	defer s.Close()
	go s.Run(ctx)
	// connect to the primary cluster syncer
	err = func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_, err := m.syncer.Connect(ctx, s.Addr())
		return err
	}(ctx)
	if err != nil {
		return fmt.Errorf("failed to connect to cluster syncer: %w", err)
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
		RetryTransactionIntervals: []time.Duration{
			200 * time.Millisecond,
			500 * time.Millisecond,
			time.Second,
			3 * time.Second,
			10 * time.Second,
			10 * time.Second,
		},
		WalletAddress:     types.StandardUnlockHash(pk.PublicKey()),
		LongQueryDuration: time.Second,
		LongTxDuration:    time.Second,
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

	wm, err := wallet.NewSingleAddressWallet(pk, cm, store)
	if err != nil {
		return fmt.Errorf("failed to create wallet: %w", err)
	}
	defer wm.Close()

	b, err := bus.New(ctx, am, wh, cm, s, wm, store, 24*time.Hour, log.Named("bus"))
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

	workerKey := blake2b.Sum256(append([]byte("worker"), pk...))
	w, err := worker.New(config.Worker{
		ContractLockTimeout:      5 * time.Second,
		ID:                       "worker",
		BusFlushInterval:         100 * time.Millisecond,
		DownloadOverdriveTimeout: 500 * time.Millisecond,
		UploadOverdriveTimeout:   500 * time.Millisecond,
		DownloadMaxMemory:        1 << 28, // 256 MiB
		UploadMaxMemory:          1 << 28, // 256 MiB
		UploadMaxOverdrive:       5,
	}, workerKey, busClient, log.Named("worker"))
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

	ap, err := autopilot.New(config.Autopilot{
		AccountsRefillInterval:         time.Second,
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
	autopilotHandler = jape.BasicAuth("sia is cool")(ap.Handler())

	log.Info("node started", zap.Stringer("http", apiListener.Addr()))
	node.APIAddress = "http://" + apiListener.Addr().String()
	node.Password = "sia is cool"

	err = autopilotClient.UpdateConfig(api.AutopilotConfig{
		Contracts: api.ContractsConfig{
			Set:         "autopilot",
			Amount:      1000,
			Allowance:   types.Siacoins(1000000),
			Period:      4320,
			RenewWindow: 144 * 7,
			Download:    1 << 30,
			Upload:      1 << 30,
			Storage:     1 << 30,
			Prune:       false,
		},
		Hosts: api.HostsConfig{
			AllowRedundantIPs:     true,
			MaxDowntimeHours:      1440,
			MinProtocolVersion:    "1.6.0",
			MinRecentScanFailures: 100,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to update autopilot config: %w", err)
	}

	if _, err := autopilotClient.Trigger(true); err != nil {
		return fmt.Errorf("failed to trigger autopilot: %w", err)
	}

	// mine blocks to fund the wallet
	walletAddress := types.StandardUnlockHash(pk.PublicKey())
	if err := m.MineBlocks(ctx, 200, walletAddress); err != nil {
		return fmt.Errorf("failed to mine blocks: %w", err)
	}
	m.Put(node)
	if ready != nil {
		ready <- struct{}{}
	}
	<-ctx.Done()
	m.Delete(node.ID)
	log.Info("shutting down")
	return nil
}
