package config

// ClaudeKey represents the configuration for a Claude API key,
// including the API key itself and an optional base URL for the API endpoint.
type ClaudeKey struct {
	// APIKey is the authentication key for accessing Claude API services.
	APIKey string `yaml:"api-key" json:"api-key"`

	// Name is a human-readable label for this channel.
	Name string `yaml:"name,omitempty" json:"name,omitempty"`

	// Priority controls selection preference when multiple credentials match.
	// Higher values are preferred; defaults to 0.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Prefix optionally namespaces models for this credential (e.g., "teamA/claude-sonnet-4").
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`

	// BaseURL is the base URL for the Claude API endpoint.
	// If empty, the default Claude API URL will be used.
	BaseURL string `yaml:"base-url" json:"base-url"`

	// ProxyURL overrides the global proxy setting for this API key if provided.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

	// ProxyID references a reusable proxy-pool entry. When valid, it takes precedence over ProxyURL.
	ProxyID string `yaml:"proxy-id,omitempty" json:"proxy-id,omitempty"`

	// Models defines upstream model names and aliases for request routing.
	Models []ClaudeModel `yaml:"models" json:"models"`

	// Headers optionally adds extra HTTP headers for requests sent with this key.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// ExcludedModels lists model IDs that should be excluded for this provider.
	ExcludedModels []string `yaml:"excluded-models,omitempty" json:"excluded-models,omitempty"`

	// Cloak configures request cloaking for non-Claude-Code clients.
	Cloak *CloakConfig `yaml:"cloak,omitempty" json:"cloak,omitempty"`

	// SkipAnthropicProcessing disables Anthropic-specific request processing
	// (cloaking, cache_control injection, tool prefix, thinking constraints).
	// Enable this when the base URL points to a third-party Claude-compatible API.
	// Default: false (Anthropic processing enabled).
	SkipAnthropicProcessing bool `yaml:"skip-anthropic-processing,omitempty" json:"skip-anthropic-processing,omitempty"`
}

func (k ClaudeKey) GetAPIKey() string  { return k.APIKey }
func (k ClaudeKey) GetBaseURL() string { return k.BaseURL }

// ClaudeModel describes a mapping between an alias and the actual upstream model name.
type ClaudeModel struct {
	// Name is the upstream model identifier used when issuing requests.
	Name string `yaml:"name" json:"name"`

	// Alias is the client-facing model name that maps to Name.
	Alias string `yaml:"alias" json:"alias"`
}

func (m ClaudeModel) GetName() string  { return m.Name }
func (m ClaudeModel) GetAlias() string { return m.Alias }

// CodexKey represents the configuration for a Codex API key,
// including the API key itself and an optional base URL for the API endpoint.
type CodexKey struct {
	// APIKey is the authentication key for accessing Codex API services.
	APIKey string `yaml:"api-key" json:"api-key"`

	// Name is a human-readable label for this channel.
	Name string `yaml:"name,omitempty" json:"name,omitempty"`

	// Priority controls selection preference when multiple credentials match.
	// Higher values are preferred; defaults to 0.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Prefix optionally namespaces models for this credential (e.g., "teamA/gpt-5-codex").
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`

	// BaseURL is the base URL for the Codex API endpoint.
	// If empty, the default Codex API URL will be used.
	BaseURL string `yaml:"base-url" json:"base-url"`

	// Websockets enables the Responses API websocket transport for this credential.
	Websockets bool `yaml:"websockets,omitempty" json:"websockets,omitempty"`

	// ProxyURL overrides the global proxy setting for this API key if provided.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

	// ProxyID references a reusable proxy-pool entry. When valid, it takes precedence over ProxyURL.
	ProxyID string `yaml:"proxy-id,omitempty" json:"proxy-id,omitempty"`

	// Models defines upstream model names and aliases for request routing.
	Models []CodexModel `yaml:"models" json:"models"`

	// Headers optionally adds extra HTTP headers for requests sent with this key.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// ExcludedModels lists model IDs that should be excluded for this provider.
	ExcludedModels []string `yaml:"excluded-models,omitempty" json:"excluded-models,omitempty"`
}

func (k CodexKey) GetAPIKey() string  { return k.APIKey }
func (k CodexKey) GetBaseURL() string { return k.BaseURL }

