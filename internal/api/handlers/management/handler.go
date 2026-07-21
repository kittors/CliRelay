// Package management provides the management API handlers and middleware
// for configuring the server and managing auth files.
package management

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	serviceapp "github.com/router-for-me/CLIProxyAPI/v6/internal/app/service"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/aiaccountstatus"
	imagegeneration "github.com/router-for-me/CLIProxyAPI/v6/internal/management/imagegeneration"
	settingsstore "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/store"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"golang.org/x/crypto/bcrypt"
)

type attemptInfo struct {
	count        int
	blockedUntil time.Time
	lastActivity time.Time // track last activity for cleanup
}

// attemptCleanupInterval controls how often stale IP entries are purged
const attemptCleanupInterval = 1 * time.Hour

// attemptMaxIdleTime controls how long an IP can be idle before cleanup
const attemptMaxIdleTime = 2 * time.Hour

// Handler aggregates config reference, persistence path and helpers.
type Handler struct {
	cfg                  *config.Config
	configFilePath       string
	mu                   sync.Mutex
	attemptsMu           sync.Mutex
	failedAttempts       map[string]*attemptInfo // keyed by client IP
	authManager          *coreauth.Manager
	usageStats           *usage.RequestStatistics
	tokenStore           coreauth.Store
	localPassword        string
	allowRemoteOverride  bool
	envSecret            string
	logDir               string
	postAuthHook         coreauth.PostAuthHook
	onConfigMutated      func(*config.Config)
	onModelConfigMutated func()
	startTime            time.Time
	attemptCleanupStop   chan struct{}
	attemptCleanupOnce   sync.Once
	accessManager        *sdkaccess.Manager
	trendCacheMu         sync.Mutex
	trendCache           map[string]trendCacheEntry
	systemStatsCacheMu   sync.Mutex
	systemStatsCache     systemStatsCacheEntry
	imageGeneration      *imagegeneration.Service
	identityService      *identity.Service
	aiAccountStatus      *aiaccountstatus.Service
}

type trendCacheEntry struct {
	expiresAt time.Time
	payload   any
}

// NewHandler creates a new management handler instance.
func NewHandler(cfg *config.Config, configFilePath string, manager *coreauth.Manager) *Handler {
	envSecret, _ := os.LookupEnv("MANAGEMENT_PASSWORD")
	envSecret = strings.TrimSpace(envSecret)

	h := &Handler{
		cfg:                 cfg,
		configFilePath:      configFilePath,
		failedAttempts:      make(map[string]*attemptInfo),
		authManager:         manager,
		usageStats:          usage.GetRequestStatistics(),
		tokenStore:          sdkAuth.GetTokenStore(),
		allowRemoteOverride: envSecret != "",
		envSecret:           envSecret,
		startTime:           time.Now(),
		attemptCleanupStop:  make(chan struct{}),
		trendCache:          make(map[string]trendCacheEntry),
	}
	h.imageGeneration = h.newImageGenerationService()
	h.startAttemptCleanup()
	return h
}

func (h *Handler) newImageGenerationService() *imagegeneration.Service {
	if h == nil {
		return nil
	}
	return imagegeneration.NewService(func(ctx context.Context, tenantID string, payload []byte, alt string) ([]byte, error) {
		return h.executeImageGenerationTestForTenant(ctx, tenantID, payload, alt)
	}, imageGenerationSystemAPIKey)
}

func (h *Handler) ensureImageGenerationService() *imagegeneration.Service {
	if h == nil || h.authManager == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.imageGeneration == nil {
		h.imageGeneration = h.newImageGenerationService()
	}
	return h.imageGeneration
}

// startAttemptCleanup launches a background goroutine that periodically
// removes stale IP entries from failedAttempts to prevent memory leaks.
func (h *Handler) startAttemptCleanup() {
	go func() {
		ticker := time.NewTicker(attemptCleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				h.purgeStaleAttempts()
			case <-h.attemptCleanupStop:
				return
			}
		}
	}()
}

// Close stops background cleanup workers owned by the management handler.
func (h *Handler) Close() {
	if h == nil {
		return
	}
	h.attemptCleanupOnce.Do(func() {
		close(h.attemptCleanupStop)
	})
}

