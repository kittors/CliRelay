package management

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	internalclaude "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/claude"
	internalxai "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/xai"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	managementauthfiles "github.com/router-for-me/CLIProxyAPI/v6/internal/management/authfiles"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/credentialimport"
	antigravityprovider "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/providers/antigravity"
	claudeprovider "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/providers/claude"
	codexprovider "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/providers/codex"
	xaiprovider "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/providers/xai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// Adding an account from a credential the operator already holds.
//
// Besides signing in through the browser, an account can be added from a
// claude.ai sessionKey cookie, a Codex CLI or Antigravity refresh token, or a
// Grok web SSO cookie. Each import hands the credential to the upstream for the
// tokens a browser login would have produced, then saves the account under the
// caller's tenant and bound to the chosen proxy, so the result is
// indistinguishable from one added by signing in.
//
// The panel posts one request per credential: a batch then shows progress row
// by row, and a failed row is retried on its own.

// Import provider names, matching the URL segment under /oauth-import/.
const (
	credentialImportAnthropicSession        = "anthropic-session"
	credentialImportCodexRefreshToken       = "codex-refresh-token"
	credentialImportAntigravityRefreshToken = "antigravity-refresh-token"
	credentialImportXAISSO                  = "xai-sso"
)

const (
	credentialImportBodyLimit = 64 << 10
	// credentialImportTimeout bounds the slowest exchange: Grok's device grant
	// polls for up to ~75s before the user code expires.
	credentialImportTimeout = 2 * time.Minute
)

// Codes the panel keys its messages on. A credential problem answers 422, never
// 401: the client treats a 401 from the management API as its own session
// expiring and would sign the operator out mid-import.
const (
	importCodeRequestInvalid      = "request_invalid"
	importCodeCredentialRequired  = "credential_required"
	importCodeCredentialMalformed = "credential_unrecognized"
	importCodeCredentialInvalid   = "credential_invalid"
	importCodeNoOrganization      = "no_organization"
	importCodeAuthorizationDenied = "authorization_denied"
	importCodeUpstreamBlocked     = "upstream_blocked"
	importCodeUpstreamTimeout     = "upstream_timeout"
	importCodeUpstreamError       = "upstream_error"
	importCodeSaveFailed          = "save_failed"
)

type credentialImportRequest struct {
	Credential string `json:"credential"`
	ProxyID    string `json:"proxy_id"`
	// UsingAPI only matters to Grok: bill api.x.ai credit instead of the Grok
	// Build plan, mirroring the browser login's toggle.
	UsingAPI bool `json:"using_api"`
}

// credentialImportInput is what an exchange receives after validation.
type credentialImportInput struct {
	Credential string
	ProxyURL   string
	UsingAPI   bool
}

// credentialImporter turns one credential into an account record.
type credentialImporter func(ctx context.Context, cfg *config.Config, in credentialImportInput) (*coreauth.Auth, credentialimport.Details, error)

// credentialImportKind binds a provider's credential normaliser to its exchange.
type credentialImportKind struct {
	// normalize pulls the credential out of what was pasted and returns "" when
	// it cannot be one, so a wrong field is reported before any upstream call.
	normalize func(string) string
	exchange  credentialImporter
}

var credentialImportKinds = map[string]credentialImportKind{
	credentialImportAnthropicSession: {
		normalize: internalclaude.NormalizeSessionKey,
		exchange: func(ctx context.Context, cfg *config.Config, in credentialImportInput) (*coreauth.Auth, credentialimport.Details, error) {
			return claudeprovider.ImportSessionKey(ctx, claudeprovider.SessionImportOptions{
				Config:     cfg,
				ProxyURL:   in.ProxyURL,
				SessionKey: in.Credential,
			})
		},
	},
	credentialImportCodexRefreshToken: {
		normalize: strings.TrimSpace,
		exchange: func(ctx context.Context, cfg *config.Config, in credentialImportInput) (*coreauth.Auth, credentialimport.Details, error) {
			return codexprovider.ImportRefreshToken(ctx, codexprovider.RefreshTokenImportOptions{
				Config:       cfg,
				ProxyURL:     in.ProxyURL,
				RefreshToken: in.Credential,
			})
		},
	},
	credentialImportAntigravityRefreshToken: {
		normalize: strings.TrimSpace,
		exchange: func(ctx context.Context, cfg *config.Config, in credentialImportInput) (*coreauth.Auth, credentialimport.Details, error) {
			return antigravityprovider.ImportRefreshToken(ctx, antigravityprovider.RefreshTokenImportOptions{
				Config:       cfg,
				ProxyURL:     in.ProxyURL,
				RefreshToken: in.Credential,
			})
		},
	},
	credentialImportXAISSO: {
		normalize: internalxai.NormalizeSSOToken,
		exchange: func(ctx context.Context, cfg *config.Config, in credentialImportInput) (*coreauth.Auth, credentialimport.Details, error) {
			return xaiprovider.ImportSSO(ctx, xaiprovider.SSOImportOptions{
				Config:   cfg,
				ProxyURL: in.ProxyURL,
				SSOToken: in.Credential,
				UsingAPI: in.UsingAPI,
			})
		},
	},
}

