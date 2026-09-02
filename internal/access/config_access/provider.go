package configaccess

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

// Register ensures the config-access provider is available to the access manager.
func Register(cfg *sdkconfig.SDKConfig) {
	if cfg == nil {
		sdkaccess.UnregisterProvider(sdkaccess.AccessProviderTypeConfigAPIKey)
		return
	}

	keyConfigs := buildKeyConfigMap(cfg)
	if len(keyConfigs) == 0 {
		sdkaccess.UnregisterProvider(sdkaccess.AccessProviderTypeConfigAPIKey)
		return
	}

	sdkaccess.RegisterProvider(
		sdkaccess.AccessProviderTypeConfigAPIKey,
		newProvider(sdkaccess.DefaultAccessProviderName, keyConfigs),
	)
}

// buildKeyConfigMap builds a map from API key to its full configuration.
// Primary source: management/settings/apikey service backed by PostgreSQL.
// Fallback: legacy APIKeys and APIKeyEntries from YAML config.
func buildKeyConfigMap(cfg *sdkconfig.SDKConfig) map[string]keyConfig {
	result := make(map[string]keyConfig)

	// Primary: preload tenant profiles and owned accounts once, then build every key.
	storedRows := usage.ListAllAPIKeys()
	profilesByTenant := make(map[string][]usage.APIKeyPermissionProfileRow)
	endUserIDs := make([]string, 0)
	for _, stored := range storedRows {
		tenantID := strings.TrimSpace(stored.TenantID)
		if _, ok := profilesByTenant[tenantID]; !ok {
			profilesByTenant[tenantID] = usage.ListAPIKeyPermissionProfilesForTenant(tenantID)
		}
		if endUserID := strings.TrimSpace(stored.EndUserID); endUserID != "" {
			endUserIDs = append(endUserIDs, endUserID)
		}
	}
	accounts, _ := usage.ListEndUserQuotasByIDs(endUserIDs)
	for _, stored := range storedRows {
		profiles := profilesByTenant[strings.TrimSpace(stored.TenantID)]
		entry := usage.EffectiveAPIKeyRowWithProfiles(stored, profiles)
		trimmed := strings.TrimSpace(entry.Key)
		if trimmed == "" || entry.Disabled {
			continue
		}
		var account *usage.EndUserQuota
		if endUserID := strings.TrimSpace(entry.EndUserID); endUserID != "" {
			loaded, ok := accounts[endUserID]
			if !ok {
				fallback := usage.GetEndUserQuota(endUserID)
				if fallback == nil {
					continue
				}
				loaded = *fallback
			}
			effective := usage.EffectiveEndUserQuotaWithProfiles(loaded, profiles)
			if strings.TrimSpace(effective.Status) != "active" {
				continue
			}
			account = &effective
		}
		if _, exists := result[trimmed]; exists {
			continue
		}
		result[trimmed] = keyConfigFromRowWithAccount(entry, account)
	}

	// Fallback: YAML config (for backward compatibility during migration)
	profiles := usage.ListAPIKeyPermissionProfiles()
	for _, entry := range cfg.APIKeyEntries {
		row := usage.EffectiveAPIKeyRowWithProfiles(usage.APIKeyRowFromConfig(entry), profiles)
		trimmed := strings.TrimSpace(entry.Key)
		if trimmed == "" || entry.Disabled {
			continue
		}
		if _, exists := result[trimmed]; exists {
			continue
		}
		result[trimmed] = keyConfigFromRow(row)
	}
	for _, k := range cfg.APIKeys {
		trimmed := strings.TrimSpace(k)
		if trimmed == "" {
			continue
		}
		if _, exists := result[trimmed]; exists {
			continue
		}
		result[trimmed] = keyConfig{}
	}
	return result
}