// CodexModel describes a mapping between an alias and the actual upstream model name.
type CodexModel struct {
	// Name is the upstream model identifier used when issuing requests.
	Name string `yaml:"name" json:"name"`

	// Alias is the client-facing model name that maps to Name.
	Alias string `yaml:"alias" json:"alias"`
}

func (m CodexModel) GetName() string  { return m.Name }
func (m CodexModel) GetAlias() string { return m.Alias }

// GeminiKey represents the configuration for a Gemini API key,
// including optional overrides for upstream base URL, proxy routing, and headers.
type GeminiKey struct {
	// APIKey is the authentication key for accessing Gemini API services.
	APIKey string `yaml:"api-key" json:"api-key"`

	// Name is a human-readable label for this channel.
	Name string `yaml:"name,omitempty" json:"name,omitempty"`

	// Priority controls selection preference when multiple credentials match.
	// Higher values are preferred; defaults to 0.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Prefix optionally namespaces models for this credential (e.g., "teamA/gemini-3-pro-preview").
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`

	// BaseURL optionally overrides the Gemini API endpoint.
	BaseURL string `yaml:"base-url,omitempty" json:"base-url,omitempty"`

	// ProxyURL optionally overrides the global proxy for this API key.
	ProxyURL string `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`

	// ProxyID references a reusable proxy-pool entry. When valid, it takes precedence over ProxyURL.
	ProxyID string `yaml:"proxy-id,omitempty" json:"proxy-id,omitempty"`

	// Models defines upstream model names and aliases for request routing.
	Models []GeminiModel `yaml:"models,omitempty" json:"models,omitempty"`

	// Headers optionally adds extra HTTP headers for requests sent with this key.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// ExcludedModels lists model IDs that should be excluded for this provider.
	ExcludedModels []string `yaml:"excluded-models,omitempty" json:"excluded-models,omitempty"`
}

func (k GeminiKey) GetAPIKey() string  { return k.APIKey }
func (k GeminiKey) GetBaseURL() string { return k.BaseURL }

// GeminiModel describes a mapping between an alias and the actual upstream model name.
type GeminiModel struct {
	// Name is the upstream model identifier used when issuing requests.
	Name string `yaml:"name" json:"name"`

	// Alias is the client-facing model name that maps to Name.
	Alias string `yaml:"alias" json:"alias"`
}

func (m GeminiModel) GetName() string  { return m.Name }
func (m GeminiModel) GetAlias() string { return m.Alias }

// OpenAICompatibility represents the configuration for OpenAI API compatibility
// with external providers, allowing model aliases to be routed through OpenAI API format.
type OpenAICompatibility struct {
	// Name is the identifier for this OpenAI compatibility configuration.
	Name string `yaml:"name" json:"name"`

	// Disabled marks this provider as inactive while preserving its configuration.
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`

	// Priority controls selection preference when multiple providers or credentials match.
	// Higher values are preferred; defaults to 0.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Prefix optionally namespaces model aliases for this provider (e.g., "teamA/kimi-k2").
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`

	// BaseURL is the base URL for the external OpenAI-compatible API endpoint.
	BaseURL string `yaml:"base-url" json:"base-url"`

	// APIKeyEntries defines API keys with optional per-key proxy configuration.
	APIKeyEntries []OpenAICompatibilityAPIKey `yaml:"api-key-entries,omitempty" json:"api-key-entries,omitempty"`

	// Models defines the model configurations including aliases for routing.
	Models []OpenAICompatibilityModel `yaml:"models" json:"models"`

	// Headers optionally adds extra HTTP headers for requests sent to this provider.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
}

// OpenAICompatibilityAPIKey represents an API key configuration with optional proxy setting.
type OpenAICompatibilityAPIKey struct {
	// APIKey is the authentication key for accessing the external API services.
	APIKey string `yaml:"api-key" json:"api-key"`

	// Disabled marks this API key entry as inactive while preserving the config.
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`

	// ProxyURL overrides the global proxy setting for this API key if provided.
	ProxyURL string `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`

	// ProxyID references a reusable proxy-pool entry. When valid, it takes precedence over ProxyURL.
	ProxyID string `yaml:"proxy-id,omitempty" json:"proxy-id,omitempty"`
}

