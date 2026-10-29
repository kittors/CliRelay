package management

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/contentmoderation"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type contentModerationProfileResponse struct {
	contentmoderation.Profile
	APIKeyConfigured bool           `json:"api_key_configured"`
	APIKeyMasked     string         `json:"api_key_masked,omitempty"`
	BindingCounts    map[string]int `json:"binding_counts"`
}

func (h *Handler) contentModerationStore() *contentmoderation.Store {
	return contentmoderation.NewStore(usage.RuntimeDB())
}

func (h *Handler) deleteContentModerationBindings(ctx context.Context, tenantID, channelType string, channelIDs []string) error {
	if usage.RuntimeDB() == nil || len(channelIDs) == 0 {
		return nil
	}
	operations := make([]contentmoderation.BindingOperation, 0, len(channelIDs))
	for _, channelID := range channelIDs {
		if channelID = strings.TrimSpace(channelID); channelID != "" {
			operations = append(operations, contentmoderation.BindingOperation{ChannelType: channelType, ChannelID: channelID})
		}
	}
	if len(operations) == 0 {
		return nil
	}
	return h.contentModerationStore().PatchBindings(ctx, tenantID, false, operations)
}

func (h *Handler) GetContentModerationMetrics(c *gin.Context) {
	runtime := contentmoderation.Runtime()
	if runtime == nil {
		c.JSON(http.StatusOK, contentmoderation.ModerationMetrics{})
		return
	}
	// Tenant-scoped only: never expose process-wide counters across tenants.
	c.JSON(http.StatusOK, runtime.MetricsForTenant(effectiveTenantID(c)))
}

func (h *Handler) GetContentModerationProfiles(c *gin.Context) {
	tenantID := effectiveTenantID(c)
	profiles, err := h.contentModerationStore().ListProfiles(c.Request.Context(), tenantID)
	if err != nil {
		contentModerationError(c, err)
		return
	}
	counts, err := h.contentModerationStore().BindingCounts(c.Request.Context(), tenantID)
	if err != nil {
		contentModerationError(c, err)
		return
	}
	items := make([]contentModerationProfileResponse, 0, len(profiles))
	for _, profile := range profiles {
		items = append(items, contentModerationProfileView(profile, counts[profile.ID]))
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "capabilities": contentModerationProfileCapabilities()})
}

// contentModerationProfileCapabilities lists the profile fields this build
// stores beyond the original set. The panel updates itself from the latest
// release and may run against an older backend, which silently drops fields it
// does not know, so it only offers a field listed here.
func contentModerationProfileCapabilities() gin.H {
	return gin.H{"keyword_fold_traditional_chinese": true, "api_includes_tool_output": true}
}

func (h *Handler) PostContentModerationProfile(c *gin.Context) {
	var input contentmoderation.CreateProfileInput
	if err := c.ShouldBindJSON(&input); err != nil {
		contentModerationBadRequest(c, err.Error())
		return
	}
	profile, err := contentmoderation.NewProfile(effectiveTenantID(c), uuid.NewString(), input, time.Now())
	if err != nil {
		contentModerationBadRequest(c, err.Error())
		return
	}
	if err = h.contentModerationStore().CreateProfile(c.Request.Context(), profile); err != nil {
		contentModerationError(c, err)
		return
	}
	c.JSON(http.StatusCreated, contentModerationProfileView(profile, map[string]int{}))
}

func (h *Handler) GetContentModerationProfile(c *gin.Context) {
	tenantID := effectiveTenantID(c)
	profile, err := h.contentModerationStore().GetProfile(c.Request.Context(), tenantID, c.Param("id"))
	if err != nil {
		contentModerationError(c, err)
		return
	}
	counts, err := h.contentModerationStore().BindingCounts(c.Request.Context(), tenantID)
	if err != nil {
		contentModerationError(c, err)
		return
	}
	c.JSON(http.StatusOK, contentModerationProfileView(profile, counts[profile.ID]))
}

func (h *Handler) PatchContentModerationProfile(c *gin.Context) {
	var input contentmoderation.PatchProfileInput
	if err := c.ShouldBindJSON(&input); err != nil {
		contentModerationBadRequest(c, err.Error())
		return
	}
	store := h.contentModerationStore()
	profile, err := store.GetProfile(c.Request.Context(), effectiveTenantID(c), c.Param("id"))
	if err != nil {
		contentModerationError(c, err)
		return
	}
	updated, err := contentmoderation.ApplyProfilePatch(profile, input, time.Now())
	if err != nil {
		contentModerationError(c, err)
		return
	}
	if err = store.UpdateProfile(c.Request.Context(), updated, profile.Version); err != nil {
		contentModerationError(c, err)
		return
	}
	counts, err := store.BindingCounts(c.Request.Context(), updated.TenantID)
	if err != nil {
		contentModerationError(c, err)
		return
	}
	c.JSON(http.StatusOK, contentModerationProfileView(updated, counts[updated.ID]))
}

