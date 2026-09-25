package cmd

import (
	"context"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/store/clusterauth"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
)

const (
	// clusterAuthStartTimeout bounds binding the credential store, which on
	// the first node includes waiting for the import lock and the import.
	clusterAuthStartTimeout = 5 * time.Minute
	// clusterAuthCloseTimeout bounds writing buffered runtime state on exit.
	clusterAuthCloseTimeout = 10 * time.Second
)

// startClusterAuthStore binds the credential store main registered for
// cluster mode to the runtime database. It must run after startCluster, so
// the store's change subscription lands on the live coordinator rather than
// the single-node placeholder, and before the service loads credentials.
// Single-node deployments register the file store, and nothing happens.
func startClusterAuthStore(cfg *config.Config) (stop func(), err error) {
	store, ok := sdkAuth.GetTokenStore().(*clusterauth.Store)
	if !ok || cfg == nil || !cfg.Cluster.Enabled {
		return func() {}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), clusterAuthStartTimeout)
	defer cancel()
	if err = store.Start(ctx, usage.RuntimeDB()); err != nil {
		return nil, err
	}
	return func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), clusterAuthCloseTimeout)
		defer closeCancel()
		store.Close(closeCtx)
	}, nil
}
