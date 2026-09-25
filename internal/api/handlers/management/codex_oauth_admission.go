package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/codexadmission"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	settingsstore "github.com/router-for-me/CLIProxyAPI/v6/internal/management/settings/store"
)

type codexOAuthAdmissionResponse struct {
	AllowedClients          []string                                 `json:"allowed_clients"`
	AvailableAllowedClients []codexadmission.AllowedClientPresetInfo `json:"available_allowed_clients"`
	CodexOAuthAdmission     config.CodexOAuthAdmissionConfig         `json:"codex-oauth-admission"`
	Version                 int64                                    `json:"version"`
}

type codexOAuthAdmissionRequest struct {
	AllowedClients []string `json:"allowed_clients"`
}

func (h *Handler) GetCodexOAuthAdmission(c *gin.Context) {
	cfg := h.providerConfigForTenant(c)
	current := config.CodexOAuthAdmissionConfig{}
	if cfg != nil {
		current = cfg.CodexOAuthAdmission
	}

	current = config.CleanCodexOAuthAdmission(current)
	c.JSON(http.StatusOK, codexOAuthAdmissionResponse{
		AllowedClients:          append([]string(nil), current.AllowedClientPresets...),
		AvailableAllowedClients: codexadmission.AvailableAllowedClientPresets(),
		CodexOAuthAdmission:     current,
		Version:                 h.requestSettingVersion(c, settingsstore.RuntimeSettingCodexOAuthAdmission),
	})
}

func (h *Handler) PutCodexOAuthAdmission(c *gin.Context) {
	var body codexOAuthAdmissionRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	allowedClients, err := codexadmission.NormalizeAllowedClientPresets(body.AllowedClients)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	next := config.CodexOAuthAdmissionConfig{AllowedClientPresets: allowedClients}

	cfg := h.providerConfigForTenant(c)
	if cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "config unavailable"})
		return
	}
	// cfg is this request's fresh copy; a failed commit leaves nothing to undo.
	cfg.CodexOAuthAdmission = next
	h.commitRequestConfig(c, cfg)
}