func (h *Handler) DeleteContentModerationProfile(c *gin.Context) {
	if err := h.contentModerationStore().DeleteProfile(c.Request.Context(), effectiveTenantID(c), c.Param("id")); err != nil {
		contentModerationError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) GetContentModerationChannels(c *gin.Context) {
	tenantID := effectiveTenantID(c)
	bindings, err := h.contentModerationStore().ListBindings(c.Request.Context(), tenantID)
	if err != nil {
		contentModerationError(c, err)
		return
	}
	cfg := h.providerConfigForTenant(c)
	var auths []*coreauth.Auth
	if h.authManager != nil {
		auths = h.authManager.ListForTenant(tenantID)
	}
	page, pageSize := queryPositiveInt(c, "page", 1), queryPositiveInt(c, "page_size", 20)
	tags := splitQueryValues(c.Query("tags"))
	result := contentmoderation.ListChannels(cfg, auths, bindings, contentmoderation.ChannelQuery{
		ChannelType: c.Query("channel_type"),
		Query:       c.Query("query"),
		Tags:        tags,
		TagMode:     c.Query("tag_mode"),
		Provider:    c.Query("provider"),
		ProfileID:   c.Query("profile_id"),
		Page:        page,
		PageSize:    pageSize,
	})
	c.JSON(http.StatusOK, result)
}

func (h *Handler) PatchContentModerationChannelBindings(c *gin.Context) {
	var body struct {
		AllowRebind bool                                 `json:"allow_rebind"`
		Operations  []contentmoderation.BindingOperation `json:"operations"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || len(body.Operations) == 0 {
		contentModerationBadRequest(c, "operations are required")
		return
	}
	tenantID := effectiveTenantID(c)
	cfg := h.providerConfigForTenant(c)
	var auths []*coreauth.Auth
	if h.authManager != nil {
		auths = h.authManager.ListForTenant(tenantID)
	}
	for _, operation := range body.Operations {
		if operation.ProfileID == nil || strings.TrimSpace(*operation.ProfileID) == "" {
			continue
		}
		if !contentmoderation.ChannelExists(cfg, auths, operation.ChannelType, operation.ChannelID) {
			contentModerationBadRequest(c, "channel does not exist in the effective tenant")
			return
		}
	}
	store := h.contentModerationStore()
	if err := store.PatchBindings(c.Request.Context(), tenantID, body.AllowRebind, body.Operations); err != nil {
		contentModerationError(c, err)
		return
	}
	bindings, err := store.ListBindings(c.Request.Context(), tenantID)
	if err != nil {
		contentModerationError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"bindings": bindings})
}

func contentModerationProfileView(profile contentmoderation.Profile, counts map[string]int) contentModerationProfileResponse {
	if counts == nil {
		counts = map[string]int{}
	}
	return contentModerationProfileResponse{
		Profile:          profile,
		APIKeyConfigured: strings.TrimSpace(profile.APIKeySecret) != "",
		APIKeyMasked:     contentmoderation.MaskSecret(profile.APIKeySecret),
		BindingCounts:    counts,
	}
}

func contentModerationError(c *gin.Context, err error) {
	status, code := http.StatusInternalServerError, "content_moderation_error"
	extra := gin.H{}
	switch {
	case errors.Is(err, contentmoderation.ErrNotFound):
		status, code = http.StatusNotFound, "content_moderation_not_found"
	case errors.Is(err, contentmoderation.ErrNameConflict):
		status, code = http.StatusConflict, "content_moderation_name_conflict"
	case errors.Is(err, contentmoderation.ErrVersionConflict):
		status, code = http.StatusConflict, "content_moderation_version_conflict"
	case errors.Is(err, contentmoderation.ErrProfileBound):
		status, code = http.StatusConflict, "content_moderation_profile_bound"
		var bound *contentmoderation.ProfileBoundError
		if errors.As(err, &bound) {
			extra["binding_count"] = bound.Count
		}
	case errors.Is(err, contentmoderation.ErrBindingConflict):
		status, code = http.StatusConflict, "content_moderation_binding_conflict"
		var conflict *contentmoderation.BindingConflictError
		if errors.As(err, &conflict) {
			extra["channel_type"] = conflict.ChannelType
			extra["channel_id"] = conflict.ChannelID
			extra["existing_profile_id"] = conflict.ExistingProfileID
		}
	case errors.Is(err, contentmoderation.ErrInvalidChannel):
		status, code = http.StatusBadRequest, "content_moderation_invalid_channel"
	case errors.Is(err, contentmoderation.ErrUnavailable):
		status, code = http.StatusServiceUnavailable, "content_moderation_unavailable"
	}
	payload := gin.H{"code": code, "message": err.Error()}
	for key, value := range extra {
		payload[key] = value
	}
	c.JSON(status, gin.H{"error": payload})
}

func contentModerationBadRequest(c *gin.Context, message string) {
	c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "content_moderation_invalid_request", "message": message}})
}

func queryPositiveInt(c *gin.Context, key string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(c.Query(key)))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func splitQueryValues(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
