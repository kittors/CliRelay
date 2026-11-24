package management

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	claudeauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	managementapitools "github.com/router-for-me/CLIProxyAPI/v6/internal/management/apitools"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

const defaultAPICallTimeout = 60 * time.Second

const (
	managementAPICallResponseLimit    int64 = 4 << 20
	managementOAuthTokenResponseLimit int64 = 64 << 10
)

var geminiOAuthScopes = []string{
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
}

var antigravityOAuthTokenURL = "https://oauth2.googleapis.com/token"

type claudeOAuthRefresher = managementapitools.ClaudeOAuthRefresher

var newClaudeOAuthRefresher = func(cfg *config.Config) claudeOAuthRefresher {
	if cfg == nil {
		cfg = &config.Config{}
	}
	return claudeauth.NewClaudeAuth(cfg)
}

const kimiOAuthClientID = "17e5f671-d194-4dfb-9706-5516cb48c098"
const kimiOAuthTokenURL = "https://auth.kimi.com/api/oauth/token"

type APIToolsHandler struct {
	*Handler
}

type apiCallRequest = managementapitools.APICallRequest
type apiCallResponse = managementapitools.APICallResponse

func (h *Handler) APITools() *APIToolsHandler {
	if h == nil {
		return nil
	}
	return &APIToolsHandler{Handler: h}
}

func (h *APIToolsHandler) service() *managementapitools.Service {
	return h.serviceForTenant(identity.SystemTenantID)
}

func (h *APIToolsHandler) serviceForTenant(tenantID string) *managementapitools.Service {
	deps := managementapitools.Dependencies{
		DefaultAPICallTimeout:             defaultAPICallTimeout,
		ManagementAPICallResponseLimit:    managementAPICallResponseLimit,
		ManagementOAuthTokenResponseLimit: managementOAuthTokenResponseLimit,
		GeminiOAuthScopes:                 append([]string(nil), geminiOAuthScopes...),
		AntigravityOAuthTokenURL:          antigravityOAuthTokenURL,
		NewClaudeOAuthRefresher: func(cfg *config.Config) managementapitools.ClaudeOAuthRefresher {
			return newClaudeOAuthRefresher(cfg)
		},
		KimiOAuthClientID: kimiOAuthClientID,
		KimiOAuthTokenURL: kimiOAuthTokenURL,
	}
	if h == nil {
		return managementapitools.NewForTenant(tenantID, nil, nil, deps)
	}
	cfg := usage.BuildTenantRuntimeConfig(h.cfg, tenantID)
	return managementapitools.NewForTenant(tenantID, &cfg, h.authManager, deps)
}

// authByIndex remains on the root handler as a narrow compatibility bridge for
// management flows that have not yet switched to the APITools transport.
func (h *Handler) authByIndex(authIndex string) *coreauth.Auth {
	if h == nil {
		return nil
	}
	return h.APITools().authByIndex(authIndex)
}

func (h *Handler) authByIndexForTenant(tenantID, authIndex string) *coreauth.Auth {
	if h == nil || h.authManager == nil {
		return nil
	}
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return nil
	}
	for _, auth := range h.authManager.ListForTenant(tenantID) {
		if auth != nil && auth.Index == authIndex {
			return auth
		}
	}
	return nil
}

// APICall makes a generic HTTP request on behalf of the management API caller.
func (h *APIToolsHandler) APICall(c *gin.Context) {
	var body apiCallRequest
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil {
		writeInvalidBodyError(c, errBindJSON)
		return
	}
	status, payload := h.serviceForTenant(effectiveTenantID(c)).APICall(c.Request.Context(), body)
	c.JSON(status, payload)
}

func firstNonEmptyString(values ...*string) string {
	return managementapitools.FirstNonEmptyString(values...)
}

func tokenValueForAuth(auth *coreauth.Auth) string {
	return managementapitools.TokenValueForAuth(auth)
}

func tokenValueFromMetadata(metadata map[string]any) string {
	return managementapitools.TokenValueFromMetadata(metadata)
}

func (h *APIToolsHandler) resolveTokenForAuth(ctx context.Context, auth *coreauth.Auth) (string, error) {
	return h.service().ResolveTokenForAuth(ctx, auth)
}

func (h *APIToolsHandler) authByIndex(authIndex string) *coreauth.Auth {
	return h.service().AuthByIndex(authIndex)
}

func (h *APIToolsHandler) apiCallTransport(auth *coreauth.Auth) http.RoundTripper {
	return h.service().APICallTransport(auth)
}

func normalizedStringSlicesEqual(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if strings.TrimSpace(strings.ToLower(a[i])) != strings.TrimSpace(strings.ToLower(b[i])) {
			return false
		}
	}
	return true
}
