package cliproxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

// TestRunServesWhileTheAntigravityUpstreamHangs drives the real Antigravity model
// listing against an upstream that accepts connections and never answers, which
// is how a proxy that cannot reach its exit looks from here. Registration used to
// list each credential in turn before Run listened, waiting out a 15s timeout for
// every one, and then registered nothing for them.
func TestRunServesWhileTheAntigravityUpstreamHangs(t *testing.T) {
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	resetModelDiscovery(t)

	release := make(chan struct{})
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))

	const credentials = 6
	auths := make([]*coreauth.Auth, 0, credentials)
	for i := range credentials {
		auth := &coreauth.Auth{
			ID:         fmt.Sprintf("ag-upstream-hangs-%d", i),
			Provider:   "antigravity",
			Status:     coreauth.StatusActive,
			Attributes: map[string]string{"auth_kind": "oauth", "base_url": upstream.URL},
			Metadata: map[string]any{
				"access_token": "access-token",
				"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
				"project_id":   "hanging-upstream-project",
			},
		}
		GlobalModelRegistry().UnregisterClient(auth.ID)
		auths = append(auths, auth)
	}

	listening := make(chan struct{})
	service := &Service{
		cfg:            &config.Config{AuthDir: filepath.Join(t.TempDir(), "auths")},
		configPath:     filepath.Join(t.TempDir(), "config.yaml"),
		tokenProvider:  startupTokenProviderStub{},
		apiKeyProvider: startupAPIKeyProviderStub{},
		watcherFactory: func(string, string, func(*config.Config)) (*WatcherWrapper, error) {
			return &WatcherWrapper{
				start:                 func(context.Context) error { return nil },
				stop:                  func() error { return nil },
				setConfig:             func(*config.Config) {},
				setUpdateQueue:        func(chan<- runtimeAuthUpdate) {},
				dispatchRuntimeUpdate: func(runtimeAuthUpdate) bool { return false },
			}, nil
		},
		coreManager: coreauth.NewManager(&startupStoreStub{auths: auths}, &coreauth.RoundRobinSelector{}, nil),
		hooks:       Hooks{OnAfterStart: func(*Service) { close(listening) }},
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- service.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		close(release)
		upstream.CloseClientConnections()
		select {
		case <-runErr:
		case <-time.After(30 * time.Second):
			t.Error("Run did not return after its context was cancelled")
		}
		upstream.Close()
		for _, auth := range auths {
			GlobalModelRegistry().UnregisterClient(auth.ID)
		}
	})

	select {
	case <-listening:
	case err := <-runErr:
		t.Fatalf("Run returned before serving: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatalf("not listening 10s after Run while the Antigravity upstream hangs (%d listing requests made)", requests.Load())
	}
	for _, auth := range auths {
		if len(GlobalModelRegistry().GetModelsForClient(auth.ID)) == 0 {
			t.Fatalf("%s serves no models while its upstream hangs", auth.ID)
		}
	}
	// The listings were still made, in the background.
	waitForCondition(t, "the background listings to reach the upstream", func() bool { return requests.Load() > 0 })
}
