package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gin "github.com/gin-gonic/gin"
	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func newRouteTestServer(t *testing.T, configure func(*proxyconfig.Config), opts ...ServerOption) *Server {
	t.Helper()

	gin.SetMode(gin.TestMode)

	tmpDir := t.TempDir()
	authDir := filepath.Join(tmpDir, "auth")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("failed to create auth dir: %v", err)
	}

	cfg := &proxyconfig.Config{
		SDKConfig: sdkconfig.SDKConfig{
			APIKeys: []string{"test-key"},
		},
		Port:                   0,
		AuthDir:                authDir,
		Debug:                  true,
		LoggingToFile:          false,
		UsageStatisticsEnabled: false,
	}
	if configure != nil {
		configure(cfg)
	}

	authManager := auth.NewManager(nil, nil, nil)
	accessManager := sdkaccess.NewManager()
	configPath := filepath.Join(tmpDir, "config.yaml")
	return NewServer(cfg, authManager, accessManager, configPath, opts...)
}

func routeKeySet(engine *gin.Engine) map[string]struct{} {
	keys := make(map[string]struct{})
	for _, route := range engine.Routes() {
		keys[route.Method+" "+route.Path] = struct{}{}
	}
	return keys
}

func managementRouteCount(engine *gin.Engine) int {
	count := 0
	for _, route := range engine.Routes() {
		if strings.HasPrefix(route.Path, "/v0/management") {
			count++
		}
	}
	return count
}

func TestPublicAndOAuthRoutesRemainStable(t *testing.T) {
	server := newRouteTestServer(t, nil)
	routes := routeKeySet(server.engine)

	required := []string{
		"GET /",
		"GET /management.html",
		"GET /manage",
		"GET /manage/*filepath",
		"GET /v1/models",
		"POST /v1/chat/completions",
		"POST /v1/responses",
		"GET /v1beta/models",
		"POST /v1beta/models/*action",
		"GET /anthropic/callback",
		"GET /codex/callback",
		"GET /google/callback",
		"GET /iflow/callback",
		"GET /antigravity/callback",
		"GET /xai/callback",
	}
	for _, key := range required {
		if _, ok := routes[key]; !ok {
			t.Fatalf("required route %s was not registered", key)
		}
	}
}

func TestManagementRoutesRegisterOnlyWithAvailableSecret(t *testing.T) {
	noSecret := newRouteTestServer(t, nil)
	if got := managementRouteCount(noSecret.engine); got != 0 {
		t.Fatalf("management routes without secret = %d, want 0", got)
	}

	withRemoteSecret := newRouteTestServer(t, func(cfg *proxyconfig.Config) {
		cfg.RemoteManagement.SecretKey = "remote-secret"
	})
	if got := managementRouteCount(withRemoteSecret.engine); got == 0 {
		t.Fatal("expected management routes when remote secret is configured")
	}

	withLocalPassword := newRouteTestServer(t, nil, WithLocalManagementPassword("local-secret"))
	if got := managementRouteCount(withLocalPassword.engine); got == 0 {
		t.Fatal("expected management routes when local management password is configured")
	}

	t.Setenv("MANAGEMENT_PASSWORD", "env-secret")
	withEnvSecret := newRouteTestServer(t, nil)
	if got := managementRouteCount(withEnvSecret.engine); got == 0 {
		t.Fatal("expected management routes when MANAGEMENT_PASSWORD is configured")
	}
}
