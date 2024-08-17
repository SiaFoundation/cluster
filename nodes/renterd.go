package nodes

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/minio/blake2b-simd"
	"go.sia.tech/core/types"
	"go.sia.tech/coreutils/chain"
	"go.sia.tech/coreutils/syncer"
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

func Renterd(ctx context.Context, baseDir string, pk types.PrivateKey, cm *chain.Manager, s *syncer.Syncer, nm *Manager, log *zap.Logger) error {
	node := Node{
		ID:            NodeID(frand.Bytes(8)),
		Type:          NodeTypeRenterd,
		WalletAddress: types.StandardUnlockHash(pk.PublicKey()),
	}
	log = log.Named(node.ID.String())
	am := alerts.NewManager()

	apiListener, err := net.Listen("tcp", ":0")
	if err != nil {
		return fmt.Errorf("failed to listen on http address: %w", err)
	}
	defer apiListener.Close()

	dir, err := createNodeDir(baseDir, node.ID)
	if err != nil {
		return fmt.Errorf("failed to create node directory: %w", err)
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
		Handler: jape.BasicAuth("sia is cool")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/worker") {
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
		})),
		ReadTimeout: 15 * time.Second,
	}
	defer s.Close()
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
	busHandler = b.Handler()

	apiAddr := apiListener.Addr().String()
	busClient := bus.NewClient(fmt.Sprintf("http://%s/api/bus", apiAddr), "sia is cool")
	workerClient := worker.NewClient(fmt.Sprintf("http://%s/api/worker", apiAddr), "sia is cool")

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
	workerHandler = w.Handler()

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
	autopilotHandler = ap.Handler()

	log.Info("node started", zap.Stringer("http", apiListener.Addr()))
	node.APIAddress = "http://" + apiListener.Addr().String()
	node.Password = "sia is cool"
	nm.Put(node)
	<-ctx.Done()
	nm.Delete(node.ID)
	log.Info("shutting down")
	return nil
}
