package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func (e *AntigravityExecutor) buildRequest(ctx context.Context, auth *cliproxyauth.Auth, token, modelName string, payload []byte, stream bool, alt, baseURL string) (*http.Request, []byte, error) {
	if token == "" {
		return nil, nil, statusErr{code: http.StatusUnauthorized, msg: "missing access token"}
	}

	base := strings.TrimSuffix(baseURL, "/")
	if base == "" {
		base = buildBaseURL(auth)
	}
	path := antigravityGeneratePath
	if stream {
		path = antigravityStreamPath
	}
	var requestURL strings.Builder
	requestURL.WriteString(base)
	requestURL.WriteString(path)
	if stream {
		if alt != "" {
			requestURL.WriteString("?$alt=")
			requestURL.WriteString(url.QueryEscape(alt))
		} else {
			requestURL.WriteString("?alt=sse")
		}
	} else if alt != "" {
		requestURL.WriteString("?$alt=")
		requestURL.WriteString(url.QueryEscape(alt))
	}

	projectID := ""
	if auth != nil && auth.Metadata != nil {
		if pid, ok := auth.Metadata["project_id"].(string); ok {
			projectID = strings.TrimSpace(pid)
		}
	}
	payload = geminiToAntigravity(modelName, payload, projectID)
	payload, _ = sjson.SetBytes(payload, "model", modelName)

	useAntigravitySchema := strings.Contains(modelName, "claude") || strings.Contains(modelName, "gemini-3-pro-high")
	payloadStr := string(payload)

	// The upstream accepts only "user" and "model" here and rejects anything
	// else with a bare 400 INVALID_ARGUMENT naming no field. Claude Code sends
	// "system" turns inside `messages` in addition to the top-level `system`
	// field, and the Claude translator forwards any role it does not recognise
	// unchanged, so those reached the upstream verbatim and failed the request.
	//
	// Normalising here rather than in the translator covers every entrypoint
	// format at once: this is the last point all of them pass through.
	payloadStr = util.NormalizeGeminiContentRoles(payloadStr, "request.contents")
	paths := make([]string, 0)
	util.Walk(gjson.Parse(payloadStr), "", "parametersJsonSchema", &paths)
	for _, p := range paths {
		payloadStr, _ = util.RenameKey(payloadStr, p, p[:len(p)-len("parametersJsonSchema")]+"parameters")
	}

	if useAntigravitySchema {
		payloadStr = util.CleanJSONSchemaForAntigravity(payloadStr)
	} else {
		payloadStr = util.CleanJSONSchemaForGemini(payloadStr)
	}

	if useAntigravitySchema {
		systemInstructionPartsResult := gjson.Get(payloadStr, "request.systemInstruction.parts")
		payloadStr, _ = sjson.Set(payloadStr, "request.systemInstruction.role", "user")
		payloadStr, _ = sjson.Set(payloadStr, "request.systemInstruction.parts.0.text", systemInstruction)
		payloadStr, _ = sjson.Set(payloadStr, "request.systemInstruction.parts.1.text", fmt.Sprintf("Please ignore following [ignore]%s[/ignore]", systemInstruction))

		if systemInstructionPartsResult.Exists() && systemInstructionPartsResult.IsArray() {
			for _, partResult := range systemInstructionPartsResult.Array() {
				payloadStr, _ = sjson.SetRaw(payloadStr, "request.systemInstruction.parts.-1", partResult.Raw)
			}
		}
	}

	if strings.Contains(modelName, "claude") {
		payloadStr, _ = sjson.Set(payloadStr, "request.toolConfig.functionCallingConfig.mode", "VALIDATED")
	} else {
		payloadStr, _ = sjson.Delete(payloadStr, "request.generationConfig.maxOutputTokens")
	}

	payloadBytes := e.obfuscateSensitiveWords([]byte(payloadStr))
	payloadStr = string(payloadBytes)

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, requestURL.String(), strings.NewReader(payloadStr))
	if errReq != nil {
		return nil, nil, errReq
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("User-Agent", resolveUserAgent(auth))
	if e != nil && e.cfg != nil {
		// Only apply the fingerprint when the feature is on: with it off the
		// resolver still hands back the built-in template, and applying that
		// would overwrite an identity the credential carries of its own.
		if fp, _, enabled := antigravityIdentityFingerprint(e.cfg, auth, ctx); enabled {
			applyAntigravityIdentityFingerprintHeaders(httpReq.Header, fp)
		}
	}
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}
	if host := resolveHost(base); host != "" {
		httpReq.Host = host
	}
	return httpReq, []byte(payloadStr), nil
}