// keyConfig holds the per-key configuration extracted from APIKeyEntry.
// When endUserID is set, quota/permissions come from the end-user account pool.
type keyConfig struct {
	tenantID                    string
	apiKeyID                    string
	apiKeyName                  string
	endUserID                   string
	allowedModels               []string
	allowedChannels             []string
	allowedChannelGroups        []string
	dailyLimit                  int
	totalQuota                  int
	spendingLimit               float64
	dailySpendingLimit          float64
	accountPeriodSpendingLimits quota.PeriodSpendingLimits
	keyPeriodSpendingLimits     quota.PeriodSpendingLimits
	concurrencyLimit            int
	rpmLimit                    int
	tpmLimit                    int
	systemPrompt                string
}

func keyConfigFromRow(row usage.APIKeyRow) keyConfig {
	var account *usage.EndUserQuota
	if endUserID := strings.TrimSpace(row.EndUserID); endUserID != "" {
		if loaded := usage.GetEndUserQuota(endUserID); loaded != nil {
			effective := usage.EffectiveEndUserQuota(*loaded)
			account = &effective
		}
	}
	return keyConfigFromRowWithAccount(row, account)
}

func keyConfigFromRowWithAccount(row usage.APIKeyRow, account *usage.EndUserQuota) keyConfig {
	tenantID := strings.TrimSpace(row.TenantID)
	if tenantID == "" {
		tenantID = identity.SystemTenantID
	}
	kc := keyConfig{
		tenantID: tenantID, apiKeyID: strings.TrimSpace(row.ID), apiKeyName: strings.TrimSpace(row.Name),
		endUserID: strings.TrimSpace(row.EndUserID), allowedModels: row.AllowedModels,
		allowedChannels: row.AllowedChannels, allowedChannelGroups: row.AllowedChannelGroups,
		dailyLimit: row.DailyLimit, totalQuota: row.TotalQuota, spendingLimit: row.SpendingLimit,
		dailySpendingLimit: row.DailySpendingLimit, keyPeriodSpendingLimits: row.PeriodSpendingLimits,
		concurrencyLimit: row.ConcurrencyLimit, rpmLimit: row.RPMLimit, tpmLimit: row.TPMLimit, systemPrompt: row.SystemPrompt,
	}
	if kc.endUserID != "" && account != nil {
		if name := strings.TrimSpace(account.DisplayName); name != "" {
			kc.apiKeyName = name
		}
		kc.allowedModels = account.AllowedModels
		kc.allowedChannels = account.AllowedChannels
		kc.allowedChannelGroups = account.AllowedChannelGroups
		kc.dailyLimit = account.DailyLimit
		kc.totalQuota = account.TotalQuota
		kc.spendingLimit = account.SpendingLimit
		kc.dailySpendingLimit = account.DailySpendingLimit
		kc.accountPeriodSpendingLimits = account.PeriodSpendingLimits
		kc.concurrencyLimit = account.ConcurrencyLimit
		kc.rpmLimit = account.RPMLimit
		kc.tpmLimit = account.TPMLimit
		kc.systemPrompt = account.SystemPrompt
	}
	kc.keyPeriodSpendingLimits.Day = row.DailySpendingLimit
	if kc.endUserID == "" {
		kc.dailySpendingLimit = kc.keyPeriodSpendingLimits.Day
	}
	return kc
}

type provider struct {
	name string
	keys map[string]keyConfig
}

func newProvider(name string, keyConfigs map[string]keyConfig) *provider {
	providerName := strings.TrimSpace(name)
	if providerName == "" {
		providerName = sdkaccess.DefaultAccessProviderName
	}
	return &provider{name: providerName, keys: keyConfigs}
}

func (p *provider) Identifier() string {
	if p == nil || p.name == "" {
		return sdkaccess.DefaultAccessProviderName
	}
	return p.name
}

