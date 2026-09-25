package management

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	serviceapp "github.com/router-for-me/CLIProxyAPI/v6/internal/app/service"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	settingsstore "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/store"
)

const configVersionContextKey = "management.config_version"

// configConflictMessage is shown to the operator as is; the panel keys its own
// wording on the error code.
const configConflictMessage = "数据已被其他节点修改，请刷新 (configuration was modified by another node or administrator; refresh and retry)"

// CaptureConfigVersion remembers the version a write request says it edited:
// the If-Match header, or else a top-level "version" field of a JSON object
// body. The body is restored for the handler. The version is optional so that
// panels predating versioning keep working with last-writer-wins semantics.
func CaptureConfigVersion() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c == nil || c.Request == nil {
			return
		}
		switch c.Request.Method {
		case http.MethodPut, http.MethodPatch, http.MethodPost, http.MethodDelete:
		default:
			c.Next()
			return
		}
		if version, ok := parseConfigVersion(c.GetHeader("If-Match")); ok {
			c.Set(configVersionContextKey, version)
			c.Next()
			return
		}
		contentType := strings.ToLower(c.ContentType())
		if c.Request.Body == nil || (contentType != "" && contentType != "application/json") {
			c.Next()
			return
		}
		data, err := io.ReadAll(c.Request.Body)
		var rest io.Reader = bytes.NewReader(data)
		if err != nil {
			// Hand the handler the same failure it would have hit reading.
			rest = io.MultiReader(rest, errorReader{err: err})
		}
		c.Request.Body = io.NopCloser(rest)
		if err == nil {
			if version, ok := bodyConfigVersion(data); ok {
				c.Set(configVersionContextKey, version)
			}
		}
		c.Next()
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

// parseConfigVersion accepts `3`, `"3"` and `W/"3"`.
func parseConfigVersion(raw string) (int64, bool) {
	raw = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "W/"))
	raw = strings.Trim(raw, `"`)
	if raw == "" {
		return 0, false
	}
	version, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || version < 0 {
		return 0, false
	}
	return version, true
}

func bodyConfigVersion(data []byte) (int64, bool) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return 0, false
	}
	var body struct {
		Version json.RawMessage `json:"version"`
	}
	if err := json.Unmarshal(trimmed, &body); err != nil || len(body.Version) == 0 {
		return 0, false
	}
	return parseConfigVersion(string(body.Version))
}

// requestVersion returns the version the client edited, or nil when it sent
// none.
func requestVersion(c *gin.Context) *int64 {
	if c == nil {
		return nil
	}
	if value, ok := c.Get(configVersionContextKey); ok {
		if version, ok := value.(int64); ok {
			return &version
		}
	}
	return nil
}

// requestExpectedVersion is the version a whole-value replacement is checked
// against: the client's, or none at all for clients that send none.
func requestExpectedVersion(c *gin.Context) int64 {
	if version := requestVersion(c); version != nil {
		return *version
	}
	return configsync.AnyVersion
}

// writeConfigConflict answers a version conflict with 409 and reports whether
// err was one.
func writeConfigConflict(c *gin.Context, err error) bool {
	if !errors.Is(err, configsync.ErrVersionConflict) {
		return false
	}
	details := gin.H{}
	var conflict *configsync.ConflictError
	if errors.As(err, &conflict) {
		details = gin.H{"domain": conflict.Domain, "key": conflict.Key, "expected_version": conflict.Expected, "current_version": conflict.Current}
	}
	c.JSON(http.StatusConflict, gin.H{"error": gin.H{"code": "config_version_conflict", "message": configConflictMessage, "details": details}})
	return true
}

// writeConfigSaveError answers a failed configuration write.
func writeConfigSaveError(c *gin.Context, what string, err error) {
	if writeConfigConflict(c, err) {
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("%s: %v", what, err)})
}

// freshConfig returns a copy of the configuration of tenantID whose runtime
// settings were just read from the database. Management edits apply to this
// copy, never to the live config, so they start from the stored values even
// when this node has not heard of another node's change yet.
func (h *Handler) freshConfig(tenantID string) *config.Config {
	h.mu.Lock()
	var base config.Config
	if h.cfg != nil {
		base = *h.cfg
	}
	h.mu.Unlock()
	return settingsstore.FreshConfig(&base, tenantID)
}

// mutateSystemConfig applies a management edit to a fresh copy of the system
// configuration, stores the settings it changed against the versions it read
// and then makes them live. A mutate error is answered with 400.
func (h *Handler) mutateSystemConfig(c *gin.Context, mutate func(*config.Config) error) bool {
	if !settingsstore.StoreAvailable() {
		return h.mutateLiveConfigFile(c, mutate)
	}
	cfg := h.freshConfig(identity.SystemTenantID)
	if err := mutate(cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return false
	}
	return h.commitConfig(c, identity.SystemTenantID, cfg)
}

