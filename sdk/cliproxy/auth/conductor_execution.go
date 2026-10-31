package auth

import (
	"context"
	"fmt"
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// Execute performs a non-streaming execution using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) Execute(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return m.executeGuarded(ctx, providers, req, opts)
}

// ExecuteCount performs a non-streaming execution using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) ExecuteCount(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return m.executeCountGuarded(ctx, providers, req, opts)
}

// ExecuteStream performs a streaming execution using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) ExecuteStream(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return m.executeStreamGuarded(ctx, providers, req, opts)
}

func (m *Manager) executeMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return newExecutionService(m).executeMixedOnce(ctx, providers, req, opts)
}

func (m *Manager) executeCountMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return newExecutionService(m).executeCountMixedOnce(ctx, providers, req, opts)
}

func (m *Manager) executeStreamMixedOnce(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return newExecutionService(m).executeStreamMixedOnce(ctx, providers, req, opts)
}

func ensureRequestedModelMetadata(opts cliproxyexecutor.Options, requestedModel string) cliproxyexecutor.Options {
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return opts
	}
	if hasRequestedModelMetadata(opts.Metadata) {
		return opts
	}
	if len(opts.Metadata) == 0 {
		opts.Metadata = map[string]any{cliproxyexecutor.RequestedModelMetadataKey: requestedModel}
		return opts
	}
	meta := make(map[string]any, len(opts.Metadata)+1)
	for k, v := range opts.Metadata {
		meta[k] = v
	}
	meta[cliproxyexecutor.RequestedModelMetadataKey] = requestedModel
	opts.Metadata = meta
	return opts
}

func hasRequestedModelMetadata(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	raw, ok := meta[cliproxyexecutor.RequestedModelMetadataKey]
	if !ok || raw == nil {
		return false
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v) != ""
	case []byte:
		return strings.TrimSpace(string(v)) != ""
	default:
		return false
	}
}

// Explicit single-pick controls pin execution to one auth; route groups only scope candidates.
func isSinglePickRouteRequest(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	if pinnedAuthIDFromMetadata(meta) != "" {
		return true
	}
	raw, ok := meta[cliproxyexecutor.SinglePickMetadataKey]
	if !ok || raw == nil {
		return false
	}
	enabled, ok := parseBoolAny(raw)
	return ok && enabled
}

func allowedChannelsFromMetadata(meta map[string]any) map[string]struct{} {
	if len(meta) == 0 {
		return nil
	}
	raw, ok := meta["allowed-channels"]
	if !ok || raw == nil {
		return nil
	}
	var values []string
	switch typed := raw.(type) {
	case string:
		values = strings.Split(typed, ",")
	case []string:
		values = typed
	case []any:
		values = make([]string, 0, len(typed))
		for _, item := range typed {
			values = append(values, fmt.Sprint(item))
		}
	default:
		return nil
	}
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		trimmed := strings.ToLower(strings.TrimSpace(value))
		if trimmed == "" {
			continue
		}
		out[trimmed] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func pinnedAuthIDFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	raw, ok := meta[cliproxyexecutor.PinnedAuthMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch val := raw.(type) {
	case string:
		return strings.TrimSpace(val)
	case []byte:
		return strings.TrimSpace(string(val))
	default:
		return ""
	}
}

func publishSelectedAuthMetadata(meta map[string]any, authID string) {
	if len(meta) == 0 {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	meta[cliproxyexecutor.SelectedAuthMetadataKey] = authID
	if callback, ok := meta[cliproxyexecutor.SelectedAuthCallbackMetadataKey].(func(string)); ok && callback != nil {
		callback(authID)
	}
}

func rewriteModelForAuth(model string, auth *Auth) string {
	if auth == nil || model == "" {
		return model
	}
	prefix := strings.TrimSpace(auth.Prefix)
	if prefix == "" {
		return model
	}
	needle := prefix + "/"
	if !strings.HasPrefix(model, needle) {
		return model
	}
	return strings.TrimPrefix(model, needle)
}
