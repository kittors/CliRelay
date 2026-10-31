package api

import (
	"testing"

	proxyconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// streaming.busy-wait-max-waiters reaches the auth manager when the server
// starts and again on every reload, and a reload back to unset or 0 returns it
// to the default rather than leaving the last value in force.
func TestServerAppliesBusyWaitCapAtStartAndOnReload(t *testing.T) {
	server := newTestServerWithConfig(t, func(cfg *proxyconfig.Config) { cfg.Streaming.BusyWaitMaxWaiters = 7 })
	manager := server.handlers.AuthManager
	if got := manager.BusyWaitMaxWaiters(); got != 7 {
		t.Fatalf("cap after start = %d, want the configured 7", got)
	}

	raised := *server.cfg
	raised.Streaming.BusyWaitMaxWaiters = 25
	server.applyAuthRuntimeConfig(server.cfg, &raised)
	if got := manager.BusyWaitMaxWaiters(); got != 25 {
		t.Fatalf("cap after a reload to 25 = %d", got)
	}

	cleared := raised
	cleared.Streaming.BusyWaitMaxWaiters = 0
	server.applyAuthRuntimeConfig(&raised, &cleared)
	if got := manager.BusyWaitMaxWaiters(); got != proxyconfig.DefaultBusyWaitMaxWaiters {
		t.Fatalf("cap after a reload to 0 = %d, want the default %d", got, proxyconfig.DefaultBusyWaitMaxWaiters)
	}
}
