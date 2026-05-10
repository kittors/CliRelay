package routing

import (
	"context"
	"net/url"
	"strings"
	"unicode"
)

const (
	// GinPathRouteContextKey stores the resolved path-route context in gin.Context.
	GinPathRouteContextKey = "cliproxy.path_route"
)

// PathRouteContext captures request-scoped channel-group routing derived from the URL path.
type PathRouteContext struct {
	RoutePath string
	Group     string
	Fallback  string
	CcSwitch  *CcSwitchRouteContext
}

type CcSwitchRouteContext struct {
	ConfigID             string
	ClientType           string
	RoutePath            string
	EndpointPath         string
	AllowedChannelGroups []string
	ModelMappings        []CcSwitchModelMapping
}

type CcSwitchModelMapping struct {
	Role         string
	RequestModel string
	TargetModel  string
}

type pathRouteContextKey struct{}

// WithPathRouteContext returns a child context tagged with the resolved path-route scope.
func WithPathRouteContext(ctx context.Context, route *PathRouteContext) context.Context {
	if route == nil {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, pathRouteContextKey{}, clonePathRouteContext(route))
}

// PathRouteContextFromContext extracts the resolved path-route scope from context.
func PathRouteContextFromContext(ctx context.Context) *PathRouteContext {
	if ctx == nil {
		return nil
	}
	raw := ctx.Value(pathRouteContextKey{})
	route, _ := raw.(*PathRouteContext)
	if route == nil {
		return nil
	}
	return clonePathRouteContext(route)
}

func clonePathRouteContext(route *PathRouteContext) *PathRouteContext {
	if route == nil {
		return nil
	}
	cloned := *route
	if route.CcSwitch != nil {
		ccSwitch := *route.CcSwitch
		if route.CcSwitch.AllowedChannelGroups != nil {
			ccSwitch.AllowedChannelGroups = append([]string(nil), route.CcSwitch.AllowedChannelGroups...)
		}
		if route.CcSwitch.ModelMappings != nil {
			ccSwitch.ModelMappings = append([]CcSwitchModelMapping(nil), route.CcSwitch.ModelMappings...)
		}
		cloned.CcSwitch = &ccSwitch
	}
	return &cloned
}

// NormalizeGroupName trims, lowercases, and canonicalizes channel group names.
func NormalizeGroupName(value string) string {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	trimmed = strings.Trim(trimmed, "/")
	return trimmed
}

// NormalizeNamespacePath converts route namespace inputs like "pro", "/pro/",
// "/openai/pro", or "https://example.com/openai/pro" to a canonical path.
func NormalizeNamespacePath(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	if parsed, err := url.Parse(trimmed); err == nil && parsed != nil && parsed.Scheme != "" && parsed.Host != "" {
		trimmed = parsed.EscapedPath()
		if decoded, errDecode := url.PathUnescape(trimmed); errDecode == nil {
			trimmed = decoded
		}
	}
	if idx := strings.IndexAny(trimmed, "?#"); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	trimmed = strings.Trim(trimmed, "/")
	if trimmed == "" {
		return ""
	}
	segments := strings.Split(trimmed, "/")
	for _, segment := range segments {
		if segment == "" {
			return ""
		}
		for _, r := range segment {
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
				continue
			}
			return ""
		}
	}
	return "/" + trimmed
}

// NormalizeFallback canonicalizes fallback values. Empty defaults to "none".
func NormalizeFallback(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "none":
		return "none"
	case "default":
		return "default"
	default:
		return "none"
	}
}

// ParseNormalizedSet splits a comma-separated string into a normalized set.
func ParseNormalizedSet(raw string, normalizer func(string) string) map[string]struct{} {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := make(map[string]struct{})
	for _, part := range strings.Split(raw, ",") {
		value := strings.TrimSpace(part)
		if normalizer != nil {
			value = normalizer(value)
		}
		if value == "" {
			continue
		}
		out[value] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
