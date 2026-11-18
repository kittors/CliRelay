package auth

import cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"

func isSinglePickRouteRequest(meta map[string]any) bool {
	if routeGroupFromMetadata(meta) != "" {
		return true
	}
	if len(meta) == 0 {
		return false
	}
	raw, ok := meta[cliproxyexecutor.SinglePickMetadataKey]
	if !ok || raw == nil {
		return false
	}
	enabled, ok := parseBoolAny(raw)
	return ok && enabled
}
