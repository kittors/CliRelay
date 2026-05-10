package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	internalrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/routing"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func attachPathRouteContext(c *gin.Context, route *internalrouting.PathRouteContext) {
	if c == nil || route == nil {
		return
	}
	c.Set(internalrouting.GinPathRouteContextKey, route)
	if c.Request != nil {
		c.Request = c.Request.WithContext(internalrouting.WithPathRouteContext(c.Request.Context(), route))
	}
}

func resolvePathRouteContext(cfg *config.Config, authManager *cliproxyauth.Manager, rawGroup string) (*internalrouting.PathRouteContext, bool) {
	group := internalrouting.NormalizeGroupName(rawGroup)
	if group == "" {
		return nil, false
	}
	routePath := internalrouting.NormalizeNamespacePath(group)
	if routePath == "" {
		return nil, false
	}
	if row, ok := usage.FindCcSwitchImportConfigByRoutePath(routePath); ok {
		group := ""
		if len(row.AllowedChannelGroups) > 0 {
			group = internalrouting.NormalizeGroupName(row.AllowedChannelGroups[0])
		}
		if group == "" {
			return nil, false
		}
		return &internalrouting.PathRouteContext{
			RoutePath: row.RoutePath,
			Group:     group,
			Fallback:  "none",
			CcSwitch:  ccSwitchRouteContextFromImportConfig(row),
		}, true
	}
	if cfg != nil {
		for i := range cfg.Routing.PathRoutes {
			route := cfg.Routing.PathRoutes[i]
			if route.Path == routePath {
				return &internalrouting.PathRouteContext{
					RoutePath: route.Path,
					Group:     route.Group,
					Fallback:  internalrouting.NormalizeFallback(route.Fallback),
				}, true
			}
		}
	}
	if authManager != nil {
		if _, ok := authManager.KnownChannelGroups()[group]; ok {
			return &internalrouting.PathRouteContext{
				RoutePath: routePath,
				Group:     group,
				Fallback:  "none",
			}, true
		}
	}
	return nil, false
}

func ccSwitchRouteContextFromImportConfig(row usage.CcSwitchImportConfigRow) *internalrouting.CcSwitchRouteContext {
	mappings := make([]internalrouting.CcSwitchModelMapping, 0, len(row.ModelMappings))
	for _, mapping := range row.ModelMappings {
		mappings = append(mappings, internalrouting.CcSwitchModelMapping{
			Role:         strings.TrimSpace(mapping.Role),
			RequestModel: strings.TrimSpace(mapping.RequestModel),
			TargetModel:  strings.TrimSpace(mapping.TargetModel),
		})
	}
	return &internalrouting.CcSwitchRouteContext{
		ConfigID:             strings.TrimSpace(row.ID),
		ClientType:           strings.ToLower(strings.TrimSpace(row.ClientType)),
		RoutePath:            row.RoutePath,
		EndpointPath:         row.EndpointPath,
		AllowedChannelGroups: append([]string(nil), row.AllowedChannelGroups...),
		ModelMappings:        mappings,
	}
}

func groupRoutingMiddleware(resolve func(string) (*internalrouting.PathRouteContext, bool)) gin.HandlerFunc {
	return func(c *gin.Context) {
		if resolve == nil {
			c.Next()
			return
		}
		route, ok := resolve(c.Param("group"))
		if !ok || route == nil {
			abortChannelGroupRouteNotFound(c)
			return
		}
		attachPathRouteContext(c, route)
		c.Next()
	}
}

func abortChannelGroupRouteNotFound(c *gin.Context) {
	if c == nil {
		return
	}
	c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
		"error": map[string]any{
			"message": "channel group route not found",
			"type":    "invalid_request_error",
			"code":    "route_group_unavailable",
		},
	})
}

func splitGroupedAPIPath(path string) (string, string, bool) {
	path = strings.TrimSpace(path)
	if path == "" || path == "/" {
		return "", "", false
	}
	markers := []string{"/v1beta/", "/v1/"}
	for _, marker := range markers {
		idx := strings.LastIndex(path, marker)
		if idx <= 0 {
			continue
		}
		groupPath := path[:idx]
		apiPath := path[idx:]
		if internalrouting.NormalizeNamespacePath(groupPath) == "" {
			return "", "", false
		}
		return groupPath, apiPath, true
	}
	return "", "", false
}

func channelGroupAuthorizationMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		route := pathRouteContextFromGin(c)
		if route == nil || route.Group == "" {
			c.Next()
			return
		}

		metadataVal, exists := c.Get("accessMetadata")
		if !exists {
			c.Next()
			return
		}
		metadata, ok := metadataVal.(map[string]string)
		if !ok || len(metadata) == 0 {
			c.Next()
			return
		}
		allowed := internalrouting.ParseNormalizedSet(metadata["allowed-channel-groups"], internalrouting.NormalizeGroupName)
		if len(allowed) == 0 {
			c.Next()
			return
		}
		if _, ok := allowed[route.Group]; ok {
			c.Next()
			return
		}

		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"error": map[string]any{
				"message": "channel group is not allowed for this API key",
				"type":    "forbidden",
				"code":    "channel_group_forbidden",
				"group":   route.Group,
			},
		})
	}
}

func pathRouteContextFromGin(c *gin.Context) *internalrouting.PathRouteContext {
	if c == nil {
		return nil
	}
	raw, exists := c.Get(internalrouting.GinPathRouteContextKey)
	if exists {
		route, _ := raw.(*internalrouting.PathRouteContext)
		if route != nil {
			return route
		}
	}
	if c.Request != nil {
		return internalrouting.PathRouteContextFromContext(c.Request.Context())
	}
	return nil
}

func allowedChannelGroupsFromAccessMetadata(c *gin.Context) map[string]struct{} {
	if c == nil {
		return nil
	}
	metadataVal, exists := c.Get("accessMetadata")
	if !exists {
		return nil
	}
	metadata, ok := metadataVal.(map[string]string)
	if !ok {
		return nil
	}
	return internalrouting.ParseNormalizedSet(metadata["allowed-channel-groups"], internalrouting.NormalizeGroupName)
}

func channelGroupsForProviderLookup(c *gin.Context) []string {
	set := make(map[string]struct{})
	if route := pathRouteContextFromGin(c); route != nil && route.Group != "" {
		set[route.Group] = struct{}{}
	}
	for group := range allowedChannelGroupsFromAccessMetadata(c) {
		set[group] = struct{}{}
	}
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for group := range set {
		if strings.TrimSpace(group) == "" {
			continue
		}
		out = append(out, group)
	}
	return out
}
