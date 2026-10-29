package modeldiscovery

import (
	"strings"

	sdkrouting "github.com/router-for-me/CLIProxyAPI/v6/sdk/routing"
)

// followedProvider is the provider whose discovered list a channel group that
// follows upstream grows from. Listed explains why it is Codex alone.
const followedProvider = "codex"

func init() {
	// The request gate lives in sdk/cliproxy/auth, which cannot import this
	// package. Publishing the lookup here keeps "the manifest listed it" and
	// "a following channel group will serve it" on the same store.
	sdkrouting.SetDiscoveredModel(func(tenantID, modelID string) bool {
		return Listed(tenantID, followedProvider, modelID)
	})
}

// FollowedModelIDs returns the ids a channel group that follows upstream may
// admit for tenantID beyond its allow list: the same list the gate consults
// through sdkrouting.DiscoveredModel, however old, so the panel offers exactly
// what a request would be let through with.
func FollowedModelIDs(tenantID string) []string {
	models := Snapshot(tenantID, followedProvider)
	ids := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		id := strings.TrimSpace(model.ID)
		key := strings.ToLower(id)
		if id == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}
