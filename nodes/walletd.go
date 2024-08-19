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
	"go.sia.tech/hostd/build"
	"go.sia.tech/jape"
	"go.sia.tech/walletd/api"
	"go.sia.tech/walletd/persist/sqlite"
	"go.sia.tech/walletd/wallet"
	"go.uber.org/zap"
	"lukechampine.com/frand"
)

// StartWalletd starts a new walletd node. This function blocks until the context
// is canceled. All resources will be cleaned up before the function returns.
func (m *Manager) StartWalletd(ctx context.Context, ready chan<- struct{}) error {
	node := Node{
		ID:   NodeID(frand.Bytes(8)),
		Type: NodeTypeWalletd,
	}

	log := m.log.Named("walletd." + node.ID.String())

	syncerListener, err := net.Listen("tcp", ":0")
	if err != nil {
		return fmt.Errorf("failed to listen on syncer address: %w", err)
	}
	defer syncerListener.Close()

	httpListener, err := net.Listen("tcp", ":0")
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}
	defer httpListener.Close()

	dir, err := createNodeDir(m.dir, node.ID)
	if err != nil {
		return fmt.Errorf("failed to create node directory: %w", err)
	}
	defer os.RemoveAll(dir)

	store, err := sqlite.OpenDatabase(filepath.Join(dir, "walletd.sqlite3"), log.Named("sqlite3"))
	if err != nil {
		return fmt.Errorf("failed to open wallet database: %w", err)
	}
	defer store.Close()

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

	wm, err := wallet.NewManager(cm, store, wallet.WithLogger(log.Named("wallet")), wallet.WithIndexMode(wallet.IndexModePersonal)) // TODO switch index modes
	if err != nil {
		return fmt.Errorf("failed to create wallet manager: %w", err)
	}
	defer wm.Close()

	api := jape.BasicAuth("sia is cool")(api.NewServer(cm, s, wm, api.WithLogger(log.Named("api"))))
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
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

	log.Info("node started", zap.Stringer("http", httpListener.Addr()), zap.String("version", build.Version()), zap.String("commit", build.Commit()))
	node.APIAddress = "http://" + httpListener.Addr().String()
	node.Password = "sia is cool"
	m.Put(node)
	if ready != nil {
		ready <- struct{}{}
	}
	<-ctx.Done()
	m.Delete(node.ID)
	log.Info("shutting down")
	return nil
}
