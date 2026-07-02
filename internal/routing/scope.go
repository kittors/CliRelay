package routing

import (
	"context"

	sdkrouting "github.com/router-for-me/CLIProxyAPI/v6/sdk/routing"
)

const GinPathRouteContextKey = sdkrouting.GinPathRouteContextKey

type PathRouteContext = sdkrouting.PathRouteContext
type CcSwitchRouteContext = sdkrouting.CcSwitchRouteContext
type CcSwitchModelMapping = sdkrouting.CcSwitchModelMapping

func WithPathRouteContext(ctx context.Context, route *PathRouteContext) context.Context {
	return sdkrouting.WithPathRouteContext(ctx, route)
}

func PathRouteContextFromContext(ctx context.Context) *PathRouteContext {
	return sdkrouting.PathRouteContextFromContext(ctx)
}

func IsCcSwitchMappedTargetModel(route *PathRouteContext, modelName string) bool {
	return sdkrouting.IsCcSwitchMappedTargetModel(route, modelName)
}

func NormalizeGroupName(value string) string {
	return sdkrouting.NormalizeGroupName(value)
}

func NormalizeNamespacePath(value string) string {
	return sdkrouting.NormalizeNamespacePath(value)
}

func NormalizeFallback(value string) string {
	return sdkrouting.NormalizeFallback(value)
}

func ParseNormalizedSet(raw string, normalizer func(string) string) map[string]struct{} {
	return sdkrouting.ParseNormalizedSet(raw, normalizer)
}