// credentialImportKind resolves a provider's import. Tests set h.importKinds to
// drive the handler with a canned exchange; otherwise the package defaults,
// which build real upstream clients, are used.
func (h *Handler) credentialImportKind(provider string) (credentialImportKind, bool) {
	if h != nil && h.importKinds != nil {
		kind, ok := h.importKinds[provider]
		return kind, ok
	}
	kind, ok := credentialImportKinds[provider]
	return kind, ok
}

// ImportAnthropicSession adds a Claude account from a claude.ai sessionKey.
func (h *Handler) ImportAnthropicSession(c *gin.Context) {
	h.importCredential(c, credentialImportAnthropicSession)
}

// ImportCodexRefreshToken adds a Codex account from a CLI refresh token.
func (h *Handler) ImportCodexRefreshToken(c *gin.Context) {
	h.importCredential(c, credentialImportCodexRefreshToken)
}

// ImportAntigravityRefreshToken adds an Antigravity account from a refresh token.
func (h *Handler) ImportAntigravityRefreshToken(c *gin.Context) {
	h.importCredential(c, credentialImportAntigravityRefreshToken)
}

// ImportXAISSO adds a Grok account from a grok.com / x.ai web SSO cookie.
func (h *Handler) ImportXAISSO(c *gin.Context) {
	h.importCredential(c, credentialImportXAISSO)
}

func (h *Handler) importCredential(c *gin.Context, provider string) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "error", "error": "config unavailable"})
		return
	}
	kind, ok := h.credentialImportKind(provider)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": "unknown import provider"})
		return
	}

	if c.Request != nil && c.Request.Body != nil && c.Writer != nil {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, credentialImportBodyLimit)
	}
	var payload credentialImportRequest
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "code": importCodeRequestInvalid, "error": "invalid request body"})
		return
	}
	if strings.TrimSpace(payload.Credential) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "code": importCodeCredentialRequired, "error": "credential is required"})
		return
	}
	// Reject a credential pasted into the wrong field before dialling any
	// upstream: a Grok cookie sent to the Claude import can never succeed, and
	// saying so locally is faster and leaks nothing.
	credential := kind.normalize(payload.Credential)
	if credential == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"status": "error", "code": importCodeCredentialMalformed, "error": "the pasted value is not a credential this import accepts"})
		return
	}

	// The record is saved by this request (unlike the browser flow, which saves
	// from a callback goroutine), so the tenant/proxy binding is read straight
	// from the request here.
	tenantID := effectiveTenantID(c)
	proxyURL := h.cfg.ResolveProxyURL(strings.TrimSpace(payload.ProxyID), "")

	ctx, cancel := context.WithTimeout(requestAuthContext(c), credentialImportTimeout)
	defer cancel()

	record, details, err := kind.exchange(ctx, h.cfg, credentialImportInput{
		Credential: credential,
		ProxyURL:   proxyURL,
		UsingAPI:   payload.UsingAPI,
	})
	if err != nil {
		h.respondCredentialImportError(c, provider, err, payload.Credential, credential)
		return
	}

	managementauthfiles.BindProxyID(record, strings.TrimSpace(payload.ProxyID))
	savedPath, err := h.saveTenantTokenRecord(ctx, tenantID, record)
	if err != nil {
		log.WithError(err).WithField("provider", provider).Error("failed to save imported credential")
		c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "code": importCodeSaveFailed, "error": "failed to save the account"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":       "ok",
		"saved_path":   savedPath,
		"provider":     record.Provider,
		"email":        details.Email,
		"organization": details.Organization,
		"plan":         details.Plan,
		"label":        record.Label,
	})
}

// respondCredentialImportError maps an import failure to a status, a stable code
// and a message safe to show. The upstream's own text is masked for the secrets
// the import handled (an upstream may echo what it was sent) before it reaches
// the response.
func (h *Handler) respondCredentialImportError(c *gin.Context, provider string, err error, pasted, credential string) {
	message := credentialimport.SafeMessage(err, pasted, credential)
	var status int
	var code string
	switch {
	case errors.Is(err, credentialimport.ErrInvalidCredential):
		status, code = http.StatusUnprocessableEntity, importCodeCredentialInvalid
	case errors.Is(err, credentialimport.ErrNoOrganization):
		status, code = http.StatusUnprocessableEntity, importCodeNoOrganization
	case errors.Is(err, credentialimport.ErrDenied):
		status, code = http.StatusUnprocessableEntity, importCodeAuthorizationDenied
	case errors.Is(err, credentialimport.ErrBlocked):
		// The upstream refused the request itself (a challenge), which is about
		// our egress, not the credential; 502 keeps it out of the "bad
		// credential" bucket the panel shows the operator.
		status, code = http.StatusBadGateway, importCodeUpstreamBlocked
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		status, code = http.StatusGatewayTimeout, importCodeUpstreamTimeout
	default:
		status, code = http.StatusBadGateway, importCodeUpstreamError
	}
	if status >= http.StatusInternalServerError {
		log.WithError(err).WithField("provider", provider).Warn("credential import upstream failure")
	}
	c.JSON(status, gin.H{"status": "error", "code": code, "error": message})
}
