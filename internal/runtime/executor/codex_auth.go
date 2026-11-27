package executor

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/codexclientver"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

const (
	// Fallback identity for requests that bypass the fingerprint resolver. Kept in
	// sync with config.DefaultCodexFingerprint* so both paths present the same
	// official Codex CLI identity; see that block for why `codex-tui` was dropped.
	codexUserAgent  = config.DefaultCodexFingerprintUserAgent
	codexOriginator = config.DefaultCodexFingerprintOriginator
)

func applyCodexHeaders(r *http.Request, cfg *config.Config, auth *cliproxyauth.Auth, token string, stream bool) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)

	var ginHeaders http.Header
	if ginCtx, ok := r.Context().Value(util.ContextKeyGin).(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		ginHeaders = ginCtx.Request.Header
	}

	fp, fingerprintEnabled := codexIdentityFingerprint(cfg, auth, r.Context())
	if ginHeaders != nil && !fingerprintEnabled {
		// Align with upstream: if the client sent Codex beta features, preserve them.
		if v := strings.TrimSpace(ginHeaders.Get("X-Codex-Beta-Features")); v != "" {
			r.Header.Set("X-Codex-Beta-Features", v)
		}
	}
	// Align with upstream: only propagate these from the client when present.
	misc.EnsureHeader(r.Header, ginHeaders, "Version", "")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Codex-Turn-Metadata", "")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Client-Request-Id", "")

	if fingerprintEnabled {
		applyCodexIdentityFingerprintHeaders(r.Header, fp, false)
	} else {
		misc.EnsureHeader(r.Header, ginHeaders, "User-Agent", codexUserAgent)
	}

	// Device fingerprint convergence runs after the client headers above have
	// been copied in, so it rewrites the values that actually reach upstream.
	// The executor resolves the id set before translating the body and stores it
	// on the context; requests that never pass through that path (token probes,
	// PrepareRequest) resolve their own set here.
	convergedIDs := codexConvergedIDsFromContext(r.Context())
	if convergedIDs == nil {
		convergedIDs = resolveCodexConvergedIDs(cfg, auth, ginHeaders)
	}
	applyCodexConvergenceHeaders(r.Header, convergedIDs, ginHeaders)

	// Upstream codex-tui behavior: only attach Session_id when the UA indicates a desktop client.
	if strings.Contains(r.Header.Get("User-Agent"), "Mac OS") && strings.TrimSpace(r.Header.Get("Session_id")) == "" {
		r.Header.Set("Session_id", uuid.NewString())
	}

	if stream {
		r.Header.Set("Accept", "text/event-stream")
	} else {
		r.Header.Set("Accept", "application/json")
	}
	r.Header.Set("Connection", "Keep-Alive")

	isAPIKey := false
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["api_key"]); v != "" {
			isAPIKey = true
		}
	}

	originatorFromClient := ""
	if ginHeaders != nil {
		originatorFromClient = strings.TrimSpace(ginHeaders.Get("Originator"))
	}
	if originatorFromClient != "" {
		r.Header.Set("Originator", originatorFromClient)
	} else if !isAPIKey {
		if fingerprintEnabled {
			r.Header.Set("Originator", fp.Originator)
		} else {
			r.Header.Set("Originator", codexOriginator)
		}
	}
	if !isAPIKey {
		if auth != nil && auth.Metadata != nil {
			if accountID, ok := auth.Metadata["account_id"].(string); ok {
				r.Header.Set("Chatgpt-Account-Id", accountID)
			}
		}
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(r, attrs)
	if fingerprintEnabled {
		applyCodexIdentityFingerprintHeaders(r.Header, fp, false)
		if !isAPIKey {
			if originator := strings.TrimSpace(fp.Originator); originator != "" {
				r.Header.Set("Originator", originator)
			} else {
				r.Header.Del("Originator")
			}
		}
	}
	if !isAPIKey {
		raiseCodexPresentedClientVersion(r.Header, codexRequestModel(r.Context()))
	}
}

