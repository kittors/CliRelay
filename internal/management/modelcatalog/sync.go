package modelcatalog

import (
	"context"

	modelconfigsettings "github.com/router-for-me/CLIProxyAPI/v8/internal/management/settings/modelconfig"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

// External sync contract:
// - Owner: external model catalog synchronization boundary.
// - Responsibility: surface OpenRouter sync state, settings updates, and manual sync execution.

func (s *Service) OpenRouterModelSyncState() usage.OpenRouterModelSyncState {
	return modelconfigsettings.GetOpenRouterSyncStateForTenant(s.tenantID)
}

func (s *Service) UpdateOpenRouterModelSyncSettings(enabled bool, intervalMinutes int) (usage.OpenRouterModelSyncState, error) {
	return modelconfigsettings.UpdateOpenRouterSyncSettingsForTenant(s.tenantID, enabled, intervalMinutes)
}

func (s *Service) RunOpenRouterModelSync(ctx context.Context) (usage.OpenRouterModelSyncResult, usage.OpenRouterModelSyncState, error) {
	return modelconfigsettings.RunOpenRouterSyncForTenant(ctx, s.tenantID)
}
