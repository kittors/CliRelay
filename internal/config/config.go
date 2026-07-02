// Package config provides configuration management for the CLI Proxy API server.
// It handles loading and parsing YAML configuration files, and provides structured
// access to application settings including server port, authentication directory,
// debug settings, proxy configuration, and API keys.
package config

import (
	"strings"
	"time"
)

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

	// RequestBody controls request-body limits for public model API endpoints.
	RequestBody RequestBodyConfig `yaml:"request-body,omitempty" json:"request-body,omitempty"`

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

	// UsageStatisticsEnabled toggles in-memory usage aggregation; request log
	// metadata is still persisted so monitoring/history pages keep working.
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

	// ClineKey defines ClinePass API key configurations.
	ClineKey []ClineKey `yaml:"cline-api-key" json:"cline-api-key"`

	// ClaudeHeaderDefaults configures default header values for Claude API requests.
	// These are used as fallbacks when the client does not send its own headers.
	ClaudeHeaderDefaults ClaudeHeaderDefaults `yaml:"claude-header-defaults" json:"claude-header-defaults"`

	// KimiHeaderDefaults configures default header values for Kimi API requests.
	// These control how requests appear in the Kimi console (e.g., User-Agent as source).
	KimiHeaderDefaults KimiHeaderDefaults `yaml:"kimi-header-defaults" json:"kimi-header-defaults"`

	// IdentityFingerprint controls provider-specific upstream identity headers.
	IdentityFingerprint IdentityFingerprintConfig `yaml:"identity-fingerprint,omitempty" json:"identity-fingerprint,omitempty"`

	// CodexOAuthAdmission controls global fixed allowed-client presets for Codex OAuth accounts.
	CodexOAuthAdmission CodexOAuthAdmissionConfig `yaml:"codex-oauth-admission,omitempty" json:"codex-oauth-admission,omitempty"`

	// ProxyPool stores reusable outbound proxies that can be referenced by providers and auth files.
	ProxyPool []ProxyPoolEntry `yaml:"proxy-pool,omitempty" json:"proxy-pool,omitempty"`

	// ProxyWarmup configures proactive upstream connection warming for fixed proxy hosts.
	// When enabled, the server establishes and maintains idle TLS/HTTP2 connections
	// to common upstream AI API hosts through the fixed residential proxy.
	ProxyWarmup ProxyWarmConfig `yaml:"proxy-warmup,omitempty" json:"proxy-warmup,omitempty"`

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

// RequestBodyConfig controls public model API request-body handling.
type RequestBodyConfig struct {
	// ModelMaxMB is the maximum decoded request body size for model endpoints.
	// Management and upload endpoints keep their narrower endpoint-specific limits.
	ModelMaxMB int `yaml:"model-max-mb,omitempty" json:"model-max-mb,omitempty"`
	// DiskThresholdMB is the decoded body size above which reusable request
	// bodies spill to a temporary file instead of staying cached in memory.
	DiskThresholdMB int `yaml:"disk-threshold-mb,omitempty" json:"disk-threshold-mb,omitempty"`
	// CacheDir optionally overrides the dedicated request-body temp file directory.
	// When empty, the runtime uses the OS temp directory under a CliRelay-specific subdirectory.
	CacheDir string `yaml:"cache-dir,omitempty" json:"cache-dir,omitempty"`
}

// ModelRequestBodyLimitBytes returns the decoded body limit for model endpoints.
func (cfg *Config) ModelRequestBodyLimitBytes() int64 {
	maxMB := DefaultModelRequestBodyMB
	if cfg != nil && cfg.RequestBody.ModelMaxMB > 0 {
		maxMB = cfg.RequestBody.ModelMaxMB
	}
	return int64(maxMB) << 20
}

// RequestBodyDiskThresholdBytes returns the memory threshold for reusable body storage.
func (cfg *Config) RequestBodyDiskThresholdBytes() int64 {
	thresholdMB := DefaultRequestBodyDiskThresholdMB
	if cfg != nil && cfg.RequestBody.DiskThresholdMB > 0 {
		thresholdMB = cfg.RequestBody.DiskThresholdMB
	}
	return int64(thresholdMB) << 20
}

// RequestBodyCacheDir returns the optional request-body temp cache directory.
func (cfg *Config) RequestBodyCacheDir() string {
	if cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg.RequestBody.CacheDir)
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

