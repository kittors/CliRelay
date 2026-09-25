package cmd

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/store/clusterauth"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
)

func TestStartClusterAuthStoreIsInertOnASingleNode(t *testing.T) {
	previous := sdkAuth.GetTokenStore()
	t.Cleanup(func() { sdkAuth.RegisterTokenStore(previous) })
	sdkAuth.RegisterTokenStore(sdkAuth.NewFileTokenStore())
	stop, err := startClusterAuthStore(&config.Config{})
	if err != nil || stop == nil {
		t.Fatalf("single node: stop missing=%v err=%v", stop == nil, err)
	}
	stop()
}

func TestStartClusterAuthStoreNeedsTheRuntimeDatabase(t *testing.T) {
	previous := sdkAuth.GetTokenStore()
	t.Cleanup(func() { sdkAuth.RegisterTokenStore(previous) })
	sdkAuth.RegisterTokenStore(clusterauth.New(clusterauth.Options{AuthDir: t.TempDir()}))
	cfg := &config.Config{}
	cfg.Cluster.Enabled = true
	if _, err := startClusterAuthStore(cfg); err == nil || !strings.Contains(err.Error(), "PostgreSQL runtime database") {
		t.Fatalf("err = %v, cluster mode without the runtime database must refuse to start", err)
	}
}
