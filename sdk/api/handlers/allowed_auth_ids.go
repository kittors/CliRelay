package handlers

import (
	"context"
	"strings"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type allowedAuthIDsContextKey struct{}

// WithAllowedAuthIDs returns a child context that restricts credential
// selection to exactly the given auth IDs. The restriction fails closed: an
// empty (or nil) list leaves no credential eligible. Blank IDs are dropped. It
// is meant for trusted server-side code (custom middleware or an embedding
// application), never for values copied from an untrusted client.
func WithAllowedAuthIDs(ctx context.Context, authIDs []string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	ids := make([]string, 0, len(authIDs))
	for _, id := range authIDs {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	return context.WithValue(ctx, allowedAuthIDsContextKey{}, ids)
}

// allowedAuthIDsFromContext returns a copy of the strict auth allowlist and
// whether one was set; an empty list with ok=true means "nothing eligible".
func allowedAuthIDsFromContext(ctx context.Context) ([]string, bool) {
	if ctx == nil {
		return nil, false
	}
	ids, ok := ctx.Value(allowedAuthIDsContextKey{}).([]string)
	if !ok {
		return nil, false
	}
	return append([]string{}, ids...), true
}

// withAllowedAuthIDsMetadata copies the context's strict auth allowlist, if
// any, into execution metadata so the auth manager enforces it.
func withAllowedAuthIDsMetadata(ctx context.Context, meta map[string]any) map[string]any {
	if allowedAuthIDs, ok := allowedAuthIDsFromContext(ctx); ok {
		meta[coreexecutor.AllowedAuthIDsMetadataKey] = allowedAuthIDs
	}
	return meta
}
