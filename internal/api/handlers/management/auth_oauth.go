package management

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
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
	return h.authFileRepository().Save(ctx, record)
}

func (h *Handler) RequestAnthropicToken(c *gin.Context) {
	ctx := detachedAuthContext(c)

	result, err := claudeprovider.StartOAuthLogin(ctx, claudeprovider.OAuthLoginOptions{
		Config:                h.cfg,
		WebUI:                 isWebUIRequest(c),
		PreferredCallbackPort: anthropicCallbackPort,
		CallbackTarget:        h.managementCallbackURL,
		WaitCallback:          WaitOAuthCallbackFile,
		CallbackWaitTimeout:   oauthCallbackWaitTimeout,
		SaveRecord:            h.saveTokenRecord,
		Sessions: claudeprovider.SessionCallbacks{
			Register:         RegisterOAuthSession,
			SetError:         SetOAuthSessionError,
			Complete:         CompleteOAuthSession,
			CompleteProvider: CompleteOAuthSessionsByProvider,
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

	result, err := geminicli.StartOAuthLogin(ctx, geminicli.OAuthLoginOptions{
		Config:              h.cfg,
		ProjectID:           c.Query("project_id"),
		WebUI:               isWebUIRequest(c),
		CallbackPort:        geminiCallbackPort,
		CallbackTarget:      h.managementCallbackURL,
		WaitCallback:        WaitOAuthCallbackFile,
		CallbackWaitTimeout: oauthCallbackWaitTimeout,
		SaveRecord:          h.saveTokenRecord,
		Sessions: geminicli.SessionCallbacks{
			Register:         RegisterOAuthSession,
			SetError:         SetOAuthSessionError,
			Complete:         CompleteOAuthSession,
			CompleteProvider: CompleteOAuthSessionsByProvider,
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

	result, err := codexprovider.StartOAuthLogin(ctx, codexprovider.OAuthLoginOptions{
		Config:              h.cfg,
		WebUI:               isWebUIRequest(c),
		CallbackPort:        codexCallbackPort,
		CallbackTarget:      h.managementCallbackURL,
		WaitCallback:        WaitOAuthCallbackFile,
		CallbackWaitTimeout: oauthCallbackWaitTimeout,
		SaveRecord:          h.saveTokenRecord,
		Sessions: codexprovider.SessionCallbacks{
			Register:         RegisterOAuthSession,
			SetError:         SetOAuthSessionError,
			Complete:         CompleteOAuthSession,
			CompleteProvider: CompleteOAuthSessionsByProvider,
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

	result, err := antigravityprovider.StartOAuthLogin(ctx, antigravityprovider.OAuthLoginOptions{
		Config:              h.cfg,
		WebUI:               isWebUIRequest(c),
		CallbackTarget:      h.managementCallbackURL,
		WaitCallback:        WaitOAuthCallbackFile,
		CallbackWaitTimeout: oauthCallbackWaitTimeout,
		SaveRecord:          h.saveTokenRecord,
		Sessions: antigravityprovider.SessionCallbacks{
			Register:         RegisterOAuthSession,
			SetError:         SetOAuthSessionError,
			Complete:         CompleteOAuthSession,
			CompleteProvider: CompleteOAuthSessionsByProvider,
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

	result, err := qwenprovider.StartDeviceLogin(ctx, qwenprovider.DeviceLoginOptions{
		Config:     h.cfg,
		SaveRecord: h.saveTokenRecord,
		Sessions: qwenprovider.SessionCallbacks{
			Register: RegisterOAuthSession,
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

	result, err := kimiprovider.StartDeviceLogin(ctx, kimiprovider.DeviceLoginOptions{
		Config:     h.cfg,
		SaveRecord: h.saveTokenRecord,
		Sessions: kimiprovider.SessionCallbacks{
			Register:         RegisterOAuthSession,
			SetError:         SetOAuthSessionError,
			Complete:         CompleteOAuthSession,
			CompleteProvider: CompleteOAuthSessionsByProvider,
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

	result, err := xaiprovider.StartOAuthLogin(ctx, xaiprovider.OAuthLoginOptions{
		Config:              h.cfg,
		WebUI:               isWebUIRequest(c),
		UsingAPI:            queryBool(c, "using_api"),
		CallbackTarget:      h.managementCallbackURL,
		WaitCallback:        WaitOAuthCallbackFile,
		CallbackWaitTimeout: oauthCallbackWaitTimeout,
		SaveRecord:          h.saveTokenRecord,
		Sessions: xaiprovider.SessionCallbacks{
			Register:         RegisterOAuthSession,
			SetError:         SetOAuthSessionError,
			Complete:         CompleteOAuthSession,
			CompleteProvider: CompleteOAuthSessionsByProvider,
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

	result, err := iflowprovider.StartOAuthLogin(ctx, iflowprovider.OAuthLoginOptions{
		Config:              h.cfg,
		WebUI:               isWebUIRequest(c),
		CallbackTarget:      h.managementCallbackURL,
		WaitCallback:        WaitOAuthCallbackFile,
		CallbackWaitTimeout: oauthCallbackWaitTimeout,
		SaveRecord:          h.saveTokenRecord,
		Sessions: iflowprovider.SessionCallbacks{
			Register:         RegisterOAuthSession,
			SetError:         SetOAuthSessionError,
			Complete:         CompleteOAuthSession,
			CompleteProvider: CompleteOAuthSessionsByProvider,
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

	var payload struct {
		Cookie string `json:"cookie"`
	}
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "cookie is required"})
		return
	}

	result, err := iflowprovider.AuthenticateCookie(ctx, payload.Cookie, iflowprovider.CookieLoginOptions{
		Config:     h.cfg,
		SaveRecord: h.saveTokenRecord,
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

	_, status, ok := GetOAuthSession(state)
	if !ok {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
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
