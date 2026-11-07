package executor

import (
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"golang.org/x/net/http/httpguts"
)

// applyCodexServiceTierPolicy adjusts the top-level "service_tier" on an outbound Codex request body
// based on per-account settings and incoming client payload. Upstream remains
// authoritative for model/account tier eligibility; never silently downgrade it.
func applyCodexServiceTierPolicy(body []byte, originalClientPayload []byte, auth *cliproxyauth.Auth) []byte {
	if len(body) == 0 {
		return body
	}
	tierPolicy := ""
	if auth != nil {
		for _, key := range []string{"codex_service_tier", "service_tier", "codex-service-tier"} {
			if auth.Attributes != nil {
				if v := strings.TrimSpace(strings.ToLower(auth.Attributes[key])); v != "" {
					tierPolicy = v
					break
				}
			}
			if auth.Metadata != nil {
				if raw, ok := auth.Metadata[key]; ok {
					if s, isStr := raw.(string); isStr {
						if v := strings.TrimSpace(strings.ToLower(s)); v != "" {
							tierPolicy = v
							break
						}
					}
				}
			}
		}
	}

	switch tierPolicy {
	case "priority", "fast":
		// Force priority / fast mode on outbound body
		return util.MutateTopLevelObject(body, map[string][]byte{
			"service_tier": util.JSONString("priority"),
		}, nil)

	case "ultrafast", "flex":
		// Keep Ultrafast distinct from priority/Fast.
		return util.MutateTopLevelObject(body, map[string][]byte{
			"service_tier": util.JSONString(tierPolicy),
		}, nil)

	case "pass":
		// Passthrough client's requested service_tier (if any), normalized:
		// "fast" -> "priority", valid tiers kept, invalid dropped
		clientTier := ""
		if len(originalClientPayload) > 0 {
			clientTier = strings.TrimSpace(strings.ToLower(gjson.GetBytes(originalClientPayload, "service_tier").String()))
		}
		if clientTier == "" {
			clientTier = strings.TrimSpace(strings.ToLower(gjson.GetBytes(body, "service_tier").String()))
		}

		switch clientTier {
		case "priority", "fast":
			return util.MutateTopLevelObject(body, map[string][]byte{
				"service_tier": util.JSONString("priority"),
			}, nil)
		case "flex":
			return util.MutateTopLevelObject(body, map[string][]byte{
				"service_tier": util.JSONString("flex"),
			}, nil)
		case "ultrafast", "auto", "default", "scale":
			return util.MutateTopLevelObject(body, map[string][]byte{
				"service_tier": util.JSONString(clientTier),
			}, nil)
		default:
			return util.MutateTopLevelObject(body, nil, []string{"service_tier"})
		}

	case "drop":
		// Completely filter / strip service_tier
		return util.MutateTopLevelObject(body, nil, []string{"service_tier"})

	default:
		// Default / auto / empty: standard safe behavior (strip service_tier from outbound body
		// unless explicitly enabled, avoiding unexpected charges or 400 errors from upstream).
		return util.MutateTopLevelObject(body, nil, []string{"service_tier"})
	}
}

// Codex OAuth routes by both model and tier. Derive this gateway-owned hint
// from the final outbound body, after custom headers, so policy rewrites cannot
// disagree with a stale client/account hint. Subscription labels grant no access.
func applyCodexServiceTierRoutingHint(headers http.Header, body []byte, auth *cliproxyauth.Auth) {
	for key := range headers {
		if strings.EqualFold(key, "X-Codex-Routing-Hint") {
			delete(headers, key)
		}
	}
	if auth == nil || strings.TrimSpace(auth.Attributes["api_key"]) != "" {
		return
	}
	model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if model == "" || strings.ContainsAny(model, ";=") || !httpguts.ValidHeaderFieldValue(model) {
		return
	}
	hint := "model=" + model
	switch tier := gjson.GetBytes(body, "service_tier").String(); tier {
	case "priority", "ultrafast", "flex":
		hint += ";tier=" + tier
	}
	headers.Set("X-Codex-Routing-Hint", hint)
}