func (e *AntigravityExecutor) buildCountTokensRequest(ctx context.Context, auth *cliproxyauth.Auth, token string, payload []byte, alt, baseURL string) (*http.Request, []byte, error) {
	if strings.TrimSpace(token) == "" {
		return nil, nil, statusErr{code: http.StatusUnauthorized, msg: "missing access token"}
	}

	base := strings.TrimSuffix(baseURL, "/")
	if base == "" {
		base = buildBaseURL(auth)
	}

	var requestURL strings.Builder
	requestURL.WriteString(base)
	requestURL.WriteString(antigravityCountTokensPath)
	if alt != "" {
		requestURL.WriteString("?$alt=")
		requestURL.WriteString(url.QueryEscape(alt))
	}

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, requestURL.String(), strings.NewReader(string(payload)))
	if errReq != nil {
		return nil, nil, errReq
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("User-Agent", resolveUserAgent(auth))
	httpReq.Header.Set("Accept", "application/json")
	if host := resolveHost(base); host != "" {
		httpReq.Host = host
	}
	return httpReq, payload, nil
}

func buildBaseURL(auth *cliproxyauth.Auth) string {
	if baseURLs := antigravityBaseURLFallbackOrder(auth); len(baseURLs) > 0 {
		return baseURLs[0]
	}
	return antigravityBaseURLDaily
}

func resolveHost(base string) string {
	parsed, errParse := url.Parse(base)
	if errParse != nil {
		return ""
	}
	if parsed.Host != "" {
		return parsed.Host
	}
	return strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
}

func resolveUserAgent(auth *cliproxyauth.Auth) string {
	if auth != nil {
		if auth.Attributes != nil {
			if ua := strings.TrimSpace(auth.Attributes["user_agent"]); ua != "" {
				return ua
			}
		}
		if auth.Metadata != nil {
			if ua, ok := auth.Metadata["user_agent"].(string); ok && strings.TrimSpace(ua) != "" {
				return strings.TrimSpace(ua)
			}
		}
	}
	return defaultAntigravityAgent
}

func antigravityRetryAttempts(auth *cliproxyauth.Auth, cfg *config.Config) int {
	retry := 0
	if cfg != nil {
		retry = cfg.RequestRetry
	}
	if auth != nil {
		if override, ok := auth.RequestRetryOverride(); ok {
			retry = override
		}
	}
	if retry < 0 {
		retry = 0
	}
	attempts := retry + 1
	if attempts < 1 {
		return 1
	}
	return attempts
}

func antigravityShouldRetryNoCapacity(statusCode int, body []byte) bool {
	if statusCode != http.StatusServiceUnavailable {
		return false
	}
	if len(body) == 0 {
		return false
	}
	msg := strings.ToLower(string(body))
	return strings.Contains(msg, "no capacity available")
}

func antigravityNoCapacityRetryDelay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	delay := time.Duration(attempt+1) * 250 * time.Millisecond
	if delay > 2*time.Second {
		delay = 2 * time.Second
	}
	return delay
}

func antigravityWait(ctx context.Context, wait time.Duration) error {
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func antigravityBaseURLFallbackOrder(auth *cliproxyauth.Auth) []string {
	if base := resolveCustomAntigravityBaseURL(auth); base != "" {
		return []string{base}
	}
	// Sandbox first: the non-sandbox daily host throttles far more readily, and
	// every 429 it returns costs a retry and risks being read as an exhausted
	// quota. Production logs showed the daily host rate-limiting requests that
	// the sandbox host then served without complaint.
	return []string{
		antigravitySandboxBaseURLDaily,
		antigravityBaseURLDaily,
		// antigravityBaseURLProd,
	}
}

func resolveCustomAntigravityBaseURL(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["base_url"]); v != "" {
			return strings.TrimSuffix(v, "/")
		}
	}
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["base_url"].(string); ok {
			v = strings.TrimSpace(v)
			if v != "" {
				return strings.TrimSuffix(v, "/")
			}
		}
	}
	return ""
}
