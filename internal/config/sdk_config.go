// Package config provides configuration management for the CLI Proxy API server.
// It handles loading and parsing YAML configuration files, and provides structured
// access to application settings including server port, authentication directory,
// debug settings, proxy configuration, and API keys.
package config

// SDKConfig represents the application's configuration, loaded from a YAML file.
type SDKConfig struct {
	// ProxyURL is the URL of an optional proxy server to use for outbound requests.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

	// ForceModelPrefix requires explicit model prefixes (e.g., "teamA/gemini-3-pro-preview")
	// to target prefixed credentials. When false, unprefixed model requests may use prefixed
	// credentials as well.
	ForceModelPrefix bool `yaml:"force-model-prefix" json:"force-model-prefix"`

	// RequestLog enables or disables detailed request logging functionality.
	RequestLog bool `yaml:"request-log" json:"request-log"`

	// RequestLogStorage controls how full request/response bodies are retained.
	RequestLogStorage RequestLogStorageConfig `yaml:"request-log-storage" json:"request-log-storage"`

	// APIKeys is a list of keys for authenticating clients to this proxy server.
	APIKeys []string `yaml:"api-keys" json:"api-keys"`

	// AllowUnauthenticated controls whether client API requests are allowed when
	// no authentication providers (API keys) are configured. Default is false
	// (secure-by-default).
	AllowUnauthenticated bool `yaml:"allow-unauthenticated" json:"allow-unauthenticated"`

	// APIKeyEntries is a list of API key entries with metadata for advanced management.
	// Keys from both APIKeys and APIKeyEntries are valid for authentication.
	APIKeyEntries []APIKeyEntry `yaml:"api-key-entries,omitempty" json:"api-key-entries,omitempty"`

	// PassthroughHeaders controls whether upstream response headers are forwarded to downstream clients.
	// Default is false (disabled).
	PassthroughHeaders bool `yaml:"passthrough-headers" json:"passthrough-headers"`

	// Streaming configures server-side streaming behavior (keep-alives and safe bootstrap retries).
	Streaming StreamingConfig `yaml:"streaming" json:"streaming"`

	// NonStreamKeepAliveInterval controls how often blank lines are emitted for non-streaming responses.
	// <= 0 disables keep-alives. Value is in seconds.
	NonStreamKeepAliveInterval int `yaml:"nonstream-keepalive-interval,omitempty" json:"nonstream-keepalive-interval,omitempty"`

	// Observability controls optional high-cardinality diagnostic logs.
	Observability ObservabilityConfig `yaml:"observability,omitempty" json:"observability,omitempty"`

	// ContextRetrieval reduces oversized conversational payloads using local retrieval.
	ContextRetrieval ContextRetrievalConfig `yaml:"context-retrieval,omitempty" json:"context-retrieval,omitempty"`
}

// ContextRetrievalConfig controls local SQLite FTS based context reduction.
type ContextRetrievalConfig struct {
	Enabled             bool                         `yaml:"enabled" json:"enabled"`
	Models              []PayloadModelRule           `yaml:"models,omitempty" json:"models,omitempty"`
	MaxInputBytes       int                          `yaml:"max-input-bytes,omitempty" json:"max-input-bytes,omitempty"`
	PreserveRecentTurns int                          `yaml:"preserve-recent-turns,omitempty" json:"preserve-recent-turns,omitempty"`
	Chunk               ContextRetrievalChunkConfig  `yaml:"chunk,omitempty" json:"chunk,omitempty"`
	Retrieval           ContextRetrievalSearchConfig `yaml:"retrieval,omitempty" json:"retrieval,omitempty"`
	CodexAware          CodexAwareContextConfig      `yaml:"codex-aware,omitempty" json:"codex-aware,omitempty"`
	Secondary           ContextRetrievalSecondPass   `yaml:"secondary,omitempty" json:"secondary,omitempty"`
}

type ContextRetrievalChunkConfig struct {
	MaxBytes int `yaml:"max-bytes,omitempty" json:"max-bytes,omitempty"`
}

type ContextRetrievalSearchConfig struct {
	TopK     int    `yaml:"top-k,omitempty" json:"top-k,omitempty"`
	Strategy string `yaml:"strategy,omitempty" json:"strategy,omitempty"`
}

