// Package config provides configuration management for the CLI Proxy API server.
// It handles loading and parsing YAML configuration files, and provides structured
// access to application settings including server port, authentication directory,
// debug settings, proxy configuration, and API keys.
package config

import "time"

const DefaultMainAPIReadTimeout = 2 * time.Minute

// Config represents the application's configuration, loaded from a YAML file.
type Config struct {
	SDKConfig `yaml:",inline"`
	// Host is the network host/interface on which the API server will bind.
	// Default is empty ("") to bind all interfaces (IPv4 + IPv6). Use "127.0.0.1" or "localhost" for local-only access.
	Host string `yaml:"host" json:"-"`
	// Port is the network port on which the API server will listen.
	Port int `yaml:"port" json:"-"`

	// MainAPIReadTimeoutSeconds controls how long the main API server may spend reading
	// request headers and body from downstream clients before timing out.
	// When unset or <= 0, DefaultMainAPIReadTimeout is used.
	MainAPIReadTimeoutSeconds int `yaml:"main-api-read-timeout-seconds,omitempty" json:"main-api-read-timeout-seconds,omitempty"`

	// Timezone configures the project's timezone (IANA name, e.g. "Asia/Shanghai").
	// It affects "today" boundaries and day-based aggregation in monitoring/usage pages.
	// When empty, the process local timezone (time.Local) is used.
	Timezone string `yaml:"timezone,omitempty" json:"timezone,omitempty"`

	// Redis config controls the Redis connection for usage persistence.
	Redis RedisConfig `yaml:"redis" json:"redis"`

	// TLS config controls HTTPS server settings.
	TLS TLSConfig `yaml:"tls" json:"tls"`

	// CORSAllowOrigins defines the explicit browser origins allowed to call API routes cross-origin.
	// Leave empty to disable cross-origin browser access by default.
	CORSAllowOrigins []string `yaml:"cors-allow-origins" json:"cors-allow-origins"`

	// TrustedProxies lists reverse proxy IPs or CIDRs whose forwarding headers may be trusted.
	// Leave empty to ignore X-Forwarded-For/X-Real-IP and use the direct peer address.
	TrustedProxies []string `yaml:"trusted-proxies,omitempty" json:"trusted-proxies,omitempty"`

	// RemoteManagement nests management-related options under 'remote-management'.
	RemoteManagement RemoteManagement `yaml:"remote-management" json:"-"`

	// AutoUpdate controls Docker-first update checks and updater sidecar integration.
	AutoUpdate AutoUpdateConfig `yaml:"auto-update" json:"auto-update"`

	// OAuthClients stores optional OAuth client credentials used by provider login flows.
	// When empty, the runtime may fall back to environment variables (see oauth_clients.go).
	OAuthClients OAuthClients `yaml:"oauth-clients" json:"-"`

	// OAuthUserAgent sets the User-Agent header for OAuth HTTP requests.
	// Some providers may reject the default Go HTTP client User-Agent.
	// When empty, a browser-like default is used.
	OAuthUserAgent string `yaml:"oauth-user-agent" json:"oauth-user-agent"`

	// AuthDir is the directory where authentication token files are stored.
	AuthDir string `yaml:"auth-dir" json:"-"`

	// Debug enables or disables debug-level logging and other debug features.
	Debug bool `yaml:"debug" json:"debug"`

	// Pprof config controls the optional pprof HTTP debug server.
	Pprof PprofConfig `yaml:"pprof" json:"pprof"`

	// CommercialMode disables high-overhead HTTP middleware features to minimize per-request memory usage.
	CommercialMode bool `yaml:"commercial-mode" json:"commercial-mode"`

	// LoggingToFile controls whether application logs are written to rotating files or stdout.
	LoggingToFile bool `yaml:"logging-to-file" json:"logging-to-file"`

	// LogsMaxTotalSizeMB limits the total size (in MB) of log files under the logs directory.
	// When exceeded, the oldest log files are deleted until within the limit. Set to 0 to disable.
	LogsMaxTotalSizeMB int `yaml:"logs-max-total-size-mb" json:"logs-max-total-size-mb"`

	// ErrorLogsMaxFiles limits the number of error log files retained when request logging is disabled.
	// When exceeded, the oldest error log files are deleted. Default is 10. Set to 0 to disable cleanup.
	ErrorLogsMaxFiles int `yaml:"error-logs-max-files" json:"error-logs-max-files"`

	// UsageStatisticsEnabled toggles in-memory usage aggregation; when false, usage data is discarded.
	UsageStatisticsEnabled bool `yaml:"usage-statistics-enabled" json:"usage-statistics-enabled"`

	// DisableCooling disables quota cooldown scheduling when true.
	DisableCooling bool `yaml:"disable-cooling" json:"disable-cooling"`

	// RequestRetry defines the retry times when the request failed.
	RequestRetry int `yaml:"request-retry" json:"request-retry"`
	// MaxRetryInterval defines the maximum wait time in seconds before retrying a cooled-down credential.
	MaxRetryInterval int `yaml:"max-retry-interval" json:"max-retry-interval"`

	// QuotaExceeded defines the behavior when a quota is exceeded.
	QuotaExceeded QuotaExceeded `yaml:"quota-exceeded" json:"quota-exceeded"`

	// Routing controls credential selection behavior.
	Routing RoutingConfig `yaml:"routing" json:"routing"`

	// WebsocketAuth enables or disables authentication for the WebSocket API.
	WebsocketAuth bool `yaml:"ws-auth" json:"ws-auth"`

	// GeminiKey defines Gemini API key configurations with optional routing overrides.
	GeminiKey []GeminiKey `yaml:"gemini-api-key" json:"gemini-api-key"`

	// Codex defines a list of Codex API key configurations as specified in the YAML configuration file.
	CodexKey []CodexKey `yaml:"codex-api-key" json:"codex-api-key"`

	// ClaudeKey defines a list of Claude API key configurations as specified in the YAML configuration file.
	ClaudeKey []ClaudeKey `yaml:"claude-api-key" json:"claude-api-key"`

	// BedrockKey defines AWS Bedrock Runtime credential configurations.
	BedrockKey []BedrockKey `yaml:"bedrock-api-key" json:"bedrock-api-key"`

	// OpenCodeGoKey defines OpenCode Go plan API key configurations.
	OpenCodeGoKey []OpenCodeGoKey `yaml:"opencode-go-api-key" json:"opencode-go-api-key"`

	// ClaudeHeaderDefaults configures default header values for Claude API requests.
	// These are used as fallbacks when the client does not send its own headers.
	ClaudeHeaderDefaults ClaudeHeaderDefaults `yaml:"claude-header-defaults" json:"claude-header-defaults"`

	// KimiHeaderDefaults configures default header values for Kimi API requests.
	// These control how requests appear in the Kimi console (e.g., User-Agent as source).
	KimiHeaderDefaults KimiHeaderDefaults `yaml:"kimi-header-defaults" json:"kimi-header-defaults"`

	// IdentityFingerprint controls provider-specific upstream identity headers.
	IdentityFingerprint IdentityFingerprintConfig `yaml:"identity-fingerprint,omitempty" json:"identity-fingerprint,omitempty"`

	// ProxyPool stores reusable outbound proxies that can be referenced by providers and auth files.
	ProxyPool []ProxyPoolEntry `yaml:"proxy-pool,omitempty" json:"proxy-pool,omitempty"`

	// OpenAICompatibility defines OpenAI API compatibility configurations for external providers.
	OpenAICompatibility []OpenAICompatibility `yaml:"openai-compatibility" json:"openai-compatibility"`

	// VertexCompatAPIKey defines Vertex AI-compatible API key configurations for third-party providers.
	// Used for services that use Vertex AI-style paths but with simple API key authentication.
	VertexCompatAPIKey []VertexCompatKey `yaml:"vertex-api-key" json:"vertex-api-key"`

	// AmpCode contains Amp CLI upstream configuration, management restrictions, and model mappings.
	AmpCode AmpCode `yaml:"ampcode" json:"ampcode"`

	// OAuthExcludedModels defines per-provider global model exclusions applied to OAuth/file-backed auth entries.
	OAuthExcludedModels map[string][]string `yaml:"oauth-excluded-models,omitempty" json:"oauth-excluded-models,omitempty"`

	// OAuthModelAlias defines global model name aliases for OAuth/file-backed auth channels.
	// These aliases affect both model listing and model routing for supported channels:
	// gemini-cli, vertex, aistudio, antigravity, claude, codex, qwen, iflow.
	//
	// NOTE: This does not apply to existing per-credential model alias features under:
	// gemini-api-key, codex-api-key, claude-api-key, openai-compatibility, vertex-api-key, and ampcode.
	OAuthModelAlias map[string][]OAuthModelAlias `yaml:"oauth-model-alias,omitempty" json:"oauth-model-alias,omitempty"`

	// Payload defines default and override rules for provider payload parameters.
	Payload PayloadConfig `yaml:"payload" json:"payload"`

	legacyMigrationPending bool `yaml:"-" json:"-"`
}

func (cfg *Config) MainAPIReadTimeout() time.Duration {
	if cfg == nil || cfg.MainAPIReadTimeoutSeconds <= 0 {
		return DefaultMainAPIReadTimeout
	}
	return time.Duration(cfg.MainAPIReadTimeoutSeconds) * time.Second
}

// ClaudeHeaderDefaults configures default header values injected into Claude API requests
// when the client does not send them. Update these when Claude Code releases a new version.
type ClaudeHeaderDefaults struct {
	UserAgent      string `yaml:"user-agent" json:"user-agent"`
	PackageVersion string `yaml:"package-version" json:"package-version"`
	RuntimeVersion string `yaml:"runtime-version" json:"runtime-version"`
	Timeout        string `yaml:"timeout" json:"timeout"`
}

// KimiHeaderDefaults configures default header values for Kimi API requests.
// These headers identify the client to the Kimi API and affect how requests
// appear in the Kimi console (e.g., User-Agent shows as the source).
type KimiHeaderDefaults struct {
	UserAgent string `yaml:"user-agent" json:"user-agent"`
	Platform  string `yaml:"platform" json:"platform"`
	Version   string `yaml:"version" json:"version"`
}
