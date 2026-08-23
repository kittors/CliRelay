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
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/aiaccountstatus"
	imagegeneration "github.com/router-for-me/CLIProxyAPI/v6/internal/management/imagegeneration"
	settingsstore "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/store"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// Handler aggregates config reference, persistence path and helpers.
type Handler struct {
	cfg                  *config.Config
	configFilePath       string
	mu                   sync.Mutex
	loginThrottle        *loginThrottle
	auditRepeat          *auditRepeatLimiter
	authManager          *coreauth.Manager
	usageStats           *usage.RequestStatistics
	tokenStore           coreauth.Store
	localPassword        string
	allowRemoteOverride  bool
	envSecret            string
	logDir               string
	postAuthHook         coreauth.PostAuthHook
	onConfigMutated      func(*config.Config)
	onModelConfigMutated func(tenantID string)
	startTime            time.Time
	accessManager        *sdkaccess.Manager
	trendCacheMu         sync.Mutex
	trendCache           map[string]trendCacheEntry
	systemStatsCacheMu   sync.Mutex
	systemStatsCache     systemStatsCacheEntry
	imageGeneration      *imagegeneration.Service
	videoGeneration      *imagegeneration.Service
	identityService      *identity.Service
	aiAccountStatus      *aiaccountstatus.Service
	statusScheduler      *aiaccountstatus.Scheduler
}

type trendCacheEntry struct {
	expiresAt time.Time
	payload   any
}

// NewHandler creates a new management handler instance.
func NewHandler(cfg *config.Config, configFilePath string, manager *coreauth.Manager) *Handler {
	envSecret, _ := os.LookupEnv("MANAGEMENT_PASSWORD")
	envSecret = strings.TrimSpace(envSecret)
	// IP access control follows database availability, not one startup path: any
	// caller that assembles the management API itself must get a working registry
	// rather than 503 on every rule endpoint.
	ensureIPAccessRuntime(cfg)

	h := &Handler{
		cfg:                 cfg,
		configFilePath:      configFilePath,
		loginThrottle:       newLoginThrottle(throttlePoliciesFromConfig(cfg)),
		auditRepeat:         newAuditRepeatLimiter(auditRepeatWindow, auditRepeatIdleTTL),
		authManager:         manager,
		usageStats:          usage.GetRequestStatistics(),
		tokenStore:          sdkAuth.GetTokenStore(),
		allowRemoteOverride: envSecret != "",
		envSecret:           envSecret,
		startTime:           time.Now(),
		trendCache:          make(map[string]trendCacheEntry),
	}
	h.imageGeneration = h.newImageGenerationService()
	h.videoGeneration = h.newVideoGenerationService()
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

func (h *Handler) ensureVideoGenerationService() *imagegeneration.Service {
	if h == nil || h.authManager == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.videoGeneration == nil {
		h.videoGeneration = h.newVideoGenerationService()
	}
	return h.videoGeneration
}

// Close stops background cleanup workers owned by the management handler.
func (h *Handler) Close() {
	if h == nil {
		return
	}
	h.loginThrottle.close()
	h.stopAccountStatusScheduler()
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
	ensureIPAccessRuntime(cfg)
	// Throttle thresholds are part of the reloadable config, so a limit raised in
	// config.yaml has to take effect without a restart — otherwise the documented
	// escape hatch for an over-tight limit is "restart the server". Panel-set
	// overrides are re-applied on top, or a config reload would silently revert
	// them while the panel kept displaying them as active.
	h.loginThrottle.setPolicies(overlayThrottleOverride(throttlePoliciesFromConfig(cfg), storedThrottleOverride()))
	// Interval and on/off are reloadable for the same reason: turning the probe
	// down must not require a restart.
	h.StartAccountStatusScheduler()
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

func (h *Handler) SetModelConfigMutatedHook(fn func(tenantID string)) { h.onModelConfigMutated = fn }

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
// The implementation lives in middleware_auth.go, where the step order that
// keeps the ban check ahead of every secret comparison is documented.
func (h *Handler) Middleware() gin.HandlerFunc {
	return h.managementAuthMiddleware
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
