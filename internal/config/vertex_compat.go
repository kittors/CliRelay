package config

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// VertexCompatKey represents the configuration for Vertex AI-compatible API keys.
// This supports third-party services that use Vertex AI-style endpoint paths
// (/publishers/google/models/{model}:streamGenerateContent) but authenticate
// with simple API keys instead of Google Cloud service account credentials.
//
// Example services: zenmux.ai and similar Vertex-compatible providers.
type VertexCompatKey struct {
	// ID is the immutable persistent UUID used by channel bindings.
	ID string `yaml:"id,omitempty" json:"id,omitempty"`

	// APIKey is the authentication key for accessing the Vertex-compatible API.
	// Maps to the x-goog-api-key header.
	APIKey string `yaml:"api-key" json:"api-key"`

	// Priority controls selection preference when multiple credentials match.
	// Higher values are preferred; defaults to 0.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Weight controls proportional selection under weighted round robin.
	Weight *int `yaml:"weight,omitempty" json:"weight,omitempty"`

	// DisableCooling overrides the global disable-cooling switch for this
	// channel: true keeps a 429 or 5xx from taking it out of rotation, false
	// keeps cooldowns on even while the global switch is off, and leaving it
	// unset follows the global switch.
	DisableCooling *bool `yaml:"disable-cooling,omitempty" json:"disable-cooling,omitempty"`

	// Prefix optionally namespaces model aliases for this credential (e.g., "teamA/vertex-pro").
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`

	// BaseURL is the base URL for the Vertex-compatible API endpoint.
	// The executor will append "/v1/publishers/google/models/{model}:action" to this.
	// Example: "https://zenmux.ai/api" becomes "https://zenmux.ai/api/v1/publishers/google/models/..."
	BaseURL string `yaml:"base-url,omitempty" json:"base-url,omitempty"`

	// ProxyURL optionally overrides the global proxy for this API key.
	ProxyURL string `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`

	// ProxyID references a reusable proxy-pool entry. When valid, it takes precedence over ProxyURL.
	ProxyID string `yaml:"proxy-id,omitempty" json:"proxy-id,omitempty"`

	// ConnectionPool configures upstream connection reuse for this credential.
	ConnectionPool *ProxyConnectionPoolConfig `yaml:"connection-pool,omitempty" json:"connection-pool,omitempty"`

	// Interactions enables native Google Interactions API routing for this key.
	Interactions *bool `yaml:"interactions,omitempty" json:"interactions,omitempty"`

	// OpenAPI selects the Vertex OpenAPI-compatible request shape for this key.
	OpenAPI *bool `yaml:"openapi,omitempty" json:"openapi,omitempty"`

	// OpenAPILocation overrides the regional Vertex OpenAPI endpoint location.
	OpenAPILocation string `yaml:"openapi-location,omitempty" json:"openapi-location,omitempty"`

	// Headers optionally adds extra HTTP headers for requests sent with this key.
	// Commonly used for cookies, user-agent, and other authentication headers.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// Models defines the model configurations including aliases for routing.
	Models []VertexCompatModel `yaml:"models,omitempty" json:"models,omitempty"`

	// ExcludedModels lists model IDs that should be excluded for this credential.
	ExcludedModels []string `yaml:"excluded-models,omitempty" json:"excluded-models,omitempty"`

	// RequestRetry optionally overrides the global retry count for this credential.
	RequestRetry *int `yaml:"request-retry,omitempty" json:"request-retry,omitempty"`
}

func (k VertexCompatKey) GetAPIKey() string   { return k.APIKey }
func (k VertexCompatKey) GetBaseURL() string  { return k.BaseURL }
func (k VertexCompatKey) GetPrefix() string   { return k.Prefix }
func (k VertexCompatKey) GetProxyURL() string { return k.ProxyURL }

// VertexCompatModel represents a model configuration for Vertex compatibility,
// including the actual model name and its alias for API routing.
type VertexCompatModel struct {
	// Name is the actual model name used by the external provider.
	Name string `yaml:"name" json:"name"`

	// Alias is the model name alias that clients will use to reference this model.
	Alias        string                    `yaml:"alias" json:"alias"`
	DisplayName  string                    `yaml:"display-name,omitempty" json:"display-name,omitempty"`
	ForceMapping bool                      `yaml:"force-mapping,omitempty" json:"force-mapping,omitempty"`
	OpenAPI      *bool                     `yaml:"openapi,omitempty" json:"openapi,omitempty"`
	Thinking     *registry.ThinkingSupport `yaml:"thinking,omitempty" json:"thinking,omitempty"`
}

func (m VertexCompatModel) GetName() string                        { return m.Name }
func (m VertexCompatModel) GetAlias() string                       { return m.Alias }
func (m VertexCompatModel) GetDisplayName() string                 { return m.DisplayName }
func (m VertexCompatModel) GetForceMapping() bool                  { return m.ForceMapping }
func (m VertexCompatModel) GetThinking() *registry.ThinkingSupport { return m.Thinking }
func (m VertexCompatModel) GetOpenAPI() *bool                      { return m.OpenAPI }

// SanitizeVertexCompatKeys deduplicates and normalizes Vertex-compatible API key credentials.
func (cfg *Config) SanitizeVertexCompatKeys() {
	if cfg == nil {
		return
	}

	seen := make(map[string]struct{}, len(cfg.VertexCompatAPIKey))
	out := cfg.VertexCompatAPIKey[:0]
	for i := range cfg.VertexCompatAPIKey {
		entry := cfg.VertexCompatAPIKey[i]
		entry.APIKey = strings.TrimSpace(entry.APIKey)
		if entry.APIKey == "" {
			continue
		}
		entry.Prefix = normalizeModelPrefix(entry.Prefix)
		entry.BaseURL = strings.TrimSpace(entry.BaseURL)
		if entry.BaseURL == "" {
			// BaseURL is required for Vertex API key entries
			continue
		}
		entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
		entry.ProxyID = strings.TrimSpace(entry.ProxyID)
		entry.Headers = NormalizeHeaders(entry.Headers)

		// Sanitize models: remove entries without valid alias
		sanitizedModels := make([]VertexCompatModel, 0, len(entry.Models))
		for _, model := range entry.Models {
			model.Alias = strings.TrimSpace(model.Alias)
			model.Name = strings.TrimSpace(model.Name)
			if model.Alias != "" && model.Name != "" {
				sanitizedModels = append(sanitizedModels, model)
			}
		}
		entry.Models = sanitizedModels

		// Use API key + base URL as uniqueness key
		uniqueKey := entry.APIKey + "|" + entry.BaseURL
		if _, exists := seen[uniqueKey]; exists {
			continue
		}
		seen[uniqueKey] = struct{}{}
		out = append(out, entry)
	}
	cfg.VertexCompatAPIKey = out
}
