package nodes

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"go.sia.tech/core/types"
	"go.sia.tech/coreutils/chain"
	"go.sia.tech/coreutils/syncer"
	"go.sia.tech/coreutils/wallet"
	"go.sia.tech/hostd/alerts"
	"go.sia.tech/hostd/api"
	"go.sia.tech/hostd/explorer"
	"go.sia.tech/hostd/host/accounts"
	"go.sia.tech/hostd/host/contracts"
	"go.sia.tech/hostd/host/registry"
	"go.sia.tech/hostd/host/settings"
	"go.sia.tech/hostd/host/settings/pin"
	"go.sia.tech/hostd/host/storage"
	"go.sia.tech/hostd/index"
	"go.sia.tech/hostd/persist/sqlite"
	"go.sia.tech/hostd/rhp"
	rhp2 "go.sia.tech/hostd/rhp/v2"
	rhp3 "go.sia.tech/hostd/rhp/v3"
	"go.sia.tech/hostd/webhooks"
	"go.sia.tech/jape"
	"go.uber.org/zap"
	"lukechampine.com/frand"
)

// Hostd starts a new hostd node. It listens on random ports and registers
// itself with the provided Manager. This function blocks until the context is
// canceled. All resources will be cleaned up before the function returns.
func Hostd(ctx context.Context, baseDir string, pk types.PrivateKey, cm *chain.Manager, s *syncer.Syncer, nm *Manager, log *zap.Logger) error {
	node := Node{
		ID:            NodeID(frand.Bytes(8)),
		Type:          NodeTypeHostd,
		WalletAddress: types.StandardUnlockHash(pk.PublicKey()),
	}
	log = log.Named(node.ID.String())

	dir, err := createNodeDir(baseDir, node.ID)
	if err != nil {
		return fmt.Errorf("failed to create node directory: %w", err)
	}

	store, err := sqlite.OpenDatabase(filepath.Join(dir, "hostd.sqlite3"), log.Named("sqlite3"))
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer store.Close()

	httpListener, err := net.Listen("tcp", ":0")
	if err != nil {
		return fmt.Errorf("failed to listen on http address: %w", err)
	}
	defer httpListener.Close()

	rhp2Listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return fmt.Errorf("failed to listen on rhp2 addr: %w", err)
	}
	defer rhp2Listener.Close()

	rhp3Listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return fmt.Errorf("failed to listen on rhp3 addr: %w", err)
	}
	defer rhp3Listener.Close()

	wm, err := wallet.NewSingleAddressWallet(pk, cm, store, wallet.WithLogger(log.Named("wallet")), wallet.WithReservationDuration(3*time.Hour))
	if err != nil {
		return fmt.Errorf("failed to create wallet: %w", err)
	}
	defer wm.Close()

	wr, err := webhooks.NewManager(store, log.Named("webhooks"))
	if err != nil {
		return fmt.Errorf("failed to create webhook reporter: %w", err)
	}
	defer wr.Close()
	sr := rhp.NewSessionReporter()

	am := alerts.NewManager(alerts.WithEventReporter(wr), alerts.WithLog(log.Named("alerts")))

	cfm, err := settings.NewConfigManager(pk, store, cm, s, wm, settings.WithAlertManager(am), settings.WithLog(log.Named("settings")), settings.WithValidateNetAddress(false), settings.WithAnnounceInterval(10))
	if err != nil {
		return fmt.Errorf("failed to create settings manager: %w", err)
	}
	defer cfm.Close()

	// set the host to accepting contracts
	settings := cfm.Settings()
	settings.AcceptingContracts = true
	settings.NetAddress = rhp2Listener.Addr().String()
	if err := cfm.UpdateSettings(settings); err != nil {
		return fmt.Errorf("failed to update settings: %w", err)
	}

	vm, err := storage.NewVolumeManager(store, storage.WithLogger(log.Named("volumes")), storage.WithAlerter(am))
	if err != nil {
		return fmt.Errorf("failed to create storage manager: %w", err)
	}
	defer vm.Close()

	ch := make(chan error, 1)
	if _, err := vm.AddVolume(ctx, filepath.Join(dir, "data.dat"), 64, ch); err != nil {
		return fmt.Errorf("failed to add volume: %w", err)
	}
	go func() {
		if err := <-ch; err != nil {
			log.Error("failed to add volume", zap.Error(err))
		}
	}()

	contractManager, err := contracts.NewManager(store, vm, cm, s, wm, contracts.WithLog(log.Named("contracts")), contracts.WithAlerter(am))
	if err != nil {
		return fmt.Errorf("failed to create contracts manager: %w", err)
	}
	defer contractManager.Close()

	index, err := index.NewManager(store, cm, contractManager, wm, cfm, vm, index.WithLog(log.Named("index")))
	if err != nil {
		return fmt.Errorf("failed to create index manager: %w", err)
	}
	defer index.Close()

	dr := rhp.NewDataRecorder(store, log.Named("data"))

	rhp2, err := rhp2.NewSessionHandler(rhp2Listener, pk, rhp3Listener.Addr().String(), cm, s, wm, contractManager, cfm, vm, rhp2.WithDataMonitor(dr), rhp2.WithLog(log.Named("rhp2")))
	if err != nil {
		return fmt.Errorf("failed to create rhp2 session handler: %w", err)
	}
	go rhp2.Serve()
	defer rhp2.Close()

	registry := registry.NewManager(pk, store, log.Named("registry"))
	accounts := accounts.NewManager(store, cfm)
	rhp3, err := rhp3.NewSessionHandler(rhp3Listener, pk, cm, s, wm, accounts, contractManager, registry, vm, cfm, rhp3.WithDataMonitor(dr), rhp3.WithSessionReporter(sr), rhp3.WithLog(log.Named("rhp3")))
	if err != nil {
		return fmt.Errorf("failed to create rhp3 session handler: %w", err)
	}
	go rhp3.Serve()
	defer rhp3.Close()

	ex := explorer.New("https://api.siascan.com")
	pm, err := pin.NewManager(store, cfm, ex, pin.WithLogger(log.Named("pin")))
	if err != nil {
		return fmt.Errorf("failed to create pin manager: %w", err)
	}

	api := jape.BasicAuth("sia is cool")(api.NewServer("", pk.PublicKey(), cm, s, accounts, contractManager, vm, wm, store, cfm, index, api.ServerWithAlerts(am),
		api.ServerWithLogger(log.Named("api")),
		api.ServerWithRHPSessionReporter(sr),
		api.ServerWithWebhooks(wr),
		api.ServerWithPinnedSettings(pm),
		api.ServerWithExplorer(ex)))

	server := http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api") {
				r.URL.Path = strings.TrimPrefix(r.URL.Path, "/api")
				api.ServeHTTP(w, r)
				return
			}
			http.NotFound(w, r)
		}),
		ReadTimeout: 30 * time.Second,
	}
	defer server.Close()
	go server.Serve(httpListener)

	log.Info("node started", zap.String("network", cm.TipState().Network.Name), zap.String("hostKey", pk.PublicKey().String()), zap.String("http", httpListener.Addr().String()), zap.String("p2p", string(s.Addr())), zap.String("rhp2", rhp2.LocalAddr()), zap.String("rhp3", rhp3.LocalAddr()))
	node.APIAddress = "http://" + httpListener.Addr().String()
	node.Password = "sia is cool"
	nm.Put(node)
	<-ctx.Done()
	nm.Delete(node.ID)
	log.Info("shutting down")
	return nil
}
