package configaccess

import (
	"context"
	"net/http"
	"strings"

	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

// Register ensures the config-access provider is available to the access manager.
func Register(cfg *sdkconfig.SDKConfig) {
	if cfg == nil {
		sdkaccess.UnregisterProvider(sdkaccess.AccessProviderTypeConfigAPIKey)
		return
	}

	keyModels := buildKeyModelsMap(cfg)
	if len(keyModels) == 0 {
		sdkaccess.UnregisterProvider(sdkaccess.AccessProviderTypeConfigAPIKey)
		return
	}

	sdkaccess.RegisterProvider(
		sdkaccess.AccessProviderTypeConfigAPIKey,
		newProvider(sdkaccess.DefaultAccessProviderName, keyModels),
	)
}

// buildKeyModelsMap builds a map from API key to allowed models list.
// Keys from both APIKeys (legacy, no restrictions) and APIKeyEntries are included.
func buildKeyModelsMap(cfg *sdkconfig.SDKConfig) map[string][]string {
	result := make(map[string][]string)
	// APIKeyEntries first — they have the more specific config with AllowedModels
	for _, entry := range cfg.APIKeyEntries {
		trimmed := strings.TrimSpace(entry.Key)
		if trimmed == "" || entry.Disabled {
			continue
		}
		if _, exists := result[trimmed]; exists {
			continue
		}
		result[trimmed] = entry.AllowedModels
	}
	// Legacy APIKeys — no model restrictions (empty slice = all models allowed)
	for _, k := range cfg.APIKeys {
		trimmed := strings.TrimSpace(k)
		if trimmed == "" {
			continue
		}
		if _, exists := result[trimmed]; exists {
			continue
		}
		result[trimmed] = nil
	}
	return result
}

type provider struct {
	name string
	keys map[string][]string // key -> allowed models (nil/empty = all models)
}

func newProvider(name string, keyModels map[string][]string) *provider {
	providerName := strings.TrimSpace(name)
	if providerName == "" {
		providerName = sdkaccess.DefaultAccessProviderName
	}
	return &provider{name: providerName, keys: keyModels}
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
		if allowedModels, ok := p.keys[candidate.value]; ok {
			metadata := map[string]string{
				"source": candidate.source,
			}
			if len(allowedModels) > 0 {
				metadata["allowed-models"] = strings.Join(allowedModels, ",")
			}
			return &sdkaccess.Result{
				Provider:  p.Identifier(),
				Principal: candidate.value,
				Metadata:  metadata,
			}, nil
		}
	}

	return nil, sdkaccess.NewInvalidCredentialError()
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
