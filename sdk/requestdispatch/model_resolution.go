package requestdispatch

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	sdklogging "github.com/router-for-me/CLIProxyAPI/v6/sdk/logging"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/modelcatalog"
	sdkrouting "github.com/router-for-me/CLIProxyAPI/v6/sdk/routing"
	sdkthinking "github.com/router-for-me/CLIProxyAPI/v6/sdk/thinking"
)

func ResolveRequestDetails(ctx context.Context, modelName string) (providers []string, normalizedModel string, err *sdklogging.ErrorMessage) {
	var resolvedModelName string
	initialSuffix := sdkthinking.ParseSuffix(modelName)
	if initialSuffix.ModelName == "auto" {
		resolvedBase := modelcatalog.ResolveAutoModel(initialSuffix.ModelName)
		if initialSuffix.HasSuffix {
			resolvedModelName = fmt.Sprintf("%s(%s)", resolvedBase, initialSuffix.RawSuffix)
		} else {
			resolvedModelName = resolvedBase
		}
	} else {
		resolvedModelName = modelcatalog.ResolveAutoModel(modelName)
	}

	parsed := sdkthinking.ParseSuffix(resolvedModelName)
	baseModel := strings.TrimSpace(parsed.ModelName)
	routeCtx := routeContextFromExecutionContext(ctx)
	requestedPrefix, unprefixedModel := splitRequestedModelPrefix(baseModel)
	if routeCtx != nil && routeCtx.Group != "" && requestedPrefix != "" && requestedPrefix != routeCtx.Group && !sdkrouting.IsCcSwitchMappedTargetModel(routeCtx, baseModel) {
		return nil, "", &sdklogging.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error: fmt.Errorf(
				`{"error":{"message":"model prefix conflicts with route group","type":"invalid_request_error","code":"model_prefix_conflict"}}`,
			),
		}
	}

	scopedGroups := allowedChannelGroupsFromExecutionContext(ctx)
	if routeCtx != nil && routeCtx.Group != "" {
		scopedGroups = append(scopedGroups, routeCtx.Group)
	}
	lookupModel := baseModel
	if routeCtx != nil && routeCtx.Group != "" && requestedPrefix == "" && routeCtx.Group != "default" && unprefixedModel != "" {
		lookupModel = unprefixedModel
	}

	providers = scopedProvidersForModel(lookupModel, scopedGroups)
	if len(providers) == 0 && baseModel != resolvedModelName {
		providers = scopedProvidersForModel(resolvedModelName, scopedGroups)
	}

	if len(providers) == 0 {
		return nil, "", &sdklogging.ErrorMessage{
			StatusCode: http.StatusBadGateway,
			Error:      fmt.Errorf("unknown provider for model %s", modelName),
		}
	}

	if parsed.HasSuffix {
		resolvedModelName = parsed.ModelName + "(" + parsed.RawSuffix + ")"
	} else {
		resolvedModelName = parsed.ModelName
	}

	return providers, resolvedModelName, nil
}

func scopedProvidersForModel(modelName string, groups []string) []string {
	registryRef := modelcatalog.GlobalRegistry()
	providers := make([]string, 0, 4)
	seen := make(map[string]struct{})
	appendProviders := func(candidates []string) {
		for _, provider := range candidates {
			provider = strings.TrimSpace(provider)
			if provider == "" {
				continue
			}
			if _, exists := seen[provider]; exists {
				continue
			}
			seen[provider] = struct{}{}
			providers = append(providers, provider)
		}
	}
	appendProviders(modelcatalog.GetProviderName(modelName))
	if registryRef != nil {
		for _, group := range groups {
			group = sdkrouting.NormalizeGroupName(group)
			if group == "" || group == "default" {
				continue
			}
			appendProviders(registryRef.GetModelProviders(group + "/" + modelName))
		}
	}
	return providers
}

func routeContextFromExecutionContext(ctx context.Context) *sdkrouting.PathRouteContext {
	if ctx == nil {
		return nil
	}
	if route := sdkrouting.PathRouteContextFromContext(ctx); route != nil {
		return route
	}
	ginCtx := ginContextFromExecutionContext(ctx)
	if ginCtx == nil {
		return nil
	}
	raw, exists := ginCtx.Get(sdkrouting.GinPathRouteContextKey)
	if !exists {
		return nil
	}
	route, _ := raw.(*sdkrouting.PathRouteContext)
	return route
}

func allowedChannelGroupsFromExecutionContext(ctx context.Context) []string {
	if ctx == nil {
		return nil
	}
	ginCtx := ginContextFromExecutionContext(ctx)
	if ginCtx == nil {
		return nil
	}
	metadataVal, exists := ginCtx.Get("accessMetadata")
	if !exists {
		return nil
	}
	metadata, ok := metadataVal.(map[string]string)
	if !ok {
		return nil
	}
	set := sdkrouting.ParseNormalizedSet(metadata["allowed-channel-groups"], sdkrouting.NormalizeGroupName)
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for group := range set {
		out = append(out, group)
	}
	return out
}

func splitRequestedModelPrefix(modelName string) (string, string) {
	trimmed := strings.TrimSpace(modelName)
	if trimmed == "" {
		return "", ""
	}
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) != 2 {
		return "", trimmed
	}
	return sdkrouting.NormalizeGroupName(parts[0]), strings.TrimSpace(parts[1])
}
