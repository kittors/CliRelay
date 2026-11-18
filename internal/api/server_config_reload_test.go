package api

import (
	"testing"

	proxyconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestUpdateManagementRouteAvailability_TogglesRemoteSecretState(t *testing.T) {
	server := newTestServer(t)

	if server.managementRoutesRegistered.Load() {
		t.Fatal("management routes should not be registered before a secret is configured")
	}
	if server.managementRoutesEnabled.Load() {
		t.Fatal("management routes should start disabled without a secret")
	}

	enabled := *server.cfg
	enabled.RemoteManagement.SecretKey = "remote-secret"
	server.updateManagementRouteAvailability(server.cfg, &enabled)

	if !server.managementRoutesRegistered.Load() {
		t.Fatal("expected management routes to register after secret configuration")
	}
	if !server.managementRoutesEnabled.Load() {
		t.Fatal("expected management routes to be enabled after secret configuration")
	}

	disabled := enabled
	disabled.RemoteManagement.SecretKey = ""
	server.updateManagementRouteAvailability(&enabled, &disabled)

	if !server.managementRoutesRegistered.Load() {
		t.Fatal("expected registered routes to remain attached after disable")
	}
	if server.managementRoutesEnabled.Load() {
		t.Fatal("expected management routes to be disabled after secret removal")
	}
}

func TestUpdateManagementRouteAvailability_EnvSecretKeepsRoutesEnabled(t *testing.T) {
	server := newTestServer(t)
	server.envManagementSecret = true

	server.updateManagementRouteAvailability(&proxyconfig.Config{}, &proxyconfig.Config{})

	if !server.managementRoutesRegistered.Load() {
		t.Fatal("expected env secret to register management routes")
	}
	if !server.managementRoutesEnabled.Load() {
		t.Fatal("expected env secret to keep management routes enabled")
	}
}
