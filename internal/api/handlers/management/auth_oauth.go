package management

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	managementauthfiles "github.com/router-for-me/CLIProxyAPI/v6/internal/management/authfiles"
	antigravityprovider "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/providers/antigravity"
	claudeprovider "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/providers/claude"
	codexprovider "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/providers/codex"
	geminicli "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/providers/geminicli"
	iflowprovider "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/providers/iflow"
	kimiprovider "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/providers/kimi"
	qwenprovider "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/providers/qwen"
	xaiprovider "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/providers/xai"
	oauthsession "github.com/router-for-me/CLIProxyAPI/v6/internal/management/oauth/session"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	anthropicCallbackPort    = 54545
	geminiCallbackPort       = 8085
	codexCallbackPort        = 1455
	oauthCallbackWaitTimeout = oauthSessionTTL
)

func isWebUIRequest(c *gin.Context) bool {
	raw := strings.TrimSpace(c.Query("is_webui"))
	if raw == "" {
		return false
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func (h *Handler) managementCallbackURL(path string) (string, error) {
	if h == nil || h.cfg == nil || h.cfg.Port <= 0 {
		return "", fmt.Errorf("server port is not configured")
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	scheme := "http"
	if h.cfg.TLS.Enable {
		scheme = "https"
	}
	return fmt.Sprintf("%s://127.0.0.1:%d%s", scheme, h.cfg.Port, path), nil
}

func (h *Handler) saveTokenRecord(ctx context.Context, record *coreauth.Auth) (string, error) {
	return h.saveTenantTokenRecord(ctx, "", record)
}

func (h *Handler) saveTenantTokenRecord(ctx context.Context, tenantID string, record *coreauth.Auth) (string, error) {
	if h == nil || h.cfg == nil || record == nil {
		return "", fmt.Errorf("auth record is unavailable")
	}
	tenantID = managementauthfiles.NormalizeTenantID(tenantID)
	if service := identity.Default(); service != nil {
		if err := service.ValidateTenantAccess(ctx, tenantID); err != nil {
			return "", err
		}
	}
	name := filepath.Base(strings.TrimSpace(record.FileName))
	if name == "." || name == "" {
		name = filepath.Base(strings.TrimSpace(record.ID))
	}
	if _, err := managementauthfiles.ValidateFileQueryName(name, true); err != nil {
		return "", err
	}
	tenantDir := managementauthfiles.TenantAuthDir(h.cfg.AuthDir, tenantID)
	if err := os.MkdirAll(tenantDir, 0o700); err != nil {
		return "", fmt.Errorf("prepare tenant auth directory: %w", err)
	}
	record.TenantID = tenantID
	record.ID = filepath.ToSlash(filepath.Join(tenantID, name))
	record.FileName = name
	if record.Attributes == nil {
		record.Attributes = make(map[string]string)
	}
	record.Attributes["path"] = managementauthfiles.TenantFilePath(h.cfg.AuthDir, tenantID, name)

	// Inherit operator-managed settings (proxy binding, tags, concurrency,
	// Codex options, etc.) from any existing credential for the same physical
	// account before persisting, and collect older filename aliases (such as a
	// previous plan-typed Codex file) to clean up once the new record is saved.
	replacementPlan := managementauthfiles.FindReauthReplacementPlan(h.authManager, tenantID, record)
	managementauthfiles.ApplyReauthOperationalInheritance(record, replacementPlan)

	// A login creates or replaces the credential outright (see
	// coreauth.WithCredentialCreate); single-node stores ignore the mark.
	createCtx := coreauth.WithCredentialCreate(ctx)
	savedPath, err := h.authFileRepository().Save(createCtx, record)
	if err != nil {
		return "", err
	}
	if h.authManager != nil {
		// Propagate WithCredentialCreate to Manager.Update so stale 401
		// cooldowns, StatusError, and old OAuth health from the previous token
		// are cleared immediately on re-login.
		runtimeCtx := coreauth.WithSkipPersist(createCtx)
		if _, exists := h.authManager.GetByID(record.ID); exists {
			_, err = h.authManager.Update(runtimeCtx, record)
		} else {
			_, err = h.authManager.Register(runtimeCtx, record)
		}
		if err != nil {
			return "", fmt.Errorf("register tenant auth record: %w", err)
		}
	}
	h.cleanupSupersededReauthAliases(ctx, tenantID, record, replacementPlan.SupersededAliases)
	return savedPath, nil
}

// oauthProxyIDFromRequest reads the proxy-pool entry chosen in the login dialog.
//
// The start request is the authoritative carrier: the record is built and saved
// by the goroutine that request spawns, so a proxy id arriving later on
// /oauth-callback would reach a closure that has already been handed its value.
// The callback panel sends the field too, which is harmless duplication rather
// than a second source of truth.
func oauthProxyIDFromRequest(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if proxyID := strings.TrimSpace(c.Query("proxy_id")); proxyID != "" {
		return proxyID
	}
	return strings.TrimSpace(c.Query("proxy-id"))
}

func (h *Handler) resolveOAuthProxyURL(c *gin.Context) string {
	proxyID := oauthProxyIDFromRequest(c)
	if proxyID == "" {
		return ""
	}
	return h.resolveTenantProxyURL(effectiveTenantID(c), proxyID)
}

// urlCredentials matches the user:password part of a URL, such as a proxy's.
var urlCredentials = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^\s/@"']+@`)

// oauthStartFailure is the body of a login that could not start: the code, and
// the cause as message, which the panel shows as "code: cause". The code alone
// hid a global proxy that refused its credentials on 2026-11-09: every Grok
// login answered "failed to generate authorization url" and nothing more, while
// the log said "socks connect tcp …: username/password authentication failed".
// The cause keeps what the operator acts on (the URL, the proxy's host:port,
// the upstream's words); credentials in any URL are left out.
func oauthStartFailure(code string, err error) gin.H {
	body := gin.H{"error": code}
	if err == nil {
		return body
	}
	if reason := strings.TrimSpace(urlCredentials.ReplaceAllString(err.Error(), "${1}***@")); reason != "" {
		body["message"] = reason
	}
	return body
}

// withErrorStatus adds the "status": "error" the iFlow login answers carry.
func withErrorStatus(body gin.H) gin.H {
	body["status"] = "error"
	return body
}

// resolveTenantProxyURL resolves a proxy ID the way the tenant's executors
// will once the credential is saved: against the tenant's own pool and the
// platform proxies shared with it. Resolving against the system pool instead
// sent a tenant's login through the system entry of the same ID, or straight
// out when there was none, so the token was minted from another address than
// the account's traffic would then use.
func (h *Handler) resolveTenantProxyURL(tenantID, proxyID string) string {
	proxyID = strings.TrimSpace(proxyID)
	if proxyID == "" || h == nil || h.cfg == nil {
		return ""
	}
	if tenantID == identity.SystemTenantID {
		return h.cfg.ResolveProxyURL(proxyID, "")
	}
	tenantCfg := usage.BuildTenantRuntimeConfig(h.cfg, tenantID)
	return tenantCfg.ResolveProxyURL(proxyID, "")
}

func (h *Handler) tenantOAuthBindings(c *gin.Context) (
	authDir string,
	saveRecord func(context.Context, *coreauth.Auth) (string, error),
	registerSession func(string, string),
	completeProvider func(string) int,
) {
	tenantID := effectiveTenantID(c)
	authDir = managementauthfiles.TenantAuthDir(h.cfg.AuthDir, tenantID)
	// The proxy the operator picked in the login dialog is bound here rather than
	// inside each provider flow. Every OAuth provider funnels its finished record
	// through this one closure, so binding at this point covers all of them and
	// leaves no provider to be forgotten when the next one is added.
	proxyID := oauthProxyIDFromRequest(c)
	saveRecord = func(ctx context.Context, record *coreauth.Auth) (string, error) {
		managementauthfiles.BindProxyID(record, proxyID)
		return h.saveTenantTokenRecord(ctx, tenantID, record)
	}
	registerSession = func(state, provider string) {
		RegisterOAuthSessionForTenant(state, provider, tenantID)
	}
	completeProvider = func(provider string) int {
		return CompleteOAuthSessionsByProviderForTenant(provider, tenantID)
	}
	return authDir, saveRecord, registerSession, completeProvider
}

func (h *Handler) RequestAnthropicToken(c *gin.Context) {
	ctx := detachedAuthContext(c)
	authDir, saveRecord, registerSession, completeProvider := h.tenantOAuthBindings(c)

	result, err := claudeprovider.StartOAuthLogin(ctx, claudeprovider.OAuthLoginOptions{
		AuthDir:  authDir,
		Config:   h.cfg,
		ProxyURL: h.resolveOAuthProxyURL(c),
		WebUI:    isWebUIRequest(c),
		// callback_mode=code: Anthropic's page shows the code to paste back, so a
		// remote panel never sends the browser to a localhost it cannot open.
		UsePlatformCallback:   strings.EqualFold(strings.TrimSpace(c.Query("callback_mode")), "code"),
		PreferredCallbackPort: anthropicCallbackPort,
		CallbackTarget:        h.managementCallbackURL,
		WaitCallback:          WaitOAuthCallbackFile,
		CallbackWaitTimeout:   oauthCallbackWaitTimeout,
		SaveRecord:            saveRecord,
		Sessions: claudeprovider.SessionCallbacks{
			Register:         registerSession,
			SetError:         SetOAuthSessionError,
			Complete:         CompleteOAuthSession,
			CompleteProvider: completeProvider,
		},
	})
	if err != nil {
		switch {
		case errors.Is(err, claudeprovider.ErrPKCEGeneration):
			log.Errorf("Failed to generate PKCE codes: %v", err)
			writeError(c, http.StatusInternalServerError, "pkce_generation_failed", "生成 Anthropic OAuth 安全校验参数（PKCE）失败，请稍后重试")
		case errors.Is(err, claudeprovider.ErrStateGeneration):
			log.Errorf("Failed to generate state parameter: %v", err)
			writeError(c, http.StatusInternalServerError, "state_generation_failed", "生成 Anthropic OAuth 登录状态标识（state）失败，请稍后重试")
		case errors.Is(err, claudeprovider.ErrAuthURL):
			log.Errorf("Failed to generate authorization URL: %v", err)
			c.JSON(http.StatusInternalServerError, oauthStartFailure("failed to generate authorization url", err))
		default:
			log.WithError(err).Error("failed to start anthropic oauth flow")
			c.JSON(http.StatusInternalServerError, oauthStartFailure("failed to generate authorization url", err))
		}
		return
	}

	c.JSON(http.StatusOK, oauthStartPayload(anthropicLoginFlow(result.AuthURL), result.AuthURL, result.State, oauthSessionExpiry(time.Now()), nil))
}

func (h *Handler) RequestGeminiCLIToken(c *gin.Context) {
	ctx := detachedAuthContext(c)
	authDir, saveRecord, registerSession, completeProvider := h.tenantOAuthBindings(c)

	result, err := geminicli.StartOAuthLogin(ctx, geminicli.OAuthLoginOptions{
		AuthDir:             authDir,
		Config:              h.cfg,
		ProjectID:           c.Query("project_id"),
		WebUI:               isWebUIRequest(c),
		CallbackPort:        geminiCallbackPort,
		CallbackTarget:      h.managementCallbackURL,
		WaitCallback:        WaitOAuthCallbackFile,
		CallbackWaitTimeout: oauthCallbackWaitTimeout,
		SaveRecord:          saveRecord,
		Sessions: geminicli.SessionCallbacks{
			Register:         registerSession,
			SetError:         SetOAuthSessionError,
			Complete:         CompleteOAuthSession,
			CompleteProvider: completeProvider,
		},
	})
	if err != nil {
		switch {
		case errors.Is(err, geminicli.ErrOAuthClientIDMissing):
			writeError(c, http.StatusBadRequest, "gemini_oauth_client_id_missing", "尚未配置 Gemini OAuth Client ID，请先在服务端配置 oauth-clients.gemini 后重试")
		case errors.Is(err, geminicli.ErrStateGeneration):
			writeError(c, http.StatusInternalServerError, "state_generation_failed", "生成 Gemini OAuth 登录状态标识（state）失败，请稍后重试")
		case errors.Is(err, geminicli.ErrCallbackUnavailable):
			log.WithError(err).Error("failed to compute gemini callback target")
			writeError(c, http.StatusInternalServerError, "callback_server_unavailable", "无法确定 Gemini OAuth 本地回调服务地址，请检查服务监听端口配置")
		case errors.Is(err, geminicli.ErrCallbackStart):
			log.WithError(err).Error("failed to start gemini callback forwarder")
			c.JSON(http.StatusInternalServerError, oauthStartFailure("failed to start callback server", err))
		default:
			log.WithError(err).Error("failed to start gemini oauth flow")
			c.JSON(http.StatusInternalServerError, oauthStartFailure("failed to start oauth flow", err))
		}
		return
	}

	c.JSON(http.StatusOK, oauthStartPayload(oauthFlowRedirect, result.AuthURL, result.State, oauthSessionExpiry(time.Now()), nil))
}

func (h *Handler) RequestCodexToken(c *gin.Context) {
	ctx := detachedAuthContext(c)
	authDir, saveRecord, registerSession, completeProvider := h.tenantOAuthBindings(c)

	result, err := codexprovider.StartOAuthLogin(ctx, codexprovider.OAuthLoginOptions{
		AuthDir:             authDir,
		Config:              h.cfg,
		ProxyURL:            h.resolveOAuthProxyURL(c),
		WebUI:               isWebUIRequest(c),
		CallbackPort:        codexCallbackPort,
		CallbackTarget:      h.managementCallbackURL,
		WaitCallback:        WaitOAuthCallbackFile,
		CallbackWaitTimeout: oauthCallbackWaitTimeout,
		SaveRecord:          saveRecord,
		Sessions: codexprovider.SessionCallbacks{
			Register:         registerSession,
			SetError:         SetOAuthSessionError,
			Complete:         CompleteOAuthSession,
			CompleteProvider: completeProvider,
		},
	})
	if err != nil {
		switch {
		case errors.Is(err, codexprovider.ErrPKCEGeneration):
			log.Errorf("Failed to generate PKCE codes: %v", err)
			writeError(c, http.StatusInternalServerError, "pkce_generation_failed", "生成 Codex OAuth 安全校验参数（PKCE）失败，请稍后重试")
		case errors.Is(err, codexprovider.ErrStateGeneration):
			log.Errorf("Failed to generate state parameter: %v", err)
			writeError(c, http.StatusInternalServerError, "state_generation_failed", "生成 Codex OAuth 登录状态标识（state）失败，请稍后重试")
		case errors.Is(err, codexprovider.ErrAuthURL):
			log.Errorf("Failed to generate authorization URL: %v", err)
			c.JSON(http.StatusInternalServerError, oauthStartFailure("failed to generate authorization url", err))
		default:
			log.WithError(err).Error("failed to start codex oauth flow")
			c.JSON(http.StatusInternalServerError, oauthStartFailure("failed to generate authorization url", err))
		}
		return
	}

	c.JSON(http.StatusOK, oauthStartPayload(oauthFlowRedirect, result.AuthURL, result.State, oauthSessionExpiry(time.Now()), nil))
}

func (h *Handler) RequestAntigravityToken(c *gin.Context) {
	ctx := detachedAuthContext(c)
	authDir, saveRecord, registerSession, completeProvider := h.tenantOAuthBindings(c)

	result, err := antigravityprovider.StartOAuthLogin(ctx, antigravityprovider.OAuthLoginOptions{
		AuthDir:             authDir,
		Config:              h.cfg,
		ProxyURL:            h.resolveOAuthProxyURL(c),
		WebUI:               isWebUIRequest(c),
		CallbackTarget:      h.managementCallbackURL,
		WaitCallback:        WaitOAuthCallbackFile,
		CallbackWaitTimeout: oauthCallbackWaitTimeout,
		SaveRecord:          saveRecord,
		Sessions: antigravityprovider.SessionCallbacks{
			Register:         registerSession,
			SetError:         SetOAuthSessionError,
			Complete:         CompleteOAuthSession,
			CompleteProvider: completeProvider,
		},
	})
	if err != nil {
		switch {
		case errors.Is(err, antigravityprovider.ErrStateGeneration):
			log.Errorf("Failed to generate state parameter: %v", err)
			writeError(c, http.StatusInternalServerError, "state_generation_failed", "生成 Antigravity OAuth 登录状态标识（state）失败，请稍后重试")
		case errors.Is(err, antigravityprovider.ErrCallbackUnavailable):
			log.WithError(err).Error("failed to compute antigravity callback target")
			writeError(c, http.StatusInternalServerError, "callback_server_unavailable", "无法确定 Antigravity OAuth 本地回调服务地址，请检查服务监听端口配置")
		case errors.Is(err, antigravityprovider.ErrCallbackStart):
			log.WithError(err).Error("failed to start antigravity callback forwarder")
			c.JSON(http.StatusInternalServerError, oauthStartFailure("failed to start callback server", err))
		case errors.Is(err, antigravityprovider.ErrOAuthClientIDMissing):
			writeError(c, http.StatusBadRequest, "antigravity_oauth_client_id_missing", "尚未配置 Antigravity OAuth Client ID，请先在服务端配置 oauth-clients.antigravity 后重试")
		default:
			log.WithError(err).Error("failed to start antigravity oauth flow")
			c.JSON(http.StatusInternalServerError, oauthStartFailure("failed to start oauth flow", err))
		}
		return
	}

	c.JSON(http.StatusOK, oauthStartPayload(oauthFlowRedirect, result.AuthURL, result.State, oauthSessionExpiry(time.Now()), nil))
}

func (h *Handler) RequestQwenToken(c *gin.Context) {
	ctx := detachedAuthContext(c)
	_, saveRecord, registerSession, _ := h.tenantOAuthBindings(c)

	result, err := qwenprovider.StartDeviceLogin(ctx, qwenprovider.DeviceLoginOptions{
		Config:     h.cfg,
		SaveRecord: saveRecord,
		Sessions: qwenprovider.SessionCallbacks{
			Register: registerSession,
			SetError: SetOAuthSessionError,
			Complete: CompleteOAuthSession,
		},
	})
	if err != nil {
		log.Errorf("Failed to generate authorization URL: %v", err)
		c.JSON(http.StatusInternalServerError, oauthStartFailure("failed to generate authorization url", err))
		return
	}

	c.JSON(http.StatusOK, oauthStartPayload(oauthFlowDevice, result.AuthURL, result.State, deviceLoginExpiry(time.Now(), result.ExpiresIn), deviceLoginExtra(result.UserCode, result.VerificationURI, result.ExpiresIn)))
}

func (h *Handler) RequestKimiToken(c *gin.Context) {
	ctx := detachedAuthContext(c)
	_, saveRecord, registerSession, completeProvider := h.tenantOAuthBindings(c)

	result, err := kimiprovider.StartDeviceLogin(ctx, kimiprovider.DeviceLoginOptions{
		Config:     h.cfg,
		SaveRecord: saveRecord,
		Sessions: kimiprovider.SessionCallbacks{
			Register:         registerSession,
			SetError:         SetOAuthSessionError,
			Complete:         CompleteOAuthSession,
			CompleteProvider: completeProvider,
		},
	})
	if err != nil {
		log.Errorf("Failed to generate authorization URL: %v", err)
		c.JSON(http.StatusInternalServerError, oauthStartFailure("failed to generate authorization url", err))
		return
	}

	c.JSON(http.StatusOK, oauthStartPayload(oauthFlowDevice, result.AuthURL, result.State, deviceLoginExpiry(time.Now(), result.ExpiresIn), deviceLoginExtra(result.UserCode, result.VerificationURI, result.ExpiresIn)))
}

func (h *Handler) RequestXAIToken(c *gin.Context) {
	ctx := detachedAuthContext(c)
	authDir, saveRecord, registerSession, completeProvider := h.tenantOAuthBindings(c)

	result, err := xaiprovider.StartOAuthLogin(ctx, xaiprovider.OAuthLoginOptions{
		AuthDir:             authDir,
		Config:              h.cfg,
		ProxyURL:            h.resolveOAuthProxyURL(c),
		WebUI:               isWebUIRequest(c),
		UsingAPI:            queryBool(c, "using_api"),
		CallbackTarget:      h.managementCallbackURL,
		WaitCallback:        WaitOAuthCallbackFile,
		CallbackWaitTimeout: oauthCallbackWaitTimeout,
		SaveRecord:          saveRecord,
		Sessions: xaiprovider.SessionCallbacks{
			Register:         registerSession,
			SetError:         SetOAuthSessionError,
			Complete:         CompleteOAuthSession,
			CompleteProvider: completeProvider,
		},
	})
	if err != nil {
		switch {
		case errors.Is(err, xaiprovider.ErrPKCEGeneration):
			log.WithError(err).Error("failed to generate xai PKCE codes")
			writeError(c, http.StatusInternalServerError, "pkce_generation_failed", "生成 xAI/Grok OAuth 安全校验参数（PKCE）失败，请稍后重试")
		case errors.Is(err, xaiprovider.ErrStateGeneration):
			log.WithError(err).Error("failed to generate xai state parameter")
			writeError(c, http.StatusInternalServerError, "state_generation_failed", "生成 xAI/Grok OAuth 登录状态标识（state）失败，请稍后重试")
		case errors.Is(err, xaiprovider.ErrCallbackUnavailable):
			log.WithError(err).Error("failed to compute xai callback target")
			writeError(c, http.StatusInternalServerError, "callback_server_unavailable", "无法确定 xAI/Grok OAuth 本地回调服务地址，请检查服务监听端口配置")
		case errors.Is(err, xaiprovider.ErrCallbackStart):
			log.WithError(err).Error("failed to start xai callback forwarder")
			c.JSON(http.StatusInternalServerError, oauthStartFailure("failed to start callback server", err))
		case errors.Is(err, xaiprovider.ErrDiscovery), errors.Is(err, xaiprovider.ErrAuthURL):
			log.WithError(err).Error("failed to start xai oauth flow")
			c.JSON(http.StatusInternalServerError, oauthStartFailure("failed to generate authorization url", err))
		default:
			log.WithError(err).Error("failed to start xai oauth flow")
			c.JSON(http.StatusInternalServerError, oauthStartFailure("failed to start oauth flow", err))
		}
		return
	}

	c.JSON(http.StatusOK, oauthStartPayload(oauthFlowRedirect, result.AuthURL, result.State, oauthSessionExpiry(time.Now()), nil))
}

func (h *Handler) RequestIFlowToken(c *gin.Context) {
	ctx := detachedAuthContext(c)
	authDir, saveRecord, registerSession, completeProvider := h.tenantOAuthBindings(c)

	result, err := iflowprovider.StartOAuthLogin(ctx, iflowprovider.OAuthLoginOptions{
		AuthDir:             authDir,
		Config:              h.cfg,
		WebUI:               isWebUIRequest(c),
		CallbackTarget:      h.managementCallbackURL,
		WaitCallback:        WaitOAuthCallbackFile,
		CallbackWaitTimeout: oauthCallbackWaitTimeout,
		SaveRecord:          saveRecord,
		Sessions: iflowprovider.SessionCallbacks{
			Register:         registerSession,
			SetError:         SetOAuthSessionError,
			Complete:         CompleteOAuthSession,
			CompleteProvider: completeProvider,
		},
	})
	if err != nil {
		switch {
		case errors.Is(err, iflowprovider.ErrCallbackUnavailable):
			log.WithError(err).Error("failed to compute iflow callback target")
			c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "code": "callback_server_unavailable", "error": "无法确定 iFlow OAuth 本地回调服务地址，请检查服务监听端口配置"})
		case errors.Is(err, iflowprovider.ErrCallbackStart):
			log.WithError(err).Error("failed to start iflow callback forwarder")
			c.JSON(http.StatusInternalServerError, withErrorStatus(oauthStartFailure("failed to start callback server", err)))
		default:
			log.WithError(err).Error("failed to start iflow oauth flow")
			c.JSON(http.StatusInternalServerError, withErrorStatus(oauthStartFailure("failed to start oauth flow", err)))
		}
		return
	}

	c.JSON(http.StatusOK, oauthStartPayload(oauthFlowRedirect, result.AuthURL, result.State, oauthSessionExpiry(time.Now()), nil))
}

func (h *Handler) RequestIFlowCookieToken(c *gin.Context) {
	ctx := requestAuthContext(c)
	authDir, saveRecord, _, _ := h.tenantOAuthBindings(c)

	// Cookie login posts its fields as JSON, so the proxy id arrives in the body
	// rather than the query string tenantOAuthBindings reads.
	var payload struct {
		Cookie  string `json:"cookie"`
		ProxyID string `json:"proxy_id"`
	}
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "code": "invalid_request_body", "error": "请求体格式无效或缺少 cookie 字段，请传入包含有效 iFlow Cookie 的 JSON"})
		return
	}
	saveWithProxy := func(ctx context.Context, record *coreauth.Auth) (string, error) {
		managementauthfiles.BindProxyID(record, payload.ProxyID)
		return saveRecord(ctx, record)
	}

	result, err := iflowprovider.AuthenticateCookie(ctx, payload.Cookie, iflowprovider.CookieLoginOptions{
		Config:     h.cfg,
		AuthDir:    authDir,
		SaveRecord: saveWithProxy,
	})
	if err != nil {
		var duplicate iflowprovider.DuplicateBXAuthError
		switch {
		case errors.Is(err, iflowprovider.ErrCookieRequired):
			c.JSON(http.StatusBadRequest, gin.H{"status": "error", "code": "missing_cookie", "error": "缺少必填参数 cookie，请粘贴完整的 iFlow 登录 Cookie"})
		case errors.Is(err, iflowprovider.ErrDuplicateCheck):
			log.WithError(err).Error("iflow cookie login failed duplicate check")
			c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "code": "duplicate_check_failed", "error": "检查重复 iFlow 凭据时发生服务端异常，请稍后重试"})
		case errors.As(err, &duplicate):
			c.JSON(http.StatusConflict, gin.H{"status": "error", "code": "duplicate_bxauth", "error": fmt.Sprintf("该 iFlow 账号（BXAuth）已存在于凭据文件 %q 中，无需重复导入", duplicate.ExistingFileName()), "existing_file": duplicate.ExistingFileName()})
		case errors.Is(err, iflowprovider.ErrExtractEmail):
			c.JSON(http.StatusBadRequest, gin.H{"status": "error", "code": "invalid_cookie_token", "error": "无法从提供的 iFlow Cookie 中解析出账号邮箱，请确认 Cookie 完整且未过期"})
		case errors.Is(err, iflowprovider.ErrSaveTokens):
			log.WithError(err).Error("iflow cookie login failed to save tokens")
			c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "code": "save_tokens_failed", "error": "保存 iFlow 认证凭据失败，请检查服务器存储权限或磁盘空间"})
		default:
			c.JSON(http.StatusBadRequest, gin.H{"status": "error", "code": "iflow_cookie_auth_failed", "error": fmt.Sprintf("iFlow Cookie 登录校验失败：%s", err.Error())})
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":     "ok",
		"saved_path": result.SavedPath,
		"email":      result.Email,
		"expired":    result.Expired,
		"type":       result.Type,
	})
}

