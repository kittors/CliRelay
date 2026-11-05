package requestdispatch

import (
	"context"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkrequestctx "github.com/router-for-me/CLIProxyAPI/v6/sdk/requestctx"
	sdkrouting "github.com/router-for-me/CLIProxyAPI/v6/sdk/routing"
)

const idempotencyKeyMetadataKey = "idempotency_key"

type MetadataAccessors struct {
	PinnedAuthID func(context.Context) string
	// AllowedAuthIDs returns the strict auth allowlist and whether one is set.
	// ok=true with an empty list leaves no credential eligible.
	AllowedAuthIDs         func(context.Context) ([]string, bool)
	SelectedAuthIDCallback func(context.Context) func(string)
	ExecutionSessionID     func(context.Context) string
}

func BuildScopedExecutionMetadata(ctx context.Context) map[string]any {
	allowedChannels := ""
	allowedChannelGroups := ""
	routeGroup := ""
	routeFallback := ""
	tenantID := ""
	if route := sdkrouting.PathRouteContextFromContext(ctx); route != nil {
		routeGroup = strings.TrimSpace(route.Group)
		routeFallback = strings.TrimSpace(route.Fallback)
	}
	if ginCtx := ginContextFromExecutionContext(ctx); ginCtx != nil {
		if tenantValue, exists := ginCtx.Get("tenantID"); exists {
			tenantID, _ = tenantValue.(string)
			tenantID = strings.TrimSpace(tenantID)
		}
		if metadataVal, exists := ginCtx.Get("accessMetadata"); exists {
			if metadata, okMeta := metadataVal.(map[string]string); okMeta {
				allowedChannels = strings.TrimSpace(metadata["allowed-channels"])
				allowedChannelGroups = strings.TrimSpace(metadata["allowed-channel-groups"])
			}
		}
		if routeVal, exists := ginCtx.Get(sdkrouting.GinPathRouteContextKey); exists {
			if route, okRoute := routeVal.(*sdkrouting.PathRouteContext); okRoute && route != nil {
				routeGroup = strings.TrimSpace(route.Group)
				routeFallback = strings.TrimSpace(route.Fallback)
			}
		}
		if (routeGroup == "" || routeFallback == "") && ginCtx.Request != nil {
			if route := sdkrouting.PathRouteContextFromContext(ginCtx.Request.Context()); route != nil {
				if routeGroup == "" {
					routeGroup = strings.TrimSpace(route.Group)
				}
				if routeFallback == "" {
					routeFallback = strings.TrimSpace(route.Fallback)
				}
			}
		}
	}
	meta := map[string]any{}
	if tenantID != "" {
		meta[coreexecutor.TenantMetadataKey] = tenantID
	}
	if allowedChannels != "" {
		meta["allowed-channels"] = allowedChannels
	}
	if allowedChannelGroups != "" {
		meta["allowed-channel-groups"] = allowedChannelGroups
	}
	if routeGroup != "" {
		meta[coreexecutor.RouteGroupMetadataKey] = routeGroup
	}
	if routeFallback != "" {
		meta[coreexecutor.RouteFallbackMetadataKey] = routeFallback
	}
	if len(meta) == 0 {
		return nil
	}
	return meta
}

func BuildExecutionMetadata(ctx context.Context, accessors MetadataAccessors) map[string]any {
	key := ""
	if ginCtx := ginContextFromExecutionContext(ctx); ginCtx != nil && ginCtx.Request != nil {
		key = strings.TrimSpace(ginCtx.GetHeader("Idempotency-Key"))
	}
	if key == "" {
		key = uuid.NewString()
	}

	meta := BuildScopedExecutionMetadata(ctx)
	if meta == nil {
		meta = map[string]any{}
	}
	meta[idempotencyKeyMetadataKey] = key
	if accessors.PinnedAuthID != nil {
		if pinnedAuthID := accessors.PinnedAuthID(ctx); pinnedAuthID != "" {
			meta[coreexecutor.PinnedAuthMetadataKey] = pinnedAuthID
		}
	}
	if accessors.AllowedAuthIDs != nil {
		if allowedAuthIDs, ok := accessors.AllowedAuthIDs(ctx); ok {
			meta[coreexecutor.AllowedAuthIDsMetadataKey] = append([]string{}, allowedAuthIDs...)
		}
	}
	if accessors.SelectedAuthIDCallback != nil {
		if selectedCallback := accessors.SelectedAuthIDCallback(ctx); selectedCallback != nil {
			meta[coreexecutor.SelectedAuthCallbackMetadataKey] = selectedCallback
		}
	}
	if accessors.ExecutionSessionID != nil {
		if executionSessionID := accessors.ExecutionSessionID(ctx); executionSessionID != "" {
			meta[coreexecutor.ExecutionSessionMetadataKey] = executionSessionID
		}
	}
	return meta
}

func ginContextFromExecutionContext(ctx context.Context) *gin.Context {
	if ctx == nil {
		return nil
	}
	ginCtx, _ := ctx.Value(sdkrequestctx.ContextKeyGin).(*gin.Context)
	return ginCtx
}
