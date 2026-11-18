package management

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/contentmoderation"
)

const providerModerationBindingSnapshotKey = "management.provider_moderation_binding_snapshot"

func captureProviderModerationBindingSnapshot(c *gin.Context, cfg *config.Config) {
	if c == nil {
		return
	}
	if _, exists := c.Get(providerModerationBindingSnapshotKey); exists {
		return
	}
	refs := contentmoderation.ProviderChannelRefs(cfg)
	c.Set(providerModerationBindingSnapshotKey, refs)
}

func (h *Handler) cleanupRemovedProviderModerationBindings(ctx context.Context, c *gin.Context, tenantID string, cfg *config.Config) error {
	if c == nil {
		return nil
	}
	value, exists := c.Get(providerModerationBindingSnapshotKey)
	if !exists {
		return nil
	}
	before, ok := value.([]contentmoderation.ChannelRef)
	if !ok || len(before) == 0 {
		return nil
	}
	after := make(map[string]struct{})
	for _, ref := range contentmoderation.ProviderChannelRefs(cfg) {
		after[ref.ChannelType+"\x00"+ref.ChannelID] = struct{}{}
	}
	byType := make(map[string][]string)
	for _, ref := range before {
		if _, exists := after[ref.ChannelType+"\x00"+ref.ChannelID]; !exists {
			byType[ref.ChannelType] = append(byType[ref.ChannelType], ref.ChannelID)
		}
	}
	for channelType, ids := range byType {
		if err := h.deleteContentModerationBindings(ctx, tenantID, channelType, ids); err != nil {
			return err
		}
	}
	return nil
}
