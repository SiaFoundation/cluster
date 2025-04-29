package nodes

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.sia.tech/cluster/internal/mock"
	"go.sia.tech/core/gateway"
	"go.sia.tech/core/types"
	"go.sia.tech/coreutils"
	"go.sia.tech/coreutils/chain"
	rhp4 "go.sia.tech/coreutils/rhp/v4"
	"go.sia.tech/coreutils/rhp/v4/quic"
	"go.sia.tech/coreutils/rhp/v4/siamux"
	"go.sia.tech/coreutils/syncer"
	"go.sia.tech/coreutils/testutil"
	"go.sia.tech/coreutils/wallet"
	"go.sia.tech/hostd/v2/alerts"
	"go.sia.tech/hostd/v2/api"
	"go.sia.tech/hostd/v2/certificates"
	"go.sia.tech/hostd/v2/explorer"
	"go.sia.tech/hostd/v2/host/accounts"
	"go.sia.tech/hostd/v2/host/contracts"
	"go.sia.tech/hostd/v2/host/registry"
	"go.sia.tech/hostd/v2/host/settings"
	"go.sia.tech/hostd/v2/host/settings/pin"
	"go.sia.tech/hostd/v2/host/storage"
	"go.sia.tech/hostd/v2/index"
	"go.sia.tech/hostd/v2/persist/sqlite"
	"go.sia.tech/hostd/v2/rhp"
	rhp2 "go.sia.tech/hostd/v2/rhp/v2"
	rhp3 "go.sia.tech/hostd/v2/rhp/v3"
	"go.sia.tech/jape"
	"go.uber.org/zap"
)

func parseListenerPort(addr string) (uint16, error) {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, fmt.Errorf("failed to split address: %w", err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("failed to parse port: %w", err)
	}
	return uint16(port), nil
}

