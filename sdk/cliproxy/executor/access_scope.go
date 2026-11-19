package executor

import "strings"

// CopyAccessSelectionScope copies what the API key authentication found that
// scopes credential selection from the request's access metadata into
// execution metadata: the channels and channel groups the key may use, and the
// providers the requester's access is frozen for. Every handler that builds
// execution metadata goes through it, so none of them forgets a restriction.
func CopyAccessSelectionScope(access map[string]string, meta map[string]any) {
	if len(access) == 0 || meta == nil {
		return
	}
	if allowed := strings.TrimSpace(access["allowed-channels"]); allowed != "" {
		meta["allowed-channels"] = allowed
	}
	if groups := strings.TrimSpace(access["allowed-channel-groups"]); groups != "" {
		meta["allowed-channel-groups"] = groups
	}
	if frozen := strings.TrimSpace(access[AccessFrozenProvidersKey]); frozen != "" {
		meta[FrozenProvidersMetadataKey] = frozen
	}
}
