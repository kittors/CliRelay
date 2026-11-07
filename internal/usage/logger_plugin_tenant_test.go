package usage

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestLoggerPluginUsesTrustedTenantBeforeAPIKeyLookup(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})

	const (
		businessTenantID = "00000000-0000-0000-0000-00000000000a"
		apiKeyLabel      = "POST /image-generation/test"
		businessModel    = "image-generation-business-tenant-test"
		systemModel      = "image-generation-system-tenant-test"
		realAPIKey       = "sk-real-tenant-fallback"
		realAPIKeyModel  = "ordinary-api-key-tenant-test"
		realTenantID     = "00000000-0000-0000-0000-00000000000b"
	)
	plugin := NewLoggerPlugin()
	now := time.Now().UTC()

	plugin.HandleUsage(context.Background(), coreusage.Record{
		TrustedTenantID: businessTenantID,
		APIKey:          apiKeyLabel,
		Model:           businessModel,
		RequestedAt:     now,
		Detail:          coreusage.Detail{TotalTokens: 1},
	})
	assertRequestLogTenantAndAPIKey(t, businessModel, businessTenantID, apiKeyLabel)

	var polluted int
	if err := getDB().QueryRow(`SELECT COUNT(*) FROM request_logs WHERE tenant_id = ? AND model = ?`, systemTenantID, businessModel).Scan(&polluted); err != nil {
		t.Fatalf("count system tenant pollution: %v", err)
	}
	if polluted != 0 {
		t.Fatalf("business image test wrote %d system tenant rows, want 0", polluted)
	}

	plugin.HandleUsage(context.Background(), coreusage.Record{
		TrustedTenantID: systemTenantID,
		APIKey:          apiKeyLabel,
		Model:           systemModel,
		RequestedAt:     now.Add(time.Second),
		Detail:          coreusage.Detail{TotalTokens: 1},
	})
	assertRequestLogTenantAndAPIKey(t, systemModel, systemTenantID, apiKeyLabel)

	if err := UpsertAPIKeyForTenant(realTenantID, APIKeyRow{
		ID:   "real-tenant-fallback-key",
		Key:  realAPIKey,
		Name: "Real tenant fallback",
	}); err != nil {
		t.Fatalf("UpsertAPIKeyForTenant: %v", err)
	}
	plugin.HandleUsage(context.Background(), coreusage.Record{
		APIKey:      realAPIKey,
		Model:       realAPIKeyModel,
		RequestedAt: now.Add(2 * time.Second),
		Detail:      coreusage.Detail{TotalTokens: 1},
	})
	// The tenant comes from the key's row, and the row stores only its mask.
	assertRequestLogTenantAndAPIKey(t, realAPIKeyModel, realTenantID, MaskAPIKey(realAPIKey))
}

func assertRequestLogTenantAndAPIKey(t *testing.T, model, wantTenantID, wantAPIKey string) {
	t.Helper()
	var tenantID, apiKey string
	if err := getDB().QueryRow(`SELECT tenant_id, api_key FROM request_logs WHERE model = ?`, model).Scan(&tenantID, &apiKey); err != nil {
		t.Fatalf("query request log for %s: %v", model, err)
	}
	if tenantID != wantTenantID {
		t.Fatalf("tenant_id for %s = %q, want %q", model, tenantID, wantTenantID)
	}
	if apiKey != wantAPIKey {
		t.Fatalf("api_key for %s = %q, want %q", model, apiKey, wantAPIKey)
	}
}
