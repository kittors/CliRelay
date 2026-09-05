package aiaccountstatus

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	managementapitools "github.com/router-for-me/CLIProxyAPI/v6/internal/management/apitools"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// ProbeResult is the typed output of a provider-specific upstream status probe.
type ProbeResult struct {
	PlanType               string
	SubscriptionStartedAt  *time.Time
	SubscriptionExpiresAt  *time.Time
	SubscriptionSource     string
	Quotas                 []usage.QuotaWindowDTO
	ResetCreditCount       *int64
	ResetCreditExpirations []string
	Health                 string
	Unsupported            bool
	UnsupportedReason      string
}

func probeAuth(ctx context.Context, svc *managementapitools.Service, cfg *config.Config, auth *coreauth.Auth) (ProbeResult, error) {
	if auth == nil {
		return ProbeResult{}, fmt.Errorf("auth is nil")
	}
	provider := normalizeProvider(auth.Provider)
	switch provider {
	case "codex":
		return probeCodex(ctx, svc, auth)
	case "claude", "anthropic":
		if !isClaudeOAuthLike(auth) {
			return ProbeResult{Unsupported: true, UnsupportedReason: "claude api-key accounts have no oauth usage probe"}, nil
		}
		return probeClaude(ctx, svc, auth)
	case "gemini-cli":
		return probeGeminiCLI(ctx, svc, auth)
	case "kimi":
		return probeKimi(ctx, svc, auth)
	case "kiro":
		return probeKiro(ctx, svc, auth)
	case "xai", "grok":
		return probeXAI(ctx, svc, auth)
	case "antigravity":
		return probeAntigravity(ctx, svc, auth)
	default:
		return ProbeResult{Unsupported: true, UnsupportedReason: "provider " + provider + " has no server-side status probe"}, nil
	}
}

func normalizeProvider(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	p = strings.ReplaceAll(p, "_", "-")
	if p == "x-ai" || p == "grok" {
		return "xai"
	}
	return p
}

func isClaudeOAuthLike(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	accountType, _ := auth.AccountInfo()
	accountType = strings.ToLower(strings.TrimSpace(accountType))
	if accountType == "api-key" || accountType == "apikey" || accountType == "api_key" {
		return false
	}
	return true
}

func doAuthGET(ctx context.Context, svc *managementapitools.Service, auth *coreauth.Auth, url string, headers map[string]string, mutate func(*http.Request)) ([]byte, error) {
	return doAuthRequest(ctx, svc, auth, http.MethodGet, url, headers, "", mutate)
}

func doAuthPOST(ctx context.Context, svc *managementapitools.Service, auth *coreauth.Auth, url string, headers map[string]string, body string) ([]byte, error) {
	raw, _, err := doAuthPOSTStatus(ctx, svc, auth, url, headers, body)
	return raw, err
}

// doAuthPOSTStatus is doAuthPOST plus the upstream status code. Probes that must
// branch on the status — antigravity retries a 403 without the project field
// before believing the account is actually forbidden — cannot recover it from the
// error string.
func doAuthPOSTStatus(ctx context.Context, svc *managementapitools.Service, auth *coreauth.Auth, url string, headers map[string]string, body string) ([]byte, int, error) {
	return doAuthRequestStatus(ctx, svc, auth, http.MethodPost, url, headers, body, nil)
}

func doAuthRequest(ctx context.Context, svc *managementapitools.Service, auth *coreauth.Auth, method, urlStr string, headers map[string]string, body string, mutate func(*http.Request)) ([]byte, error) {
	raw, _, err := doAuthRequestStatus(ctx, svc, auth, method, urlStr, headers, body, mutate)
	return raw, err
}

func doAuthRequestStatus(ctx context.Context, svc *managementapitools.Service, auth *coreauth.Auth, method, urlStr string, headers map[string]string, body string, mutate func(*http.Request)) ([]byte, int, error) {
	if svc == nil {
		return nil, 0, fmt.Errorf("api tools unavailable")
	}
	token, err := svc.ResolveTokenForAuth(ctx, auth)
	if err != nil || strings.TrimSpace(token) == "" {
		return nil, 0, fmt.Errorf("token unavailable")
	}
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, urlStr, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for k, v := range headers {
		if strings.EqualFold(k, "Authorization") {
			// Always server-controlled bearer from resolved token.
			continue
		}
		req.Header.Set(k, strings.ReplaceAll(v, "$TOKEN$", token))
	}
	if mutate != nil {
		mutate(req)
	}
	client := util.NewHTTPClient(30 * time.Second)
	targetURL, _ := url.Parse(urlStr)
	if transport := svc.APICallTransportForURL(auth, targetURL); transport != nil {
		client.Transport = transport
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("upstream http %d", resp.StatusCode)
	}
	return raw, resp.StatusCode, nil
}

func authString(auth *coreauth.Auth, keys ...string) string {
	if auth == nil {
		return ""
	}
	if value := metadataString(auth, keys...); value != "" {
		return value
	}
	for _, key := range keys {
		if value := strings.TrimSpace(auth.Attributes[key]); value != "" {
			return value
		}
	}
	return ""
}

func metadataNestedString(auth *coreauth.Auth, parent string, keys ...string) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	raw, ok := auth.Metadata[parent]
	if !ok {
		return ""
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return ""
	}
	for _, key := range keys {
		if v, ok := m[key]; ok {
			if s, ok := v.(string); ok {
				if t := strings.TrimSpace(s); t != "" {
					return t
				}
			}
		}
	}
	return ""
}

func firstJSONResult(root gjson.Result, paths ...string) gjson.Result {
	for _, path := range paths {
		if value := root.Get(path); value.Exists() {
			return value
		}
	}
	return gjson.Result{}
}

func quotaFraction(value gjson.Result) *float64 {
	if !value.Exists() {
		return nil
	}
	raw := strings.TrimSpace(value.String())
	fraction := value.Float()
	if strings.HasSuffix(raw, "%") {
		parsed, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(raw, "%")), 64)
		if err != nil {
			return nil
		}
		fraction = parsed / 100
	}
	return &fraction
}

func parseFlexibleTime(value gjson.Result) *time.Time {
	if !value.Exists() {
		return nil
	}
	raw := strings.TrimSpace(value.String())
	if raw != "" {
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if parsed, err := time.Parse(layout, raw); err == nil {
				t := parsed.UTC()
				return &t
			}
		}
	}
	seconds := value.Float()
	if value.Type == gjson.String {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil
		}
		seconds = parsed
	}
	if seconds <= 0 {
		return nil
	}
	if seconds > 1e12 {
		seconds /= 1000
	}
	t := time.Unix(int64(seconds), int64((seconds-float64(int64(seconds)))*float64(time.Second))).UTC()
	return &t
}

func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func parseURL(raw string) *url.URL {
	u, _ := url.Parse(raw)
	return u
}