// ProxyWarmConfig controls proactive upstream connection warming for fixed proxy hosts.
// Warming establishes idle TLS/HTTP2 connections through the proxy before real requests arrive.
type ProxyWarmConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// ProxyID references a proxy-pool entry to warm. It takes precedence over ProxyURL.
	ProxyID string `yaml:"proxy-id,omitempty" json:"proxy-id,omitempty"`
	// ProxyURL warms this explicit proxy when ProxyID is empty.
	ProxyURL string `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`
	// StartupDelaySeconds waits this long after server start before beginning warmup.
	StartupDelaySeconds int `yaml:"startup-delay-seconds,omitempty" json:"startup-delay-seconds,omitempty"`
	// IntervalSeconds is the base interval between warmup rounds for active hosts.
	IntervalSeconds int `yaml:"interval-seconds,omitempty" json:"interval-seconds,omitempty"`
	// IntervalJitterSeconds adds random jitter to warmup interval to avoid fixed patterns.
	IntervalJitterSeconds int `yaml:"interval-jitter-seconds,omitempty" json:"interval-jitter-seconds,omitempty"`
	// TimeoutSeconds per individual warmup request.
	TimeoutSeconds int `yaml:"timeout-seconds,omitempty" json:"timeout-seconds,omitempty"`
	// InactiveTTLMinutes removes a host from warmup if unused for this long.
	InactiveTTLMinutes int `yaml:"inactive-ttl-minutes,omitempty" json:"inactive-ttl-minutes,omitempty"`
	// MaxHostsPerProxy limits the number of concurrently warmed hosts.
	MaxHostsPerProxy int `yaml:"max-hosts-per-proxy,omitempty" json:"max-hosts-per-proxy,omitempty"`
	// AllowedHostSuffixes restricts warming to matching host suffixes (e.g. chatgpt.com).
	AllowedHostSuffixes []string `yaml:"allowed-host-suffixes,omitempty" json:"allowed-host-suffixes,omitempty"`
	// Targets lists the specific URLs to warm for each host.
	Targets []ProxyWarmTarget `yaml:"targets,omitempty" json:"targets,omitempty"`
}

// ProxyWarmTarget defines a single warmup request target.
type ProxyWarmTarget struct {
	Host   string `yaml:"host" json:"host"`
	URL    string `yaml:"url" json:"url"`
	Method string `yaml:"method" json:"method"`
}

func defaultProxyWarmConfig() ProxyWarmConfig {
	return ProxyWarmConfig{
		Enabled:               false,
		StartupDelaySeconds:   15,
		IntervalSeconds:       60,
		IntervalJitterSeconds: 15,
		TimeoutSeconds:        5,
		InactiveTTLMinutes:    10,
		MaxHostsPerProxy:      8,
		AllowedHostSuffixes: []string{
			"chatgpt.com",
			"openai.com",
			"oaistatic.com",
			"oaiusercontent.com",
		},
		Targets: []ProxyWarmTarget{
			{Host: "chatgpt.com", URL: "https://chatgpt.com/robots.txt", Method: "GET"},
			{Host: "api.openai.com", URL: "https://api.openai.com/", Method: "HEAD"},
			{Host: "chat.openai.com", URL: "https://chat.openai.com/", Method: "HEAD"},
		},
	}
}

func (cfg *Config) SanitizeProxyWarmup() {
	if cfg == nil {
		return
	}
	defaults := defaultProxyWarmConfig()
	warm := cfg.ProxyWarmup
	warm.ProxyID = strings.TrimSpace(warm.ProxyID)
	warm.ProxyURL = strings.TrimSpace(warm.ProxyURL)
	if warm.StartupDelaySeconds <= 0 {
		warm.StartupDelaySeconds = defaults.StartupDelaySeconds
	}
	if warm.IntervalSeconds <= 0 {
		warm.IntervalSeconds = defaults.IntervalSeconds
	}
	if warm.IntervalJitterSeconds < 0 {
		warm.IntervalJitterSeconds = 0
	}
	if warm.TimeoutSeconds <= 0 {
		warm.TimeoutSeconds = defaults.TimeoutSeconds
	}
	if warm.InactiveTTLMinutes <= 0 {
		warm.InactiveTTLMinutes = defaults.InactiveTTLMinutes
	}
	if warm.MaxHostsPerProxy <= 0 {
		warm.MaxHostsPerProxy = defaults.MaxHostsPerProxy
	}
	if len(warm.AllowedHostSuffixes) == 0 {
		warm.AllowedHostSuffixes = defaults.AllowedHostSuffixes
	} else {
		out := make([]string, 0, len(warm.AllowedHostSuffixes))
		for _, suffix := range warm.AllowedHostSuffixes {
			if trimmed := strings.ToLower(strings.TrimSpace(suffix)); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		warm.AllowedHostSuffixes = out
	}
	if len(warm.Targets) == 0 {
		warm.Targets = defaults.Targets
	} else {
		out := make([]ProxyWarmTarget, 0, len(warm.Targets))
		for _, target := range warm.Targets {
			target.Host = strings.ToLower(strings.TrimSpace(target.Host))
			target.URL = strings.TrimSpace(target.URL)
			target.Method = strings.ToUpper(strings.TrimSpace(target.Method))
			if target.Method == "" {
				target.Method = "HEAD"
			}
			if target.Host == "" || target.URL == "" {
				continue
			}
			out = append(out, target)
		}
		warm.Targets = out
	}
	cfg.ProxyWarmup = warm
}
