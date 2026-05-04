package config

import "strings"

const (
	DefaultBedrockRegion  = "us-east-1"
	BedrockAuthModeSigV4  = "sigv4"
	BedrockAuthModeAPIKey = "api-key"
)

// BedrockKey represents an AWS Bedrock Runtime credential.
// It supports both AWS SigV4 credentials and Bedrock API key bearer auth.
type BedrockKey struct {
	// Name is a human-readable label for this channel.
	Name string `yaml:"name,omitempty" json:"name,omitempty"`

	// Priority controls selection preference when multiple credentials match.
	// Higher values are preferred; defaults to 0.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Prefix optionally namespaces model aliases for this credential.
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`

	// AuthMode selects credential handling: "sigv4" (default) or "api-key".
	AuthMode string `yaml:"auth-mode,omitempty" json:"auth-mode,omitempty"`

	// APIKey is used when AuthMode is "api-key" and maps to Authorization: Bearer.
	APIKey string `yaml:"api-key,omitempty" json:"api-key,omitempty"`

	// AccessKeyID is the AWS access key id used for SigV4 signing.
	AccessKeyID string `yaml:"access-key-id,omitempty" json:"access-key-id,omitempty"`

	// SecretAccessKey is the AWS secret access key used for SigV4 signing.
	SecretAccessKey string `yaml:"secret-access-key,omitempty" json:"secret-access-key,omitempty"`

	// SessionToken is an optional AWS session token for temporary credentials.
	SessionToken string `yaml:"session-token,omitempty" json:"session-token,omitempty"`

	// Region is the Bedrock Runtime region. Empty defaults to us-east-1.
	Region string `yaml:"region,omitempty" json:"region,omitempty"`

	// ForceGlobal maps supported Claude aliases to the global Bedrock prefix.
	ForceGlobal bool `yaml:"force-global,omitempty" json:"force-global,omitempty"`

	// BaseURL optionally overrides the Bedrock Runtime endpoint.
	BaseURL string `yaml:"base-url,omitempty" json:"base-url,omitempty"`

	// ProxyURL overrides the global proxy setting for this credential.
	ProxyURL string `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`

	// ProxyID references a reusable proxy-pool entry. When valid, it takes precedence over ProxyURL.
	ProxyID string `yaml:"proxy-id,omitempty" json:"proxy-id,omitempty"`

	// Models defines upstream model names and aliases for request routing.
	Models []BedrockModel `yaml:"models,omitempty" json:"models,omitempty"`

	// Headers optionally adds extra HTTP headers for requests sent with this credential.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// ExcludedModels lists model IDs that should be excluded for this credential.
	ExcludedModels []string `yaml:"excluded-models,omitempty" json:"excluded-models,omitempty"`
}

func (k BedrockKey) GetAPIKey() string {
	if strings.EqualFold(strings.TrimSpace(k.AuthMode), BedrockAuthModeAPIKey) {
		return k.APIKey
	}
	if strings.TrimSpace(k.APIKey) != "" {
		return k.APIKey
	}
	return k.AccessKeyID
}

func (k BedrockKey) GetBaseURL() string { return k.BaseURL }

// BedrockModel describes an upstream Bedrock/Claude model and a client-facing alias.
type BedrockModel struct {
	// Name is the upstream model or Claude alias used by the executor.
	Name string `yaml:"name" json:"name"`

	// Alias is the client-facing model name.
	Alias string `yaml:"alias" json:"alias"`
}

func (m BedrockModel) GetName() string  { return m.Name }
func (m BedrockModel) GetAlias() string { return m.Alias }

// SanitizeBedrockKeys deduplicates and normalizes AWS Bedrock credentials.
func (cfg *Config) SanitizeBedrockKeys() {
	if cfg == nil {
		return
	}

	seen := make(map[string]struct{}, len(cfg.BedrockKey))
	out := cfg.BedrockKey[:0]
	for i := range cfg.BedrockKey {
		entry := cfg.BedrockKey[i]
		entry.Name = strings.TrimSpace(entry.Name)
		entry.Prefix = normalizeModelPrefix(entry.Prefix)
		entry.AuthMode = normalizeBedrockAuthMode(entry.AuthMode)
		entry.APIKey = strings.TrimSpace(entry.APIKey)
		entry.AccessKeyID = strings.TrimSpace(entry.AccessKeyID)
		entry.SecretAccessKey = strings.TrimSpace(entry.SecretAccessKey)
		entry.SessionToken = strings.TrimSpace(entry.SessionToken)
		entry.Region = strings.TrimSpace(entry.Region)
		if entry.Region == "" {
			entry.Region = DefaultBedrockRegion
		}
		entry.BaseURL = strings.TrimSpace(entry.BaseURL)
		entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
		entry.ProxyID = strings.TrimSpace(entry.ProxyID)
		entry.Headers = NormalizeHeaders(entry.Headers)
		entry.ExcludedModels = NormalizeExcludedModels(entry.ExcludedModels)
		entry.Models = sanitizeBedrockModels(entry.Models)

		var credentialKey string
		if entry.AuthMode == BedrockAuthModeAPIKey {
			if entry.APIKey == "" {
				continue
			}
			credentialKey = strings.Join([]string{entry.AuthMode, entry.APIKey, entry.Region, entry.BaseURL}, "|")
		} else {
			if entry.AccessKeyID == "" || entry.SecretAccessKey == "" {
				continue
			}
			credentialKey = strings.Join([]string{entry.AuthMode, entry.AccessKeyID, entry.SecretAccessKey, entry.SessionToken, entry.Region, entry.BaseURL}, "|")
		}
		if _, exists := seen[credentialKey]; exists {
			continue
		}
		seen[credentialKey] = struct{}{}
		out = append(out, entry)
	}
	cfg.BedrockKey = out
}

func normalizeBedrockAuthMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "apikey", "api_key", BedrockAuthModeAPIKey:
		return BedrockAuthModeAPIKey
	default:
		return BedrockAuthModeSigV4
	}
}

func sanitizeBedrockModels(models []BedrockModel) []BedrockModel {
	if len(models) == 0 {
		return nil
	}
	out := make([]BedrockModel, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		model.Name = strings.TrimSpace(model.Name)
		model.Alias = strings.TrimSpace(model.Alias)
		if model.Name == "" && model.Alias == "" {
			continue
		}
		key := strings.ToLower(model.Name) + "|" + strings.ToLower(model.Alias)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, model)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