// mutateLiveConfigFile is the path without a database, where config.yaml is
// the store: edit the live config, write the file, and undo the edit when the
// write fails so memory never runs ahead of what was saved.
func (h *Handler) mutateLiveConfigFile(c *gin.Context, mutate func(*config.Config) error) bool {
	h.mu.Lock()
	if h.cfg == nil {
		h.cfg = &config.Config{}
	}
	previous := *h.cfg
	err := mutate(h.cfg)
	if err != nil {
		*h.cfg = previous
		h.mu.Unlock()
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return false
	}
	if saveErr := settingsstore.SaveConfig(h.cfg, h.configFilePath); saveErr != nil {
		*h.cfg = previous
		h.mu.Unlock()
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to save config: %v", saveErr)})
		return false
	}
	cfg := h.cfg
	mutated := h.onConfigMutated
	h.mu.Unlock()
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
	if mutated != nil {
		mutated(cfg)
	}
	return true
}

// commitRequestConfig commits cfg, the fresh copy the request edited, for the
// request's effective tenant.
func (h *Handler) commitRequestConfig(c *gin.Context, cfg *config.Config) bool {
	return h.commitConfig(c, effectiveTenantID(c), cfg)
}

// commitConfig stores the settings cfg changed for tenantID and makes them
// live on this node. Other nodes learn about it from the event the write
// published.
func (h *Handler) commitConfig(c *gin.Context, tenantID string, cfg *config.Config) bool {
	keys, err := settingsstore.CommitTenantConfig(c.Request.Context(), tenantID, cfg, requestVersion(c), h.configFilePath)
	if err != nil {
		writeConfigSaveError(c, "failed to save config", err)
		return false
	}
	response := gin.H{"status": "ok"}
	if len(keys) == 1 {
		setVersionHeader(c, cfg.RuntimeSettingState().Version(keys[0]))
	}
	if tenantID != identity.SystemTenantID {
		if h.authManager != nil {
			h.authManager.SetConfigForTenant(tenantID, cfg)
			serviceapp.RebindTenantExecutors(h.cfg, h.authManager, tenantID, nil)
		}
		c.JSON(http.StatusOK, response)
		return true
	}
	h.mu.Lock()
	if h.cfg == nil {
		h.cfg = &config.Config{}
	}
	live := h.cfg
	settingsstore.AdoptKeys(live, cfg, keys...)
	mutated := h.onConfigMutated
	h.mu.Unlock()
	if h.authManager != nil {
		h.authManager.SetConfig(live)
	}
	c.JSON(http.StatusOK, response)
	if mutated != nil {
		mutated(live)
	}
	return true
}

// requestSettingVersion is the version of key in the fresh config copy this
// request works on (see providerConfigForTenant).
func (h *Handler) requestSettingVersion(c *gin.Context, key string) int64 {
	cfg := h.providerConfigForTenant(c)
	if cfg == nil {
		return 0
	}
	return cfg.RuntimeSettingState().Version(key)
}

// setVersionHeader reports a version as the ETag of the response. Versions go
// in a header rather than the body because several of these bodies are maps
// that existing clients decode into a single value type.
func setVersionHeader(c *gin.Context, version int64) {
	c.Header("ETag", strconv.Quote(strconv.FormatInt(version, 10)))
}

// jsonWithLiveVersion answers a read of a live setting with its version.
func (h *Handler) jsonWithLiveVersion(c *gin.Context, key string, body any) {
	setVersionHeader(c, h.liveSettingVersion(key))
	c.JSON(http.StatusOK, body)
}

// jsonWithRequestVersion answers a read served from the request's fresh copy.
func (h *Handler) jsonWithRequestVersion(c *gin.Context, key string, body any) {
	setVersionHeader(c, h.requestSettingVersion(c, key))
	c.JSON(http.StatusOK, body)
}

// liveSettingVersion is the version of key this node's live config holds.
func (h *Handler) liveSettingVersion(key string) int64 {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		return 0
	}
	return h.cfg.RuntimeSettingState().Version(key)
}

// configResponse is GET /config: the sanitized config plus the version of each
// stored setting, which the panel sends back with a write to detect that
// someone else changed the setting in between.
type configResponse struct {
	*config.Config
	RuntimeSettingVersions map[string]int64 `json:"runtime-setting-versions,omitempty"`
}

func withSettingVersions(cfg *config.Config, versions map[string]int64) configResponse {
	return configResponse{Config: cfg, RuntimeSettingVersions: versions}
}

// WithLiveConfig runs fn with this node's live configuration while holding the
// handler lock. Cluster reloads use it to apply, in place, a change another
// node made, the same way this node's handlers apply their own.
func (h *Handler) WithLiveConfig(fn func(*config.Config)) {
	if h == nil || fn == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		return
	}
	fn(h.cfg)
}
