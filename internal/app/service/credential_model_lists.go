package serviceapp

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	sdkmodelcatalog "github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
)

// Model lists of providers whose credentials each list their own models.
//
// An Antigravity, xAI or Kimi credential asks its own upstream which models it
// can call, and the answer differs per account. The service registers each
// credential from the last list it fetched and refreshes that list in the
// background; these are the pieces of that which live on the internal side of
// the sdk boundary.

// modelListSnapshotFile keeps those lists across restarts.
const modelListSnapshotFile = "upstream-model-lists.json"

var errNotSelfListing = errors.New("model lists: provider does not list models per credential")

// IsSelfListingProvider reports whether a provider's credentials each list their
// own models.
func IsSelfListingProvider(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "antigravity", "xai", "kimi":
		return true
	default:
		return false
	}
}

// DiscoverCredentialModels asks the upstream which models one credential of a
// self-listing provider can call. A failure is reported as an error; a cached
// list is never returned in its place.
func DiscoverCredentialModels(ctx context.Context, auth *coreauth.Auth, cfg *config.Config, provider string) ([]*sdkmodelcatalog.ModelInfo, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "antigravity":
		return executor.DiscoverAntigravityModels(ctx, auth, cfg)
	case "xai":
		return executor.DiscoverXAIModels(ctx, auth, cfg)
	case "kimi":
		return executor.DiscoverKimiModels(ctx, auth, cfg)
	default:
		return nil, errNotSelfListing
	}
}

// CredentialModelFloor lists the compiled-in models a credential of a
// self-listing provider registers while no credential of its provider has ever
// fetched a list.
//
// For Antigravity the compiled-in entries are overrides for models the upstream
// listed at some point, not a catalog of what it serves today, so they are a
// last resort. The editor completion models among them are dropped, as they are
// from a live answer.
func CredentialModelFloor(provider string) []*sdkmodelcatalog.ModelInfo {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if !IsSelfListingProvider(provider) {
		return nil
	}
	models := sdkmodelcatalog.StaticModelDefinitionsByChannel(provider)
	if provider != "antigravity" {
		return models
	}
	out := make([]*sdkmodelcatalog.ModelInfo, 0, len(models))
	for _, model := range models {
		if model == nil || executor.IsInternalAntigravityModelID(model.ID) {
			continue
		}
		out = append(out, model)
	}
	return out
}

// ModelListSnapshotPath is where the last list each credential fetched is kept
// across restarts: the persistent state directory, next to the auth directory
// and never inside it.
func ModelListSnapshotPath(authDir string) string {
	return filepath.Join(util.StateDir(authDir), modelListSnapshotFile)
}
