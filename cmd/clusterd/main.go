package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"go.sia.tech/cluster/api"
	"go.sia.tech/cluster/nodes"
	"go.sia.tech/core/gateway"
	"go.sia.tech/core/types"
	"go.sia.tech/coreutils"
	"go.sia.tech/coreutils/chain"
	"go.sia.tech/coreutils/syncer"
	"go.sia.tech/coreutils/testutil"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func main() {
	var (
		dir         string
		apiAddr     string
		apiPassword string
		logLevel    string
		network     string

		siafundAddr string

		renterdCount  int
		hostdCount    int
		walletdCount  int
		exploredCount int
	)

	flag.StringVar(&dir, "dir", "", "directory to store renter data")
	flag.StringVar(&apiAddr, "api", ":3001", "API address")
	flag.StringVar(&apiPassword, "api.password", "", "API password")
	flag.StringVar(&logLevel, "log", "info", "logging level")
	flag.StringVar(&network, "network", "v1", "network to use (v1 or v2)")
	flag.StringVar(&siafundAddr, "siafund", "", "address to send siafunds to")

	flag.IntVar(&renterdCount, "renterd", 0, "number of renter daemons to run")
	flag.IntVar(&hostdCount, "hostd", 0, "number of host daemons to run")
	flag.IntVar(&walletdCount, "walletd", 0, "number of wallet daemons to run")
	flag.IntVar(&exploredCount, "explored", 0, "number of explorer daemons to run")
	flag.Parse()

	cfg := zap.NewProductionEncoderConfig()
	cfg.TimeKey = "" // prevent duplicate timestamps
	cfg.EncodeTime = zapcore.RFC3339TimeEncoder
	cfg.EncodeDuration = zapcore.StringDurationEncoder
	cfg.EncodeLevel = zapcore.CapitalColorLevelEncoder

	cfg.StacktraceKey = ""
	cfg.CallerKey = ""
	encoder := zapcore.NewConsoleEncoder(cfg)

	level, err := zap.ParseAtomicLevel(logLevel)
	if err != nil {
		fmt.Printf("invalid log level %q", level)
		os.Exit(1)
	}

	log := zap.New(zapcore.NewCore(encoder, zapcore.Lock(os.Stdout), level))
	defer log.Sync()

	zap.RedirectStdLog(log)

	if hostdCount == 0 && renterdCount == 0 && walletdCount == 0 && exploredCount == 0 {
		log.Panic("no nodes to run")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	dir, err = os.MkdirTemp(dir, "sia-cluster-*")
	if err != nil {
		log.Panic("failed to create temp dir", zap.Error(err))
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			log.Error("failed to remove temp dir", zap.Error(err))
		} else {
			log.Debug("removed temp dir", zap.String("dir", dir))
		}
	}()

	// use modified Zen testnet
	n, genesis := chain.TestnetZen()
	n.InitialTarget = types.BlockID{0xFF}
	n.HardforkDevAddr.Height = 1
	n.HardforkTax.Height = 1
	n.HardforkStorageProof.Height = 1
	n.HardforkOak.Height = 1
	n.HardforkASIC.Height = 1
	n.HardforkFoundation.Height = 1

	if siafundAddr != "" {
		// if the siafund address is set, send the siafunds to it
		var addr types.Address
		if err := addr.UnmarshalText([]byte(siafundAddr)); err != nil {
			log.Panic("failed to parse siafund address", zap.Error(err))
		}
		genesis.Transactions[0].SiafundOutputs[0].Address = addr
	}

	switch network {
	case "v1":
		n.HardforkV2.AllowHeight = 10000 // ideally unattainable
		n.HardforkV2.RequireHeight = 12000
	case "v2":
		n.HardforkV2.AllowHeight = 2
		n.HardforkV2.RequireHeight = 3
	case "transition":
		n.HardforkV2.AllowHeight = 300
		n.HardforkV2.RequireHeight = 400
	default:
		log.Fatal("invalid network", zap.String("network", network))
	}

	apiListener, err := net.Listen("tcp", apiAddr)
	if err != nil {
		log.Panic("failed to listen on api address", zap.Error(err))
	}
	defer apiListener.Close()

	bdb, err := coreutils.OpenBoltChainDB(filepath.Join(dir, "consensus.db"))
	if err != nil {
		log.Panic("failed to open bolt db", zap.Error(err))
	}
	defer bdb.Close()

	dbstore, tipState, err := chain.NewDBStore(bdb, n, genesis, nil)
	if err != nil {
		log.Panic("failed to create dbstore", zap.Error(err))
	}
	cm := chain.NewManager(dbstore, tipState)

	syncerListener, err := net.Listen("tcp", ":0")
	if err != nil {
		log.Panic("failed to listen on api address", zap.Error(err))
	}
	defer syncerListener.Close()

	_, port, err := net.SplitHostPort(syncerListener.Addr().String())
	s := syncer.New(syncerListener, cm, testutil.NewEphemeralPeerStore(), gateway.Header{
		GenesisID:  genesis.ID(),
		UniqueID:   gateway.GenerateUniqueID(),
		NetAddress: "127.0.0.1:" + port,
	}, syncer.WithLogger(log.Named("syncer")), syncer.WithMaxInboundPeers(10000), syncer.WithBanDuration(time.Second), syncer.WithPeerDiscoveryInterval(5*time.Second), syncer.WithSyncInterval(5*time.Second)) // essentially no limit on inbound peers
	if err != nil {
		log.Panic("failed to create syncer", zap.Error(err))
	}
	defer s.Close()
	go s.Run()

	nm := nodes.NewManager(dir, cm, s, nodes.WithLog(log.Named("cluster")), nodes.WithSharedConsensus(true))
	defer nm.Close()

	server := &http.Server{
		Handler:     api.Handler(cm, s, nm, log.Named("api")),
		ReadTimeout: 5 * time.Second,
	}
	defer server.Close()
	go server.Serve(apiListener)

	for i := 0; i < hostdCount; i++ {
		ready := make(chan struct{}, 1)
		go func() {
			if err := nm.StartHostd(ctx, types.GeneratePrivateKey(), ready); err != nil {
				cancel()
				log.Error("hostd failed to start", zap.Error(err))
			}
		}()
		select {
		case <-ctx.Done():
			log.Panic("context canceled")
		case <-ready:
		}
	}

	for i := 0; i < renterdCount; i++ {
		ready := make(chan struct{}, 1)
		go func() {
			if err := nm.StartRenterd(ctx, types.GeneratePrivateKey(), ready); err != nil {
				cancel()
				log.Error("renterd failed to start", zap.Error(err))
			}
		}()
		select {
		case <-ctx.Done():
			log.Panic("context canceled")
		case <-ready:
		}
	}

	for i := 0; i < walletdCount; i++ {
		ready := make(chan struct{}, 1)
		go func() {
			if err := nm.StartWalletd(ctx, ready); err != nil {
				cancel()
				log.Error("walletd failed to start", zap.Error(err))
			}
		}()
		select {
		case <-ctx.Done():
			log.Panic("context canceled")
		case <-ready:
		}
	}

	for i := 0; i < exploredCount; i++ {
		ready := make(chan struct{}, 1)
		go func() {
			if err := nm.StartExplored(ctx, ready, apiPassword); err != nil {
				cancel()
				log.Error("explored failed to start", zap.Error(err))
			}
		}()
		select {
		case <-ctx.Done():
			log.Panic("context canceled")
		case <-ready:
			log.Debug("explored ready")
		}
	}

	// mine until all payouts have matured
	if err := nm.MineBlocks(ctx, 144, types.VoidAddress); err != nil {
		log.Panic("failed to mine blocks", zap.Error(err))
	}

	<-ctx.Done()
	log.Info("shutdown complete")
}