// ContextRetrievalSecondPass controls a more aggressive fallback compression pass.
type ContextRetrievalSecondPass struct {
	Enabled             bool `yaml:"enabled" json:"enabled"`
	MaxInputBytes       int  `yaml:"max-input-bytes,omitempty" json:"max-input-bytes,omitempty"`
	PreserveRecentTurns int  `yaml:"preserve-recent-turns,omitempty" json:"preserve-recent-turns,omitempty"`
	TopK                int  `yaml:"top-k,omitempty" json:"top-k,omitempty"`
	MaxSummaryBytes     int  `yaml:"max-summary-bytes,omitempty" json:"max-summary-bytes,omitempty"`
	MaxItemBytes        int  `yaml:"max-item-bytes,omitempty" json:"max-item-bytes,omitempty"`
}

// CodexAwareContextConfig preserves Codex tool semantics while reducing context.
type CodexAwareContextConfig struct {
	Enabled                bool   `yaml:"enabled" json:"enabled"`
	PreserveToolPairs      bool   `yaml:"preserve-tool-pairs,omitempty" json:"preserve-tool-pairs,omitempty"`
	ToolPairRepair         string `yaml:"tool-pair-repair,omitempty" json:"tool-pair-repair,omitempty"`
	InsertSummary          bool   `yaml:"insert-summary,omitempty" json:"insert-summary,omitempty"`
	MaxSummaryBytes        int    `yaml:"max-summary-bytes,omitempty" json:"max-summary-bytes,omitempty"`
	PreserveRecentCommands int    `yaml:"preserve-recent-commands,omitempty" json:"preserve-recent-commands,omitempty"`
	PreserveRecentErrors   int    `yaml:"preserve-recent-errors,omitempty" json:"preserve-recent-errors,omitempty"`
}

// ObservabilityConfig groups optional diagnostic logging features.
type ObservabilityConfig struct {
	// ResponseTrace controls detailed /v1/responses lifecycle logs.
	ResponseTrace ResponseTraceConfig `yaml:"response-trace,omitempty" json:"response-trace,omitempty"`
}

// ResponseTraceConfig controls detailed /v1/responses lifecycle logs.
type ResponseTraceConfig struct {
	// Enabled emits lifecycle and latency logs for Responses API requests.
	Enabled bool `yaml:"enabled" json:"enabled"`

	// SlowThresholdMS emits slow-request summaries at or above this total duration.
	// Defaults to 20000ms when Enabled is true and this value is <= 0.
	SlowThresholdMS int `yaml:"slow-threshold-ms,omitempty" json:"slow-threshold-ms,omitempty"`

	// LogPayloadPreview includes a bounded request/response payload preview.
	// Keep disabled unless actively debugging; payloads may contain sensitive content.
	LogPayloadPreview bool `yaml:"log-payload-preview,omitempty" json:"log-payload-preview,omitempty"`

	// PayloadPreviewBytes caps logged payload preview bytes. Defaults to 512 and is capped internally.
	PayloadPreviewBytes int `yaml:"payload-preview-bytes,omitempty" json:"payload-preview-bytes,omitempty"`

	// LogHeaders includes selected safe client identity headers in lifecycle logs.
	LogHeaders bool `yaml:"log-headers,omitempty" json:"log-headers,omitempty"`
}

// RequestLogStorageConfig controls retention and cleanup of full request/response bodies.
type RequestLogStorageConfig struct {
	// StoreContent toggles persistence of full request and response bodies.
	// When false, new content is no longer written, but existing stored content is preserved.
	StoreContent bool `yaml:"store-content" json:"store-content"`

	// ContentRetentionDays defines how many days full request/response bodies are kept.
	// 0 or less means keep full content indefinitely. Metadata rows remain available
	// even after content is pruned.
	ContentRetentionDays int `yaml:"content-retention-days,omitempty" json:"content-retention-days,omitempty"`

	// CleanupIntervalMinutes controls how often the background cleanup job runs.
	CleanupIntervalMinutes int `yaml:"cleanup-interval-minutes,omitempty" json:"cleanup-interval-minutes,omitempty"`

	// MaxTotalSizeMB caps the total size of stored request/response bodies.
	// When the cap is exceeded, the oldest stored bodies are pruned before the
	// normal retention window elapses. 0 disables the size cap.
	MaxTotalSizeMB int `yaml:"max-total-size-mb,omitempty" json:"max-total-size-mb,omitempty"`

	// VacuumOnCleanup triggers a database VACUUM after content pruning so disk space is reclaimed.
	VacuumOnCleanup bool `yaml:"vacuum-on-cleanup" json:"vacuum-on-cleanup"`
}