// purgeStaleAttempts removes IP entries that have been idle beyond attemptMaxIdleTime
// and whose ban (if any) has expired.
func (h *Handler) purgeStaleAttempts() {
	now := time.Now()
	h.attemptsMu.Lock()
	defer h.attemptsMu.Unlock()
	for ip, ai := range h.failedAttempts {
		// Skip if still banned
		if !ai.blockedUntil.IsZero() && now.Before(ai.blockedUntil) {
			continue
		}
		// Remove if idle too long
		if now.Sub(ai.lastActivity) > attemptMaxIdleTime {
			delete(h.failedAttempts, ip)
		}
	}
}

// NewHandler creates a new management handler instance.
func NewHandlerWithoutConfigFilePath(cfg *config.Config, manager *coreauth.Manager) *Handler {
	return NewHandler(cfg, "", manager)
}

// SetConfig updates the in-memory config reference when the server hot-reloads.
func (h *Handler) SetConfig(cfg *config.Config) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.cfg = cfg
	// Recreate lazily so provider probes use the new proxy/TLS/runtime config.
	h.aiAccountStatus = nil
	h.mu.Unlock()
}

// SetAuthManager updates the auth manager reference used by management endpoints.
func (h *Handler) SetAuthManager(manager *coreauth.Manager) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.authManager = manager
	// Recreate lazily so tenant auth selection never uses the replaced manager.
	h.aiAccountStatus = nil
	h.mu.Unlock()
	h.ensureImageGenerationService()
}

func (h *Handler) SetConfigMutatedHook(fn func(*config.Config)) { h.onConfigMutated = fn }

func (h *Handler) SetModelConfigMutatedHook(fn func()) { h.onModelConfigMutated = fn }

// SetAccessManager wires the request authentication access manager so management writes
// (such as API key channel/model restrictions) can be applied immediately at runtime.
func (h *Handler) SetAccessManager(manager *sdkaccess.Manager) { h.accessManager = manager }

// SetUsageStatistics allows replacing the usage statistics reference.
func (h *Handler) SetUsageStatistics(stats *usage.RequestStatistics) { h.usageStats = stats }

// SetLocalPassword configures the runtime-local password accepted for localhost requests.
func (h *Handler) SetLocalPassword(password string) { h.localPassword = password }

// SetLogDirectory updates the directory where main.log should be looked up.
func (h *Handler) SetLogDirectory(dir string) {
	if dir == "" {
		return
	}
	if !filepath.IsAbs(dir) {
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
	}
	h.logDir = dir
}

// SetPostAuthHook registers a hook to be called after auth record creation but before persistence.
func (h *Handler) SetPostAuthHook(hook coreauth.PostAuthHook) {
	h.postAuthHook = hook
}