func (p *provider) Authenticate(_ context.Context, r *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	if p == nil {
		return nil, sdkaccess.NewNotHandledError()
	}
	if len(p.keys) == 0 {
		return nil, sdkaccess.NewNotHandledError()
	}
	authHeader := r.Header.Get("Authorization")
	authHeaderGoogle := r.Header.Get("X-Goog-Api-Key")
	authHeaderAnthropic := r.Header.Get("X-Api-Key")
	queryKey := ""
	queryAuthToken := ""
	if r.URL != nil {
		queryKey = r.URL.Query().Get("key")
		queryAuthToken = r.URL.Query().Get("auth_token")
	}
	if authHeader == "" && authHeaderGoogle == "" && authHeaderAnthropic == "" && queryKey == "" && queryAuthToken == "" {
		return nil, sdkaccess.NewNoCredentialsError()
	}

	apiKey := extractBearerToken(authHeader)

	candidates := []struct {
		value  string
		source string
	}{
		{apiKey, "authorization"},
		{authHeaderGoogle, "x-goog-api-key"},
		{authHeaderAnthropic, "x-api-key"},
		{queryKey, "query-key"},
		{queryAuthToken, "query-auth-token"},
	}

	for _, candidate := range candidates {
		if candidate.value == "" {
			continue
		}
		if kc, ok := p.keys[candidate.value]; ok {
			metadata := map[string]string{
				"source":     candidate.source,
				"tenant-id":  kc.tenantID,
				"api-key-id": kc.apiKeyID,
			}
			if kc.endUserID != "" {
				metadata["end-user-id"] = kc.endUserID
			}
			if len(kc.allowedModels) > 0 {
				metadata["allowed-models"] = strings.Join(kc.allowedModels, ",")
			}
			if len(kc.allowedChannels) > 0 {
				metadata["allowed-channels"] = strings.Join(kc.allowedChannels, ",")
			}
			if len(kc.allowedChannelGroups) > 0 {
				metadata["allowed-channel-groups"] = strings.Join(kc.allowedChannelGroups, ",")
			}
			if kc.dailyLimit > 0 {
				metadata["daily-limit"] = fmt.Sprintf("%d", kc.dailyLimit)
			}
			if kc.totalQuota > 0 {
				metadata["total-quota"] = fmt.Sprintf("%d", kc.totalQuota)
			}
			if kc.concurrencyLimit > 0 {
				metadata["concurrency-limit"] = fmt.Sprintf("%d", kc.concurrencyLimit)
			}
			if kc.rpmLimit > 0 {
				metadata["rpm-limit"] = fmt.Sprintf("%d", kc.rpmLimit)
			}
			if kc.tpmLimit > 0 {
				metadata["tpm-limit"] = fmt.Sprintf("%d", kc.tpmLimit)
			}
			if kc.spendingLimit > 0 {
				metadata["spending-limit"] = fmt.Sprintf("%f", kc.spendingLimit)
			}
			if kc.dailySpendingLimit > 0 {
				metadata["daily-spending-limit"] = fmt.Sprintf("%f", kc.dailySpendingLimit)
			}
			addPeriodLimitMetadata(metadata, "account-period-spending-limit-", kc.accountPeriodSpendingLimits)
			addPeriodLimitMetadata(metadata, "key-period-spending-limit-", kc.keyPeriodSpendingLimits)
			if kc.systemPrompt != "" {
				metadata["system-prompt"] = kc.systemPrompt
			}
			return &sdkaccess.Result{
				Provider:   p.Identifier(),
				Principal:  candidate.value,
				TenantID:   kc.tenantID,
				APIKeyID:   kc.apiKeyID,
				APIKeyName: kc.apiKeyName,
				Metadata:   metadata,
			}, nil
		}
	}

	return nil, sdkaccess.NewInvalidCredentialError()
}

func addPeriodLimitMetadata(metadata map[string]string, prefix string, limits quota.PeriodSpendingLimits) {
	for _, period := range quota.OrderedPeriods {
		if value := limits.Value(period); value > 0 {
			metadata[prefix+string(period)] = fmt.Sprintf("%f", value)
		}
	}
}

func extractBearerToken(header string) string {
	if header == "" {
		return ""
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 {
		return header
	}
	if strings.ToLower(parts[0]) != "bearer" {
		return header
	}
	return strings.TrimSpace(parts[1])
}