// StreamingConfig holds server streaming behavior configuration.
type StreamingConfig struct {
	// KeepAliveSeconds controls how often the server emits SSE heartbeats (": keep-alive\n\n").
	// <= 0 disables keep-alives. Default is 0.
	KeepAliveSeconds int `yaml:"keepalive-seconds,omitempty" json:"keepalive-seconds,omitempty"`

	// BootstrapRetries controls how many times the server may retry a streaming request before any bytes are sent,
	// to allow auth rotation / transient recovery.
	// <= 0 disables bootstrap retries. Default is 0.
	BootstrapRetries int `yaml:"bootstrap-retries,omitempty" json:"bootstrap-retries,omitempty"`
}

// APIKeyEntry represents an API key with optional metadata for advanced management.
type APIKeyEntry struct {
	// Key is the API key string used for authentication.
	Key string `yaml:"key" json:"key"`

	// Name is a human-readable label for this key.
	Name string `yaml:"name,omitempty" json:"name,omitempty"`

	// Disabled marks this key as inactive. Disabled keys cannot authenticate.
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`

	// DailyLimit is the maximum number of requests per day. 0 means unlimited.
	DailyLimit int `yaml:"daily-limit,omitempty" json:"daily-limit,omitempty"`

	// TotalQuota is the total number of requests allowed. 0 means unlimited.
	TotalQuota int `yaml:"total-quota,omitempty" json:"total-quota,omitempty"`

	// SpendingLimit is the maximum allowed spending in US dollars. 0 means unlimited.
	// When model pricing is configured, requests will be rejected once the API key's
	// total accumulated cost exceeds this limit.
	SpendingLimit float64 `yaml:"spending-limit,omitempty" json:"spending-limit,omitempty"`

	// ConcurrencyLimit is the maximum number of concurrent requests. 0 means unlimited.
	ConcurrencyLimit int `yaml:"concurrency-limit,omitempty" json:"concurrency-limit,omitempty"`

	// RPMLimit is the maximum number of requests per minute. 0 means unlimited.
	RPMLimit int `yaml:"rpm-limit,omitempty" json:"rpm-limit,omitempty"`

	// TPMLimit is the maximum number of tokens per minute. 0 means unlimited.
	TPMLimit int `yaml:"tpm-limit,omitempty" json:"tpm-limit,omitempty"`

	// AllowedModels lists model patterns this key can access. Empty means all models.
	AllowedModels []string `yaml:"allowed-models,omitempty" json:"allowed-models,omitempty"`

	// AllowedChannels lists channel names this key can access. Empty means all channels.
	AllowedChannels []string `yaml:"allowed-channels,omitempty" json:"allowed-channels,omitempty"`

	// AllowedChannelGroups lists channel groups this key can access. Empty means all groups.
	AllowedChannelGroups []string `yaml:"allowed-channel-groups,omitempty" json:"allowed-channel-groups,omitempty"`

	// SystemPrompt is a system-level prompt that will be prepended to all requests
	// made with this API key. When set, a system message with this content is
	// automatically injected as the first message in the conversation.
	SystemPrompt string `yaml:"system-prompt,omitempty" json:"system-prompt,omitempty"`

	// CreatedAt is the ISO 8601 timestamp when this key was created.
	CreatedAt string `yaml:"created-at,omitempty" json:"created-at,omitempty"`
}

// AllAPIKeys returns a merged, deduplicated list of all API key strings
// from both the legacy APIKeys slice and the new APIKeyEntries slice.
func (c *SDKConfig) AllAPIKeys() []string {
	seen := make(map[string]struct{}, len(c.APIKeys)+len(c.APIKeyEntries))
	var keys []string
	for _, k := range c.APIKeys {
		trimmed := k
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		keys = append(keys, trimmed)
	}
	for _, entry := range c.APIKeyEntries {
		trimmed := entry.Key
		if trimmed == "" || entry.Disabled {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		keys = append(keys, trimmed)
	}
	return keys
}