// Middleware enforces access control for management endpoints.
// All requests (local and remote) require a valid management key.
// Additionally, remote access requires allow-remote-management=true.
func (h *Handler) Middleware() gin.HandlerFunc {
	const maxFailures = 5
	const banDuration = 30 * time.Minute

	return func(c *gin.Context) {
		c.Header("X-CPA-VERSION", buildinfo.Version)
		c.Header("X-CPA-COMMIT", buildinfo.Commit)
		c.Header("X-CPA-BUILD-DATE", buildinfo.BuildDate)
		currentUIVersion, currentUICommit := h.currentFrontendState()
		c.Header("X-CPA-UI-VERSION", currentUIVersion)
		c.Header("X-CPA-UI-COMMIT", currentUICommit)

		// Session tokens (cps_*) come from Authorization: Bearer on normal HTTP, or from
		// ?token= on the system-stats WebSocket handshake (browsers cannot set WS headers).
		// Resolve the session token before falling through to management-key auth so a
		// valid user session is never bcrypt-compared against the remote management secret.
		if token := resolveSessionToken(c); strings.HasPrefix(token, "cps_") {
			_ = h.authenticateSessionToken(c, token)
			return
		}

		clientIP := c.ClientIP()
		localClient := clientIP == "127.0.0.1" || clientIP == "::1"
		cfg := h.cfg
		var (
			allowRemote bool
			secretHash  string
		)
		if cfg != nil {
			allowRemote = cfg.RemoteManagement.AllowRemote
			secretHash = cfg.RemoteManagement.SecretKey
		}
		if h.allowRemoteOverride {
			allowRemote = true
		}
		envSecret := h.envSecret

		fail := func() {}
		isBanned := func() (time.Duration, bool) { return 0, false }
		clearFailures := func() {}
		if !localClient {
			if !allowRemote {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "remote management disabled"})
				return
			}

			isBanned = func() (time.Duration, bool) {
				h.attemptsMu.Lock()
				defer h.attemptsMu.Unlock()
				ai := h.failedAttempts[clientIP]
				if ai == nil || ai.blockedUntil.IsZero() {
					return 0, false
				}
				if time.Now().Before(ai.blockedUntil) {
					return time.Until(ai.blockedUntil).Round(time.Second), true
				}
				// Ban expired, reset state.
				ai.blockedUntil = time.Time{}
				ai.count = 0
				return 0, false
			}

			fail = func() {
				h.attemptsMu.Lock()
				aip := h.failedAttempts[clientIP]
				if aip == nil {
					aip = &attemptInfo{}
					h.failedAttempts[clientIP] = aip
				}
				aip.count++
				aip.lastActivity = time.Now()
				if aip.count >= maxFailures {
					aip.blockedUntil = time.Now().Add(banDuration)
					aip.count = 0
				}
				h.attemptsMu.Unlock()
			}

			clearFailures = func() {
				h.attemptsMu.Lock()
				if ai := h.failedAttempts[clientIP]; ai != nil {
					ai.count = 0
					ai.blockedUntil = time.Time{}
				}
				h.attemptsMu.Unlock()
			}
		}
		localPasswordConfigured := localClient && h.localPassword != ""
		if secretHash == "" && envSecret == "" && !localPasswordConfigured {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "remote management key not set"})
			return
		}

		// Accept either Authorization: Bearer <key> or X-Management-Key
		var provided string
		if ah := c.GetHeader("Authorization"); ah != "" {
			parts := strings.SplitN(ah, " ", 2)
			if len(parts) == 2 && strings.ToLower(parts[0]) == "bearer" {
				provided = parts[1]
			} else {
				provided = ah
			}
		}
		if provided == "" {
			provided = c.GetHeader("X-Management-Key")
		}
		// Fallback: ?token= query param is accepted only for WebSocket handshakes;
		// normal HTTP requests must not carry credentials in URLs.
		// Session tokens (cps_*) were already handled above; remaining query tokens are
		// treated as management keys (legacy panel / service credential).
		if provided == "" && shouldReadManagementTokenFromQuery(c) {
			if queryToken := strings.TrimSpace(c.Query("token")); !strings.HasPrefix(queryToken, "cps_") {
				provided = queryToken
			}
		}

		if provided == "" {
			if !localClient {
				if remaining, banned := isBanned(); banned {
					c.Header("Retry-After", retryAfterSecondsHeader(remaining))
					c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": fmt.Sprintf("IP banned due to too many failed attempts. Try again in %s", remaining)})
					return
				}
				fail()
			}
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing management key"})
			return
		}

		if localClient {
			if lp := h.localPassword; lp != "" {
				if util.ConstantTimeStringEqual(provided, lp) {
					h.setServicePrincipal(c)
					h.nextWithManagementAudit(c)
					return
				}
			}
		}

		if envSecret != "" && util.ConstantTimeStringEqual(provided, envSecret) {
			clearFailures()
			h.setServicePrincipal(c)
			h.nextWithManagementAudit(c)
			return
		}

		if secretHash == "" || bcrypt.CompareHashAndPassword([]byte(secretHash), []byte(provided)) != nil {
			if !localClient {
				if remaining, banned := isBanned(); banned {
					c.Header("Retry-After", retryAfterSecondsHeader(remaining))
					c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": fmt.Sprintf("IP banned due to too many failed attempts. Try again in %s", remaining)})
					return
				}
				fail()
			}
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid management key"})
			return
		}

		clearFailures()
		h.setServicePrincipal(c)

		h.nextWithManagementAudit(c)
	}
}