type codexRequestModelKey struct{}

// withCodexRequestModel records the model a Codex call is about to send, so the
// outbound client version can meet that model's manifest minimum.
func withCodexRequestModel(ctx context.Context, model string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return ctx
	}
	return context.WithValue(ctx, codexRequestModelKey{}, model)
}

func codexRequestModel(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	model, _ := ctx.Value(codexRequestModelKey{}).(string)
	return strings.TrimSpace(model)
}

// raiseCodexPresentedClientVersion lifts Version and the version inside
// User-Agent up to what this model requires. A client older than the
// manifest minimum is rejected even when the model is listed. A newer
// identity is left alone. An empty requirement leaves the headers as built,
// which is what tests and a process that has not resolved a version yet see.
func raiseCodexPresentedClientVersion(headers http.Header, model string) {
	if headers == nil {
		return
	}
	required := codexclientver.Required(model)
	if required == "" {
		return
	}
	current := strings.TrimSpace(headers.Get("Version"))
	if current == "" {
		current = codexVersionFromUserAgent(headers.Get("User-Agent"))
	}
	if current != "" && compareCodexClientVersion(current, required) >= 0 {
		return
	}
	headers.Set("Version", required)
	ua := headers.Get("User-Agent")
	if ua == "" {
		return
	}
	headers.Set("User-Agent", replaceCodexVersionInUserAgent(ua, required))
}

func replaceCodexVersionInUserAgent(ua, version string) string {
	lower := strings.ToLower(ua)
	for _, prefix := range []string{"codex_cli_rs/", "codex-tui/", "codex-cli/", "codex desktop/"} {
		idx := strings.Index(lower, prefix)
		if idx < 0 {
			continue
		}
		start := idx + len(prefix)
		end := start
		for end < len(ua) {
			c := ua[end]
			if (c >= '0' && c <= '9') || c == '.' {
				end++
				continue
			}
			break
		}
		if end > start {
			return ua[:start] + version + ua[end:]
		}
	}
	return "codex_cli_rs/" + version
}

func codexCreds(a *cliproxyauth.Auth) (apiKey, baseURL string) {
	if a == nil {
		return "", ""
	}
	if a.Attributes != nil {
		apiKey = a.Attributes["api_key"]
		baseURL = a.Attributes["base_url"]
	}
	if apiKey == "" && a.Metadata != nil {
		if v, ok := a.Metadata["access_token"].(string); ok {
			apiKey = v
		}
	}
	return
}

func (e *CodexExecutor) resolveCodexConfig(auth *cliproxyauth.Auth) *config.CodexKey {
	if auth == nil || e.cfg == nil {
		return nil
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range e.cfg.CodexKey {
		entry := &e.cfg.CodexKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if attrKey != "" && attrBase != "" {
			if strings.EqualFold(cfgKey, attrKey) && strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	if attrKey != "" {
		for i := range e.cfg.CodexKey {
			entry := &e.cfg.CodexKey[i]
			if strings.EqualFold(strings.TrimSpace(entry.APIKey), attrKey) {
				return entry
			}
		}
	}
	return nil
}

// codexAuthClientIdentity resolves the User-Agent and Originator pair for
// auth.openai.com requests from the same fingerprint resolver used by Codex
// inference traffic, keeping one account's auth and inference identities aligned.
func codexAuthClientIdentity(cfg *config.Config, auth *cliproxyauth.Auth, ctx context.Context) (userAgent, originator string) {
	if fp, enabled := codexIdentityFingerprint(cfg, auth, ctx); enabled {
		userAgent = strings.TrimSpace(fp.UserAgent)
		originator = strings.TrimSpace(fp.Originator)
	}
	if userAgent == "" {
		userAgent = codexUserAgent
	}
	if originator == "" {
		originator = codexOriginator
	}
	return userAgent, originator
}