func mockOrDefault[T any](original, mock T, shared bool) T {
	if shared {
		return mock
	}
	return original
}

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
		dbstore, tipState, err := chain.NewDBStore(bdb, network, genesis, nil)
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

	am := alerts.NewManager(alerts.WithLog(log.Named("alerts")), alerts.WithEventReporter(mock.NewEventReporter()))

	vm, err := storage.NewVolumeManager(store, storage.WithLogger(log.Named("volumes")), storage.WithAlerter(am))
	if err != nil {
		return fmt.Errorf("failed to create storage manager: %w", err)
	}
	defer vm.Close()

	rhp2Listener, err := rhp.Listen("tcp", ":0")
	if err != nil {
		return fmt.Errorf("failed to listen on rhp2 addr: %w", err)
	}
	defer rhp2Listener.Close()

	rhp2Port, err := parseListenerPort(rhp2Listener.Addr().String())
	if err != nil {
		return fmt.Errorf("failed to parse rhp2 port: %w", err)
	}

	rhp3Listener, err := rhp.Listen("tcp", ":0")
	if err != nil {
		return fmt.Errorf("failed to listen on rhp3 addr: %w", err)
	}
	defer rhp3Listener.Close()

	rhp3Port, err := parseListenerPort(rhp3Listener.Addr().String())
	if err != nil {
		return fmt.Errorf("failed to parse rhp3 port: %w", err)
	}

	rhp4Listener, err := rhp.Listen("tcp", ":0")
	if err != nil {
		return fmt.Errorf("failed to listen on rhp4 addr: %w", err)
	}
	defer rhp4Listener.Close()

	rhp4Port, err := parseListenerPort(rhp4Listener.Addr().String())
	if err != nil {
		return fmt.Errorf("failed to parse rhp4 port: %w", err)
	}

	rhp4UDPAddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort("localhost", strconv.Itoa(int(rhp4Port))))
	if err != nil {
		return fmt.Errorf("failed to resolve udp address: %w", err)
	}

	rhp4UDPListener, err := net.ListenUDP("udp", rhp4UDPAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on udp addr: %w", err)
	}
	defer rhp4UDPListener.Close()

	certs, err := certificates.NewManager(dir, sk)
	if err != nil {
		return fmt.Errorf("failed to create certificates manager: %w", err)
	}
	defer certs.Close()

	rhp4QUICListener, err := quic.Listen(rhp4UDPListener, certs)
	if err != nil {
		return fmt.Errorf("failed to listen on quic addr: %w", err)
	}
	defer rhp4QUICListener.Close()

	cfm, err := settings.NewConfigManager(sk, store, cm, mockOrDefault[settings.Syncer](s, mock.NewSyncer(), m.shareConsensus), vm, wm, settings.WithAlertManager(am), settings.WithLog(log.Named("settings")),
		settings.WithValidateNetAddress(false),
		settings.WithAnnounceInterval(144*30),
		settings.WithRHP2Port(rhp2Port),
		settings.WithRHP3Port(rhp3Port),
		settings.WithRHP4Port(rhp4Port))
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
	settings.NetAddress = "localhost"
	if err := cfm.UpdateSettings(settings); err != nil {
		return fmt.Errorf("failed to update settings: %w", err)
	}

	ch := make(chan error, 1)
	if _, err := vm.AddVolume(ctx, filepath.Join(dir, "data.dat"), 256, ch); err != nil {
		return fmt.Errorf("failed to add volume: %w", err)
	} else if err := <-ch; err != nil { // wait for the volume to be initialized
		return fmt.Errorf("failed to add volume: %w", err)
	}

	contractManager, err := contracts.NewManager(store, vm, cm,
		mockOrDefault[contracts.Syncer](s, mock.NewSyncer(), m.shareConsensus),
		wm, contracts.WithLog(log.Named("contracts")), contracts.WithAlerter(am), contracts.WithRevisionSubmissionBuffer(10))
	if err != nil {
		return fmt.Errorf("failed to create contracts manager: %w", err)
	}
	defer contractManager.Close()

	index, err := index.NewManager(store, cm, contractManager, wm, cfm, vm, index.WithLog(log.Named("index")))
	if err != nil {
		return fmt.Errorf("failed to create index manager: %w", err)
	}
	defer index.Close()

	rhp2 := rhp2.NewSessionHandler(rhp2Listener, sk, cm, mockOrDefault[rhp2.Syncer](s, mock.NewSyncer(), m.shareConsensus), wm, contractManager, cfm, vm, log.Named("rhp2"))
	go rhp2.Serve()
	defer rhp2.Close()

	registry := registry.NewManager(sk, store, log.Named("registry"))
	accounts := accounts.NewManager(store, cfm)

	rhp3 := rhp3.NewSessionHandler(rhp3Listener, sk, cm, mockOrDefault[rhp3.Syncer](s, mock.NewSyncer(), m.shareConsensus), wm, accounts, contractManager, registry, vm, cfm, log.Named("rhp3"))
	go rhp3.Serve()
	defer rhp3.Close()

	rhp4 := rhp4.NewServer(sk, cm, mockOrDefault[rhp4.Syncer](s, mock.NewSyncer(), m.shareConsensus), contractManager, wm, cfm, vm, rhp4.WithPriceTableValidity(10*time.Minute))
	go siamux.Serve(rhp4Listener, rhp4, log.Named("rhp4.siamux"))
	go quic.Serve(rhp4QUICListener, rhp4, log.Named("rhp4.quic"))

	ex := explorer.New("https://api.siascan.com")
	pm, err := pin.NewManager(store, cfm, ex, pin.WithLogger(log.Named("pin")))
	if err != nil {
		return fmt.Errorf("failed to create pin manager: %w", err)
	}

	a := jape.BasicAuth("sia is cool")(api.NewServer("", sk.PublicKey(), cm, mockOrDefault[api.Syncer](s, mock.NewSyncer(), m.shareConsensus), accounts, contractManager, vm, wm, store, cfm, index, api.WithAlerts(am),
		api.WithLogger(log.Named("api")),
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

	node.APIAddress = "http://" + httpListener.Addr().String()
	node.Password = "sia is cool"

	waitForSync := func() error {
		// wait for sync
		for m.chain.Tip() != index.Tip() {
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

	log.Debug("node setup complete")

	// mine blocks to fund the wallet
	walletAddress := types.StandardUnlockHash(sk.PublicKey())
	if err := m.MineBlocks(ctx, int(network.MaturityDelay)+20, walletAddress); err != nil {
		return fmt.Errorf("failed to mine blocks: %w", err)
	}

	if err := waitForSync(); err != nil {
		return fmt.Errorf("failed to wait for sync: %w", err)
	}

	client := api.NewClient(node.APIAddress+"/api", node.Password)
	if err := client.Announce(); err != nil {
		return fmt.Errorf("failed to announce: %w", err)
	}

	// mine another block to confirm the announcement
	if err := m.MineBlocks(ctx, 1, walletAddress); err != nil {
		return fmt.Errorf("failed to mine blocks: %w", err)
	}

	log.Info("node started", zap.String("network", cm.TipState().Network.Name), zap.String("hostKey", sk.PublicKey().String()),
		zap.String("http", httpListener.Addr().String()),
		zap.String("p2p", string(s.Addr())),
		zap.String("rhp2", rhp2.LocalAddr()),
		zap.String("rhp3", rhp3.LocalAddr()),
		zap.String("rhp4", rhp4Listener.Addr().String()),
		zap.String("quic", rhp4QUICListener.Addr().String()))
	m.addNodeAndWait(ctx, node, ready)
	return nil
}