// resolveSessionToken returns a user-session token for management auth.
// Prefer Authorization Bearer; for WebSocket handshakes only, also accept ?token=cps_*.
func resolveSessionToken(c *gin.Context) string {
	if token := bearerToken(c); strings.HasPrefix(token, "cps_") {
		return token
	}
	if !shouldReadManagementTokenFromQuery(c) {
		return ""
	}
	if token := strings.TrimSpace(c.Query("token")); strings.HasPrefix(token, "cps_") {
		return token
	}
	return ""
}

// authenticateSessionToken validates a cps_* identity session and enforces RBAC + tenant scope.
// Returns false when the request was already aborted with an error response.
func (h *Handler) authenticateSessionToken(c *gin.Context, token string) bool {
	service := h.identity()
	if service == nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "identity_unavailable", "message": "identity service unavailable"}})
		return false
	}
	principal, err := service.Authenticate(c.Request.Context(), token, c.GetHeader("X-Effective-Tenant-ID"))
	if err != nil {
		identityError(c, err)
		return false
	}
	if principal.User.MustChangePassword {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": gin.H{"code": "password_change_required", "message": "password change required"}})
		return false
	}
	permission := permissionForManagementRequest(c.Request.Method, c.Request.URL.Path)
	if !principalHasManagementRequestPermission(principal, c.Request.Method, c.Request.URL.Path, permission) {
		h.recordManagementAudit(c, principal, "denied")
		identityError(c, identity.ErrPermissionDenied)
		return false
	}
	// Some runtime/config stores remain process-global. Business-tenant sessions
	// may only use fully tenant-scoped routes. Platform super-admins keep access
	// after tenant switch so ops pages like /logs still work under an effective
	// business tenant.
	if deniesTenantResourceScope(principal, c.Request.URL.Path) {
		h.recordManagementAudit(c, principal, "denied")
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": gin.H{"code": "tenant_resource_scope_unavailable", "message": "tenant business resources are not enabled for this tenant"}})
		return false
	}
	c.Set(managementPrincipalKey, principal)
	h.nextWithManagementAudit(c)
	return true
}

// persist saves the current in-memory config to disk.
func (h *Handler) persist(c *gin.Context) bool {
	h.mu.Lock()
	cfg := h.cfg
	mutated := h.onConfigMutated
	if err := settingsstore.SaveConfig(cfg, h.configFilePath); err != nil {
		h.mu.Unlock()
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to save config: %v", err)})
		return false
	}
	h.mu.Unlock()
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
	if mutated != nil {
		mutated(cfg)
	}
	return true
}

func (h *Handler) persistRuntimeSetting(c *gin.Context, key string, value any) bool {
	if err := settingsstore.PersistRuntimeSetting(h.cfg, h.configFilePath, key, value); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to save runtime setting: %v", err)})
		return false
	}
	cfg := h.cfg
	if h.authManager != nil {
		h.authManager.SetConfig(cfg)
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
	if h.onConfigMutated != nil {
		h.onConfigMutated(cfg)
	}
	return true
}

func (h *Handler) persistRuntimeSettingForTenant(c *gin.Context, key string, value any, cfg *config.Config) bool {
	tenantID := effectiveTenantID(c)
	if tenantID == identity.SystemTenantID {
		return h.persistRuntimeSetting(c, key, value)
	}
	if err := usage.UpsertRuntimeSettingForTenant(tenantID, key, value); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to save runtime setting: %v", err)})
		return false
	}
	if h.authManager != nil {
		h.authManager.SetConfigForTenant(tenantID, cfg)
		serviceapp.RebindTenantExecutors(h.cfg, h.authManager, tenantID, nil)
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
	return true
}

func (h *Handler) saveConfigFile() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return settingsstore.SaveConfig(h.cfg, h.configFilePath)
}

func (h *Handler) storeRuntimeSetting(key string, value any) error {
	return settingsstore.PersistRuntimeSetting(h.cfg, h.configFilePath, key, value)
}

// Helper methods for simple types
func (h *Handler) updateBoolField(c *gin.Context, set func(bool)) {
	var body struct {
		Value *bool `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	set(*body.Value)
	h.persist(c)
}

func (h *Handler) updateIntField(c *gin.Context, set func(int)) {
	var body struct {
		Value *int `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	set(*body.Value)
	h.persist(c)
}

func (h *Handler) updateStringField(c *gin.Context, set func(string)) {
	var body struct {
		Value *string `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	set(*body.Value)
	h.persist(c)
}
