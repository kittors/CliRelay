package cliproxy

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

// The SDK service applies streaming.busy-wait-max-waiters on start and on
// every reload, like the retry and concurrency settings next to it.
func TestServiceAppliesBusyWaitCap(t *testing.T) {
	svc := &Service{coreManager: coreauth.NewManager(nil, nil, nil)}
	cfg := &config.Config{}
	cfg.Streaming.BusyWaitMaxWaiters = 5
	svc.applyRetryConfig(cfg)
	if got := svc.coreManager.BusyWaitMaxWaiters(); got != 5 {
		t.Fatalf("cap = %d, want 5", got)
	}
	cfg.Streaming.BusyWaitMaxWaiters = -1
	svc.applyRetryConfig(cfg)
	if want := (config.StreamingConfig{}).BusyWaiters(); svc.coreManager.BusyWaitMaxWaiters() != want {
		t.Fatalf("cap after a negative value = %d, want the default %d", svc.coreManager.BusyWaitMaxWaiters(), want)
	}
}
