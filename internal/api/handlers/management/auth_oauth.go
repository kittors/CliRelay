package management

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

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

	savedPath, err := h.authFileRepository().Save(ctx, record)
	if err != nil {
		return "", err
	}
	if h.authManager != nil {
		runtimeCtx := coreauth.WithSkipPersist(ctx)
		if _, exists := h.authManager.GetByID(record.ID); exists {
			_, err = h.authManager.Update(runtimeCtx, record)
		} else {
			_, err = h.authManager.Register(runtimeCtx, record)
		}
		if err != nil {
			return "", fmt.Errorf("register tenant auth record: %w", err)
		}
	}
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
	return h.cfg.ResolveProxyURL(proxyID, "")
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
		AuthDir:               authDir,
		Config:                h.cfg,
		ProxyURL:              h.resolveOAuthProxyURL(c),
		WebUI:                 isWebUIRequest(c),
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
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate PKCE codes"})
		case errors.Is(err, claudeprovider.ErrStateGeneration):
			log.Errorf("Failed to generate state parameter: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		case errors.Is(err, claudeprovider.ErrAuthURL):
			log.Errorf("Failed to generate authorization URL: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		default:
			log.WithError(err).Error("failed to start anthropic oauth flow")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		}
		return
	}

	c.JSON(200, gin.H{"status": "ok", "url": result.AuthURL, "state": result.State})
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
			c.JSON(http.StatusBadRequest, gin.H{"error": "gemini oauth client-id not configured"})
		case errors.Is(err, geminicli.ErrStateGeneration):
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		case errors.Is(err, geminicli.ErrCallbackUnavailable):
			log.WithError(err).Error("failed to compute gemini callback target")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "callback server unavailable"})
		case errors.Is(err, geminicli.ErrCallbackStart):
			log.WithError(err).Error("failed to start gemini callback forwarder")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start callback server"})
		default:
			log.WithError(err).Error("failed to start gemini oauth flow")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start oauth flow"})
		}
		return
	}

	c.JSON(200, gin.H{"status": "ok", "url": result.AuthURL, "state": result.State})
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
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate PKCE codes"})
		case errors.Is(err, codexprovider.ErrStateGeneration):
			log.Errorf("Failed to generate state parameter: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		case errors.Is(err, codexprovider.ErrAuthURL):
			log.Errorf("Failed to generate authorization URL: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		default:
			log.WithError(err).Error("failed to start codex oauth flow")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		}
		return
	}

	c.JSON(200, gin.H{"status": "ok", "url": result.AuthURL, "state": result.State})
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
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		case errors.Is(err, antigravityprovider.ErrCallbackUnavailable):
			log.WithError(err).Error("failed to compute antigravity callback target")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "callback server unavailable"})
		case errors.Is(err, antigravityprovider.ErrCallbackStart):
			log.WithError(err).Error("failed to start antigravity callback forwarder")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start callback server"})
		case errors.Is(err, antigravityprovider.ErrOAuthClientIDMissing):
			c.JSON(http.StatusBadRequest, gin.H{"error": "antigravity oauth client-id not configured"})
		default:
			log.WithError(err).Error("failed to start antigravity oauth flow")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start oauth flow"})
		}
		return
	}

	c.JSON(200, gin.H{"status": "ok", "url": result.AuthURL, "state": result.State})
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		return
	}

	c.JSON(200, gin.H{"status": "ok", "url": result.AuthURL, "state": result.State})
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		return
	}

	c.JSON(200, gin.H{"status": "ok", "url": result.AuthURL, "state": result.State})
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
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate PKCE codes"})
		case errors.Is(err, xaiprovider.ErrStateGeneration):
			log.WithError(err).Error("failed to generate xai state parameter")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		case errors.Is(err, xaiprovider.ErrCallbackUnavailable):
			log.WithError(err).Error("failed to compute xai callback target")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "callback server unavailable"})
		case errors.Is(err, xaiprovider.ErrCallbackStart):
			log.WithError(err).Error("failed to start xai callback forwarder")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start callback server"})
		case errors.Is(err, xaiprovider.ErrDiscovery), errors.Is(err, xaiprovider.ErrAuthURL):
			log.WithError(err).Error("failed to start xai oauth flow")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		default:
			log.WithError(err).Error("failed to start xai oauth flow")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start oauth flow"})
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "url": result.AuthURL, "state": result.State})
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
			c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "callback server unavailable"})
		case errors.Is(err, iflowprovider.ErrCallbackStart):
			log.WithError(err).Error("failed to start iflow callback forwarder")
			c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "failed to start callback server"})
		default:
			log.WithError(err).Error("failed to start iflow oauth flow")
			c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "failed to start oauth flow"})
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "url": result.AuthURL, "state": result.State})
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
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "cookie is required"})
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
			c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "cookie is required"})
		case errors.Is(err, iflowprovider.ErrDuplicateCheck):
			c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "failed to check duplicate"})
		case errors.As(err, &duplicate):
			c.JSON(http.StatusConflict, gin.H{"status": "error", "error": "duplicate BXAuth found", "existing_file": duplicate.ExistingFileName()})
		case errors.Is(err, iflowprovider.ErrExtractEmail):
			c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "failed to extract email from token"})
		case errors.Is(err, iflowprovider.ErrSaveTokens):
			c.JSON(http.StatusInternalServerError, gin.H{"status": "error", "error": "failed to save authentication tokens"})
		default:
			c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": err.Error()})
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
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid state"})
		return
	}

	_, tenantID, status, ok := GetOAuthSessionWithTenant(state)
	if !ok {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return
	}
	if tenantID != "" && tenantID != effectiveTenantID(c) {
		c.JSON(http.StatusNotFound, gin.H{"status": "error", "error": "unknown or expired state"})
		return
	}
	if status == oauthSessionStatusCompleted {
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
