package usage

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
	sqlapikey "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/sqlstore/apikey"
)

// ConfigCollectionVersion returns the version of a whole-replace collection
// (configsync.DomainAPIKeys, DomainProxyPool, ...) of tenantID, 0 when it has
// never been written. Management reads report it so a client can send it back
// with a full replacement.
func ConfigCollectionVersion(domain, tenantID string) int64 {
	return configsync.CollectionVersion(context.Background(), getDB(), domain, normalizeTenantID(tenantID))
}

// ReplaceAllAPIKeysForTenantExpect replaces the tenant's API keys if the
// collection is still at expected (configsync.AnyVersion: unchecked).
func ReplaceAllAPIKeysForTenantExpect(ctx context.Context, tenantID string, entries []APIKeyRow, expected int64) (int64, error) {
	return apiKeyStoreForTenant(tenantID).ReplaceAllExpect(ctx, entries, expected)
}

// ReplaceAllAPIKeyPermissionProfilesForTenantExpect replaces the tenant's
// permission profiles if the collection is still at expected.
func ReplaceAllAPIKeyPermissionProfilesForTenantExpect(ctx context.Context, tenantID string, profiles []APIKeyPermissionProfileRow, syncEndUsers bool, expected int64) (sqlapikey.PermissionProfileSyncResult, int64, error) {
	return apiKeyPermissionProfileStoreForTenant(tenantID).ReplaceAllPermissionProfilesExpect(ctx, profiles, syncEndUsers, expected)
}

// ReplaceProxyPoolForTenantExpect replaces the tenant's proxy pool if the
// collection is still at expected.
func ReplaceProxyPoolForTenantExpect(ctx context.Context, tenantID string, entries []config.ProxyPoolEntry, expected int64) (int64, error) {
	return proxyPoolStoreForTenant(tenantID).ReplaceExpect(ctx, entries, expected)
}

// ReplaceModelOwnerPresetsForTenantExpect replaces the tenant's owner presets
// if the collection is still at expected.
func ReplaceModelOwnerPresetsForTenantExpect(ctx context.Context, tenantID string, rows []ModelOwnerPresetRow, expected int64) (int64, error) {
	return modelConfigStoreForTenant(tenantID).ReplaceModelOwnerPresetsExpect(ctx, rows, expected)
}
