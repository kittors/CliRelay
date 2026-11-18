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
	"github.com/router-for-me/CLIProxyAPI/v8/internal/identity"
	managementauthfiles "github.com/router-for-me/CLIProxyAPI/v8/internal/management/authfiles"
	geminicli "github.com/router-for-me/CLIProxyAPI/v8/internal/management/oauth/providers/geminicli"
	iflowprovider "github.com/router-for-me/CLIProxyAPI/v8/internal/management/oauth/providers/iflow"
	qwenprovider "github.com/router-for-me/CLIProxyAPI/v8/internal/management/oauth/providers/qwen"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	geminiCallbackPort       = 8085
	oauthCallbackWaitTimeout = oauthSessionTTL
)

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

	// A login creates or replaces the credential outright (see
	// coreauth.WithCredentialCreate); single-node stores ignore the mark.
	savedPath, err := h.authFileRepository().Save(coreauth.WithCredentialCreate(ctx), record)
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
			c.JSON(http.StatusInternalServerError, oauthStartFailure("failed to start callback server", err))
		default:
			log.WithError(err).Error("failed to start gemini oauth flow")
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

// PopulateAuthContext extracts request info and adds it to the context

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
