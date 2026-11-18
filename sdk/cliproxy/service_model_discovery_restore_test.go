package cliproxy

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/modeldiscovery"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	serviceapp "github.com/router-for-me/CLIProxyAPI/v8/sdkbridge/service"
)

// After a restart the models only the manifest lists route from the saved list,
// before any fetch has answered. On 2026-10-11 a restart emptied the in-memory
// list while the fetch could not reach the upstream, and Codex clients got
// "unknown provider" for every current model.
func TestRestartRoutesManifestModelsFromTheSavedList(t *testing.T) {
	resetModelDiscovery(t)
	const tenantID = "tenant-restart-discovery"
	authDir := filepath.Join(t.TempDir(), "auth")

	// The previous process saved its list, then exited.
	modeldiscovery.SetSnapshotPath(serviceapp.ProviderDiscoverySnapshotPath(authDir))
	modeldiscovery.Store(tenantID, "codex", []*ModelInfo{manifestOnlyInfo()})
	modeldiscovery.ResetForTest()

	service := &Service{cfg: &config.Config{AuthDir: authDir}}
	service.loadProviderDiscovery()

	auth := codexOAuthAuth("codex-restart-discovery", tenantID)
	reg := unregisterOnCleanup(t, auth.ID)
	service.registerModelsForAuth(context.Background(), auth)

	if !reg.ClientSupportsModel(auth.ID, manifestOnlyModel) {
		t.Fatalf("a model the saved manifest lists is not routable after a restart: %v", modelIDs(reg.GetModelsForClient(auth.ID)))
	}
}
