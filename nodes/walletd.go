package nodes

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"go.sia.tech/coreutils/chain"
	"go.sia.tech/coreutils/syncer"
	"go.sia.tech/hostd/build"
	"go.sia.tech/jape"
	"go.sia.tech/walletd/api"
	"go.sia.tech/walletd/persist/sqlite"
	"go.sia.tech/walletd/wallet"
	"go.uber.org/zap"
	"lukechampine.com/frand"
)

func Walletd(ctx context.Context, baseDir string, cm *chain.Manager, s *syncer.Syncer, nm *Manager, log *zap.Logger) error {
	node := Node{
		ID:   NodeID(frand.Bytes(8)),
		Type: NodeTypeWalletd,
	}
	log = log.Named(node.ID.String())

	httpListener, err := net.Listen("tcp", ":0")
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}
	defer httpListener.Close()

	dir, err := createNodeDir(baseDir, node.ID)
	if err != nil {
		return fmt.Errorf("failed to create node directory: %w", err)
	}

	store, err := sqlite.OpenDatabase(filepath.Join(dir, "walletd.sqlite3"), log.Named("sqlite3"))
	if err != nil {
		return fmt.Errorf("failed to open wallet database: %w", err)
	}
	defer store.Close()

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
	nm.Put(node)
	<-ctx.Done()
	nm.Delete(node.ID)
	log.Info("shutting down")
	return nil
}
