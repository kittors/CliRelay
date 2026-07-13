package modelcatalog

import (
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	internalrouting "github.com/router-for-me/CLIProxyAPI/v6/internal/routing"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// Path availability contract:
// - Owner: model path capability boundary.
// - Responsibility: expose routing/path-specific model availability derived from registry metadata.
//
// Claude/Codex live discovery is merged into the root OpenAI path so the model
// catalog "path-only" enrichment matches the auth-file models panel.
func (s *Service) PathAvailability() map[string]any {
	modelRegistry := registry.GetGlobalRegistry()
	items := make(map[string]*modelPathAvailabilityResponse)

	routingConfig := tenantRoutingConfig(s.tenantID, s.cfg)
	rootOpenAICapabilities := openAIV1Capabilities("/")
	rootGeminiCapabilities := geminiV1BetaCapabilities("/")
	authByID := s.authByID()
	discoveryByProvider := s.sharedDiscoveryByProvider(false)
	openaiModels := dropStaticDiscoveryProviderModels(
		modelRegistry.GetAvailableModels("openai"),
		modelRegistry,
		discoveryByProvider,
		authByID,
		s.authGroupOwnerMappingMap(),
	)
	openaiModels = s.modelRootRouteScopedModels(openaiModels, routingConfig)
	// Live discovery is not registry-backed, so CanServe cannot enforce
	// channel-group AllowedModels for those rows. Filter after merge so plaza
	// and catalog path enrichment match configured-availability.
	openaiModels = s.filterModelsByRoutingAllowedModels(
		appendSharedDiscoveryModels(openaiModels, discoveryByProvider),
		"",
	)
	appendModelPaths(items, openaiModels, "/", rootOpenAICapabilities)
	appendModelPaths(items, s.filterModelsByRoutingAllowedModels(
		s.modelRootRouteScopedModels(modelRegistry.GetAvailableModels("gemini"), routingConfig),
		"",
	), "/", rootGeminiCapabilities)

	routes := []modelPathRouteResponse{
		{
			Label:        "系统默认",
			Path:         "/",
			System:       true,
			ReadOnly:     true,
			Capabilities: append(append([]modelPathCapabilityResponse{}, rootOpenAICapabilities...), rootGeminiCapabilities...),
		},
	}

	for _, route := range configuredPathRoutes(s.tenantID, routingConfig) {
		capabilities := append(append([]modelPathCapabilityResponse{}, openAIV1Capabilities(route.Path)...), geminiV1BetaCapabilities(route.Path)...)
		routes = append(routes, modelPathRouteResponse{
			Label:        route.Label,
			Path:         route.Path,
			Group:        route.Group,
			System:       false,
			ReadOnly:     false,
			Capabilities: capabilities,
		})
		appendModelPaths(items, s.filterModelsByRoutingAllowedModels(
			s.modelPathRouteScopedModels(modelRegistry.GetAvailableModels("openai"), route.Group),
			route.Group,
		), route.Path, openAIV1Capabilities(route.Path))
		appendModelPaths(items, s.filterModelsByRoutingAllowedModels(
			s.modelPathRouteScopedModels(modelRegistry.GetAvailableModels("gemini"), route.Group),
			route.Group,
		), route.Path, geminiV1BetaCapabilities(route.Path))
	}

	return map[string]any{
		"object": "list",
		"data":   sortModelPathAvailabilityRows(items),
		"routes": routes,
	}
}

func (s *Service) modelPathRouteScopedModels(models []map[string]any, routeGroup string) []map[string]any {
	routeGroup = internalrouting.NormalizeGroupName(routeGroup)
	if s == nil || s.authManager == nil || routeGroup == "" {
		return models
	}
	filtered := make([]map[string]any, 0, len(models))
	for _, model := range models {
		id := strings.TrimSpace(modelPathStringValue(model["id"]))
		if id == "" {
			continue
		}
		if s.authManager.CanServeModelWithScopesForTenant(s.tenantID, id, nil, nil, routeGroup) {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

func (s *Service) modelRootRouteScopedModels(models []map[string]any, _ *config.RoutingConfig) []map[string]any {
	if s == nil || s.authManager == nil {
		return models
	}
	// Always scope root path models to auths owned by the effective tenant.
	// IncludeDefaultGroup used to skip this filter entirely, which leaked the
	// process-global registry into non-system tenants (models page + path list).
	filtered := make([]map[string]any, 0, len(models))
	for _, model := range models {
		id := strings.TrimSpace(modelPathStringValue(model["id"]))
		if id == "" {
			continue
		}
		if s.authManager.CanServeModelWithScopesForTenant(s.tenantID, id, nil, nil, "") {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

func tenantRoutingConfig(tenantID string, cfg *config.Config) *config.RoutingConfig {
	if stored := usage.GetRoutingConfigForTenant(tenantID); stored != nil {
		return stored
	}
	if strings.TrimSpace(tenantID) == "" || tenantID == identity.SystemTenantID {
		if cfg != nil {
			routing := cfg.Routing
			return &routing
		}
	}
	return &config.RoutingConfig{}
}

func configuredPathRoutes(tenantID string, routingConfig *config.RoutingConfig) []configuredModelPathRoute {
	seen := make(map[string]struct{})
	out := []configuredModelPathRoute{}
	appendRoute := func(label, path, group string) {
		path = internalrouting.NormalizeNamespacePath(path)
		group = internalrouting.NormalizeGroupName(group)
		if path == "" || group == "" {
			return
		}
		key := strings.ToLower(path)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		if strings.TrimSpace(label) == "" {
			label = path
		}
		out = append(out, configuredModelPathRoute{
			Label: strings.TrimSpace(label),
			Path:  path,
			Group: group,
		})
	}

	if routingConfig == nil {
		routingConfig = &config.RoutingConfig{}
	}
	cfg := &config.Config{Routing: *routingConfig}
	cfg.SanitizeRouting()
	for _, route := range cfg.Routing.PathRoutes {
		appendRoute(route.Path, route.Path, route.Group)
	}
	for _, row := range usage.ListCcSwitchImportConfigsForTenant(tenantID) {
		if row.RoutePath == "" || len(row.AllowedChannelGroups) == 0 {
			continue
		}
		appendRoute(row.ProviderName, row.RoutePath, row.AllowedChannelGroups[0])
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Path < out[j].Path
	})
	return out
}