// OpenAICompatibilityModel represents a model configuration for OpenAI compatibility,
// including the actual model name and its alias for API routing.
type OpenAICompatibilityModel struct {
	// Name is the actual model name used by the external provider.
	Name string `yaml:"name" json:"name"`

	// Alias is the model name alias that clients will use to reference this model.
	Alias string `yaml:"alias" json:"alias"`
}

func (m OpenAICompatibilityModel) GetName() string  { return m.Name }
func (m OpenAICompatibilityModel) GetAlias() string { return m.Alias }

// OpenCodeGoKey represents an OpenCode Go plan API key.
// The upstream endpoint is fixed to https://opencode.ai/zen/go/v1.
type OpenCodeGoKey struct {
	// APIKey is the authentication key for OpenCode Go.
	APIKey string `yaml:"api-key" json:"api-key"`

	// Name is a human-readable label for this channel.
	Name string `yaml:"name,omitempty" json:"name,omitempty"`

	// Priority controls selection preference when multiple credentials match.
	// Higher values are preferred; defaults to 0.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Prefix optionally namespaces models for this credential.
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`

	// ProxyURL overrides the global proxy setting for this API key if provided.
	ProxyURL string `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`

	// ProxyID references a reusable proxy-pool entry. When valid, it takes precedence over ProxyURL.
	ProxyID string `yaml:"proxy-id,omitempty" json:"proxy-id,omitempty"`

	// Models defines the OpenCode Go model IDs explicitly enabled for this key.
	Models []OpenCodeGoModel `yaml:"models,omitempty" json:"models,omitempty"`

	// Headers optionally adds extra HTTP headers for requests sent with this key.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// ExcludedModels lists model IDs that should be excluded for this provider.
	ExcludedModels []string `yaml:"excluded-models,omitempty" json:"excluded-models,omitempty"`

	// VisionFallbackModel is used for image requests whose requested model lacks vision support.
	VisionFallbackModel string `yaml:"vision-fallback-model,omitempty" json:"vision-fallback-model,omitempty"`

	// WorkspaceID identifies the OpenCode workspace used for dashboard usage checks.
	WorkspaceID string `yaml:"-" json:"workspace-id,omitempty"`

	// AuthCookie stores the OpenCode dashboard auth cookie value used for usage checks.
	AuthCookie string `yaml:"-" json:"auth-cookie,omitempty"`
}

// OpenCodeGoModel describes a model explicitly enabled for OpenCode Go routing.
type OpenCodeGoModel struct {
	Name string `yaml:"name" json:"name"`
}

func (m OpenCodeGoModel) GetName() string  { return m.Name }
func (m OpenCodeGoModel) GetAlias() string { return "" }

const DefaultClineBaseURL = "https://api.cline.bot/api/v1"

// ClineKey represents a ClinePass OpenAI-compatible API key.
type ClineKey struct {
	APIKey string `yaml:"api-key" json:"api-key"`

	// Name is a human-readable label for this channel.
	Name string `yaml:"name,omitempty" json:"name,omitempty"`

	// Priority controls selection preference when multiple credentials match.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Prefix optionally namespaces models for this credential.
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`

	// BaseURL is the OpenAI-compatible base URL. Defaults to DefaultClineBaseURL.
	BaseURL string `yaml:"base-url,omitempty" json:"base-url,omitempty"`

	// ProxyURL overrides the global proxy setting for this API key if provided.
	ProxyURL string `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`

	// ProxyID references a reusable proxy-pool entry. When valid, it takes precedence over ProxyURL.
	ProxyID string `yaml:"proxy-id,omitempty" json:"proxy-id,omitempty"`

	// Models defines upstream Cline model IDs and optional client-facing aliases.
	Models []ClineModel `yaml:"models,omitempty" json:"models,omitempty"`

	// Headers optionally adds extra HTTP headers for requests sent with this key.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// ExcludedModels lists model IDs that should be excluded for this provider.
	ExcludedModels []string `yaml:"excluded-models,omitempty" json:"excluded-models,omitempty"`

	// VisionFallbackModel is used for image requests whose requested model lacks vision support.
	VisionFallbackModel string `yaml:"vision-fallback-model,omitempty" json:"vision-fallback-model,omitempty"`
}

type ClineModel struct {
	Name  string `yaml:"name" json:"name"`
	Alias string `yaml:"alias,omitempty" json:"alias,omitempty"`
}

func (m ClineModel) GetName() string  { return m.Name }
func (m ClineModel) GetAlias() string { return m.Alias }