func (h *Handler) GetAuthStatus(c *gin.Context) {
	state := strings.TrimSpace(c.Query("state"))
	if state == "" {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return
	}
	if err := ValidateOAuthState(state); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "code": "invalid_oauth_state", "error": "OAuth 状态参数格式无效（invalid state），请重新发起授权登录"})
		return
	}
	if respondSharedAuthStatus(c, state) {
		return
	}

	_, tenantID, status, ok := GetOAuthSessionWithTenant(state)
	if !ok {
		// Every login registers its session (and a superseded one now leaves a
		// tombstone, answered below), so an unknown state means the session
		// expired or the process restarted and lost it. Answering "ok" here used
		// to announce a login that never finished; report it the way the cluster
		// store already does.
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": oauthsession.MessageExpired, "code": oauthCodeSessionExpired})
		return
	}
	if tenantID != "" && tenantID != effectiveTenantID(c) {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": "OAuth 登录状态不存在或已过期（unknown or expired state），请重新发起授权登录", "code": oauthCodeSessionExpired})
		return
	}
	// A login replaced by another completed login of the same provider keeps
	// answering "ok", as it always has: from the operator's point of view the
	// provider is now signed in.
	if status == oauthSessionStatusCompleted || status == oauthSessionStatusSuperseded {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return
	}
	if status != "" {
		c.JSON(http.StatusOK, gin.H{"status": "error", "error": status})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "wait"})
}

// PopulateAuthContext extracts request info and adds it to the context
func PopulateAuthContext(ctx context.Context, c *gin.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if c == nil || c.Request == nil {
		return ctx
	}
	info := &coreauth.RequestInfo{
		Query:   c.Request.URL.Query(),
		Headers: c.Request.Header,
	}
	return coreauth.WithRequestInfo(ctx, info)
}

func requestAuthContext(c *gin.Context) context.Context {
	if c != nil && c.Request != nil {
		if reqCtx := c.Request.Context(); reqCtx != nil {
			return PopulateAuthContext(reqCtx, c)
		}
	}
	return PopulateAuthContext(context.Background(), c)
}

func detachedAuthContext(c *gin.Context) context.Context {
	if c != nil && c.Request != nil {
		if reqCtx := c.Request.Context(); reqCtx != nil {
			return PopulateAuthContext(context.WithoutCancel(reqCtx), c)
		}
	}
	return PopulateAuthContext(context.Background(), c)
}
