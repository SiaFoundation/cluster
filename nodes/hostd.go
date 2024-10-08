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
)

// StartHostd starts a new hostd node. It listens on random ports and registers
// itself with the Manager. This function blocks until the context is
// canceled. All restources will be cleaned up before the funcion returns.
func (m *Manager) StartHostd(ctx context.Context, sk types.PrivateKey, ready chan<- struct{}) (err error) {
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
		Type:          NodeTypeHostd,
		WalletAddress: types.StandardUnlockHash(pk),
	}
	log := m.log.Named("hostd." + node.ID.String())

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

	syncerListener, err := net.Listen("tcp", ":0")
	if err != nil {
		return fmt.Errorf("failed to listen on syncer address: %w", err)
	}
	defer syncerListener.Close()

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

	var cm *chain.Manager
	var s *syncer.Syncer

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
	cm = chain.NewManager(dbstore, tipState)

	// start a syncer
	_, port, err := net.SplitHostPort(syncerListener.Addr().String())
	if err != nil {
		return fmt.Errorf("failed to split syncer address: %w", err)
	}
	s = syncer.New(syncerListener, cm, testutil.NewMemPeerStore(), gateway.Header{
		GenesisID:  genesisIndex.ID,
		UniqueID:   gateway.GenerateUniqueID(),
		NetAddress: "127.0.0.1:" + port,
	}, syncer.WithLogger(log.Named("syncer")), syncer.WithPeerDiscoveryInterval(5*time.Second), syncer.WithSyncInterval(5*time.Second), syncer.WithMaxInboundPeers(10000), syncer.WithMaxOutboundPeers(10000))
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
			log.Debug("failed to connect to node", zap.String("node", n.ID.String()), zap.Error(err))
		}
	}

	store, err := sqlite.OpenDatabase(filepath.Join(dir, "hostd.sqlite3"), log.Named("sqlite3"))
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer store.Close()

	wm, err := wallet.NewSingleAddressWallet(sk, cm, store, wallet.WithLogger(log.Named("wallet")), wallet.WithReservationDuration(3*time.Hour))
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

	cfm, err := settings.NewConfigManager(sk, store, cm, s, wm, settings.WithAlertManager(am), settings.WithLog(log.Named("settings")), settings.WithValidateNetAddress(false), settings.WithAnnounceInterval(144*30))
	if err != nil {
		return fmt.Errorf("failed to create settings manager: %w", err)
	}
	defer cfm.Close()

	// set the host to accepting contracts
	settings := cfm.Settings()
	settings.AcceptingContracts = true
	settings.StoragePrice = types.Siacoins(1).Div64(4320).Div64(1e9) // 1 SC / TB / Month
	settings.CollateralMultiplier = 2
	settings.MaxCollateral = types.Siacoins(100000)
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
	if _, err := vm.AddVolume(ctx, filepath.Join(dir, "data.dat"), 256, ch); err != nil {
		return fmt.Errorf("failed to add volume: %w", err)
	} else if err := <-ch; err != nil { // wait for the volume to be initialized
		return fmt.Errorf("failed to add volume: %w", err)
	}

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

	rhp2, err := rhp2.NewSessionHandler(rhp2Listener, sk, rhp3Listener.Addr().String(), cm, s, wm, contractManager, cfm, vm, rhp2.WithDataMonitor(dr), rhp2.WithLog(log.Named("rhp2")))
	if err != nil {
		return fmt.Errorf("failed to create rhp2 session handler: %w", err)
	}
	go rhp2.Serve()
	defer rhp2.Close()

	registry := registry.NewManager(sk, store, log.Named("registry"))
	accounts := accounts.NewManager(store, cfm)
	rhp3, err := rhp3.NewSessionHandler(rhp3Listener, sk, cm, s, wm, accounts, contractManager, registry, vm, cfm, rhp3.WithDataMonitor(dr), rhp3.WithSessionReporter(sr), rhp3.WithLog(log.Named("rhp3")))
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

	a := jape.BasicAuth("sia is cool")(api.NewServer("", sk.PublicKey(), cm, s, accounts, contractManager, vm, wm, store, cfm, index, api.WithAlerts(am),
		api.WithLogger(log.Named("api")),
		api.WithRHPSessionReporter(sr),
		api.WithWebhooks(wr),
		api.WithPinnedSettings(pm),
		api.WithExplorer(ex)))
	server := http.Server{
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
				a.ServeHTTP(w, r)
				return
			}
			http.NotFound(w, r)
		}),
		ReadTimeout: 30 * time.Second,
	}
	defer server.Close()
	go server.Serve(httpListener)

	log.Info("node started", zap.String("network", cm.TipState().Network.Name), zap.String("hostKey", sk.PublicKey().String()), zap.String("http", httpListener.Addr().String()), zap.String("p2p", string(s.Addr())), zap.String("rhp2", rhp2.LocalAddr()), zap.String("rhp3", rhp3.LocalAddr()))
	node.APIAddress = "http://" + httpListener.Addr().String()
	node.Password = "sia is cool"

	// mine blocks to fund the wallet
	walletAddress := types.StandardUnlockHash(sk.PublicKey())
	if err := m.MineBlocks(ctx, 200, walletAddress); err != nil {
		return fmt.Errorf("failed to mine blocks: %w", err)
	}

	// wait for sync
	for m.chain.Tip() != index.Tip() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}

	client := api.NewClient(node.APIAddress+"/api", node.Password)
	if err := client.Announce(); err != nil {
		return fmt.Errorf("failed to announce: %w", err)
	}

	// mine another block to confirm the announcement
	if err := m.MineBlocks(ctx, 1, walletAddress); err != nil {
		return fmt.Errorf("failed to mine blocks: %w", err)
	}

	m.addNodeAndWait(ctx, node, ready)
	return nil
}
