package config

// PostgresConfig holds the runtime primary database connection settings.
type PostgresConfig struct {
	DSN          string `yaml:"dsn" json:"-"`
	MaxOpenConns int    `yaml:"max-open-conns,omitempty" json:"max-open-conns,omitempty"`
	MaxIdleConns int    `yaml:"max-idle-conns,omitempty" json:"max-idle-conns,omitempty"`
}

// RedisConfig holds the configuration for connecting to a Redis instance for data persistence.
type RedisConfig struct {
	Enable   bool   `yaml:"enable" json:"enable"`
	Addr     string `yaml:"addr" json:"addr"`
	Password string `yaml:"password" json:"password"`
	DB       int    `yaml:"db" json:"db"`
}

// TLSConfig holds HTTPS server settings.
type TLSConfig struct {
	// Enable toggles HTTPS server mode.
	Enable bool `yaml:"enable" json:"enable"`
	// Cert is the path to the TLS certificate file.
	Cert string `yaml:"cert" json:"cert"`
	// Key is the path to the TLS private key file.
	Key string `yaml:"key" json:"key"`
}

// PprofConfig holds pprof HTTP server settings.
type PprofConfig struct {
	// Enable toggles the pprof HTTP debug server.
	Enable bool `yaml:"enable" json:"enable"`
	// Addr is the host:port address for the pprof HTTP server.
	Addr string `yaml:"addr" json:"addr"`
	// AllowRemote permits binding pprof to non-loopback addresses.
	AllowRemote bool `yaml:"allow-remote" json:"allow-remote"`
}

// RemoteManagement holds management API configuration under 'remote-management'.
type RemoteManagement struct {
	// AllowRemote toggles remote (non-localhost) access to management API.
	AllowRemote bool `yaml:"allow-remote"`
	// SecretKey is the management key (plaintext or bcrypt hashed). YAML key intentionally 'secret-key'.
	SecretKey string `yaml:"secret-key"`
	// DisableControlPanel skips serving and syncing the bundled management UI when true.
	DisableControlPanel bool `yaml:"disable-control-panel"`
	// PanelGitHubRepository overrides the GitHub repository used to fetch the management panel asset.
	// Accepts either a repository URL (https://github.com/org/repo) or an API releases endpoint.
	PanelGitHubRepository string `yaml:"panel-github-repository"`
	// Auth tunes login throttling and session token lifetimes.
	// Zero values fall back to the built-in defaults documented per field.
	Auth AuthHardening `yaml:"auth,omitempty"`
}

// AuthHardening groups login-throttle and session-rotation knobs. Every field is
// zero-means-default so an absent block behaves exactly like the shipped defaults.
type AuthHardening struct {
	// LoginFailureWindowSeconds is the sliding window for counting login
	// failures. Default 900. Counters outside the window are discarded.
	LoginFailureWindowSeconds int `yaml:"login-failure-window-seconds,omitempty"`
	// AccountFailureLimit arms a staged cooldown for one account. Default 5.
	AccountFailureLimit int `yaml:"account-failure-limit,omitempty"`
	// ManagementKeyFailureLimit arms a per-IP ban for wrong management keys. Default 10.
	ManagementKeyFailureLimit int `yaml:"management-key-failure-limit,omitempty"`
	// UnauthenticatedRequestLimit caps credential-less management requests per
	// client per hour. Exceeding it returns 429; it never arms a credential ban.
	// Default 300.
	UnauthenticatedRequestLimit int `yaml:"unauthenticated-request-limit,omitempty"`
	// FailureResetHours is the quiet period that clears counters and backoff
	// escalation. Default 12.
	FailureResetHours int `yaml:"failure-reset-hours,omitempty"`
	// RefreshGraceSeconds keeps a just-rotated refresh token usable so retries
	// and racing tabs do not lose the session. Default 30, clamped to 60.
	// The admin panel's total refresh budget (lock wait + retries) is 23s and
	// MUST stay below this value, or a legitimate retry lands outside the
	// window and is treated as a replay, revoking the whole session.
	RefreshGraceSeconds int `yaml:"refresh-grace-seconds,omitempty"`
	// RefreshGraceMaxReuse caps replays of one rotated refresh token inside the
	// grace window. Default 2.
	RefreshGraceMaxReuse int `yaml:"refresh-grace-max-reuse,omitempty"`
	// AccessTokenGraceSeconds keeps the previous access token valid after
	// rotation so in-flight requests are not rejected. Default 60; must exceed
	// the panel's 30s request timeout.
	AccessTokenGraceSeconds int `yaml:"access-token-grace-seconds,omitempty"`
	// AbsoluteRefreshTTLHours is the hard session ceiling; rotation never
	// extends past login time plus this value. Default 1440 (60 days).
	AbsoluteRefreshTTLHours int `yaml:"absolute-refresh-ttl-hours,omitempty"`
	// SessionReaperIntervalMinutes controls expired token/session GC. Default 60.
	SessionReaperIntervalMinutes int `yaml:"session-reaper-interval-minutes,omitempty"`
	// AuditRetentionDays drops audit_logs rows older than this. Default 180.
	// Negative disables the age limit and keeps rows until the row cap applies.
	AuditRetentionDays int `yaml:"audit-retention-days,omitempty"`
	// AuditMaxRows caps audit_logs; the oldest rows are pruned first. Default
	// 200000. Negative disables the cap. It is a runaway guard, not a trim: with
	// the audit write policy in place, normal use stays orders of magnitude below.
	AuditMaxRows int `yaml:"audit-max-rows,omitempty"`
}

// AutoUpdateConfig holds Docker-first update check and sidecar settings.
type AutoUpdateConfig struct {
	// Enabled controls whether the management UI should automatically prompt for updates.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Channel can be auto, main, or dev. Auto infers from the running build metadata.
	Channel string `yaml:"channel,omitempty" json:"channel,omitempty"`
	// Repository is the GitHub repository used for branch commits and release notes.
	Repository string `yaml:"repository,omitempty" json:"repository,omitempty"`
	// DockerImage is the image repository pulled by the updater sidecar.
	DockerImage string `yaml:"docker-image,omitempty" json:"docker-image,omitempty"`
	// UpdaterURL is the internal URL of the independent updater sidecar.
	UpdaterURL string `yaml:"updater-url,omitempty" json:"updater-url,omitempty"`
}

// QuotaExceeded defines the behavior when API quota limits are exceeded.
// It provides configuration options for automatic failover mechanisms.
type QuotaExceeded struct {
	// SwitchProject indicates whether to automatically switch to another project when a quota is exceeded.
	SwitchProject bool `yaml:"switch-project" json:"switch-project"`

	// SwitchPreviewModel indicates whether to automatically switch to a preview model when a quota is exceeded.
	SwitchPreviewModel bool `yaml:"switch-preview-model" json:"switch-preview-model"`
}

// RoutingConfig configures how credentials are selected for requests.
type RoutingConfig struct {
	// Strategy selects the credential selection strategy.
	// Supported values: "round-robin" (default), "fill-first".
	Strategy string `yaml:"strategy,omitempty" json:"strategy,omitempty"`

	// IncludeDefaultGroup keeps unprefixed channels addressable via the implicit "default" group.
	IncludeDefaultGroup bool `yaml:"include-default-group,omitempty" json:"include-default-group,omitempty"`

	// ChannelGroups defines named channel groups used by path routing and API key permissions.
	ChannelGroups []RoutingChannelGroup `yaml:"channel-groups,omitempty" json:"channel-groups,omitempty"`

	// PathRoutes maps URL path namespaces to channel groups.
	PathRoutes []RoutingPathRoute `yaml:"path-routes,omitempty" json:"path-routes,omitempty"`
}

// OAuthModelAlias defines a model ID alias for a specific channel.
// It maps the upstream model name (Name) to the client-visible alias (Alias).
// When Fork is true, the alias is added as an additional model in listings while
// keeping the original model ID available.
type OAuthModelAlias struct {
	Name  string `yaml:"name" json:"name"`
	Alias string `yaml:"alias" json:"alias"`
	Fork  bool   `yaml:"fork,omitempty" json:"fork,omitempty"`
}

// AmpModelMapping defines a model name mapping for Amp CLI requests.
// When Amp requests a model that isn't available locally, this mapping
// allows routing to an alternative model that IS available.
type AmpModelMapping struct {
	// From is the model name that Amp CLI requests (e.g., "claude-opus-4.5").
	From string `yaml:"from" json:"from"`

	// To is the target model name to route to (e.g., "claude-sonnet-4").
	// The target model must have available providers in the registry.
	To string `yaml:"to" json:"to"`

	// Regex indicates whether the 'from' field should be interpreted as a regular
	// expression for matching model names. When true, this mapping is evaluated
	// after exact matches and in the order provided. Defaults to false (exact match).
	Regex bool `yaml:"regex,omitempty" json:"regex,omitempty"`
}

// AmpCode groups Amp CLI integration settings including upstream routing,
// optional overrides, management route restrictions, and model fallback mappings.
type AmpCode struct {
	// UpstreamURL defines the upstream Amp control plane used for non-provider calls.
	UpstreamURL string `yaml:"upstream-url" json:"upstream-url"`

	// UpstreamAPIKey optionally overrides the Authorization header when proxying Amp upstream calls.
	UpstreamAPIKey string `yaml:"upstream-api-key" json:"upstream-api-key"`

	// UpstreamAPIKeys maps client API keys (from top-level api-keys) to upstream API keys.
	// When a client authenticates with a key that matches an entry, that upstream key is used.
	// If no match is found, falls back to UpstreamAPIKey (default behavior).
	UpstreamAPIKeys []AmpUpstreamAPIKeyEntry `yaml:"upstream-api-keys,omitempty" json:"upstream-api-keys,omitempty"`

	// RestrictManagementToLocalhost restricts Amp management routes (/api/user, /api/threads, etc.)
	// to only accept connections from localhost (127.0.0.1, ::1). When true, prevents drive-by
	// browser attacks and remote access to management endpoints. Default: false (API key auth is sufficient).
	RestrictManagementToLocalhost bool `yaml:"restrict-management-to-localhost" json:"restrict-management-to-localhost"`

	// ModelMappings defines model name mappings for Amp CLI requests.
	// When Amp requests a model that isn't available locally, these mappings
	// allow routing to an alternative model that IS available.
	ModelMappings []AmpModelMapping `yaml:"model-mappings" json:"model-mappings"`

	// ForceModelMappings when true, model mappings take precedence over local API keys.
	// When false (default), local API keys are used first if available.
	ForceModelMappings bool `yaml:"force-model-mappings" json:"force-model-mappings"`
}

// AmpUpstreamAPIKeyEntry maps a set of client API keys to a specific upstream API key.
// When a request is authenticated with one of the APIKeys, the corresponding UpstreamAPIKey
// is used for the upstream Amp request.
type AmpUpstreamAPIKeyEntry struct {
	// UpstreamAPIKey is the API key to use when proxying to the Amp upstream.
	UpstreamAPIKey string `yaml:"upstream-api-key" json:"upstream-api-key"`

	// APIKeys are the client API keys (from top-level api-keys) that map to this upstream key.
	APIKeys []string `yaml:"api-keys" json:"api-keys"`
}

// PayloadConfig defines default and override parameter rules applied to provider payloads.
type PayloadConfig struct {
	// Default defines rules that only set parameters when they are missing in the payload.
	Default []PayloadRule `yaml:"default" json:"default"`
	// DefaultRaw defines rules that set raw JSON values only when they are missing.
	DefaultRaw []PayloadRule `yaml:"default-raw" json:"default-raw"`
	// Override defines rules that always set parameters, overwriting any existing values.
	Override []PayloadRule `yaml:"override" json:"override"`
	// OverrideRaw defines rules that always set raw JSON values, overwriting any existing values.
	OverrideRaw []PayloadRule `yaml:"override-raw" json:"override-raw"`
	// Filter defines rules that remove parameters from the payload by JSON path.
	Filter []PayloadFilterRule `yaml:"filter" json:"filter"`
}

// PayloadFilterRule describes a rule to remove specific JSON paths from matching model payloads.
type PayloadFilterRule struct {
	// Models lists model entries with name pattern and protocol constraint.
	Models []PayloadModelRule `yaml:"models" json:"models"`
	// Params lists JSON paths (gjson/sjson syntax) to remove from the payload.
	Params []string `yaml:"params" json:"params"`
}

// PayloadRule describes a single rule targeting a list of models with parameter updates.
type PayloadRule struct {
	// Models lists model entries with name pattern and protocol constraint.
	Models []PayloadModelRule `yaml:"models" json:"models"`
	// Params maps JSON paths (gjson/sjson syntax) to values written into the payload.
	// For *-raw rules, values are treated as raw JSON fragments (strings are used as-is).
	Params map[string]any `yaml:"params" json:"params"`
}

// PayloadModelRule ties a model name pattern to a specific translator protocol.
type PayloadModelRule struct {
	// Name is the model name or wildcard pattern (e.g., "gpt-*", "*-5", "gemini-*-pro").
	Name string `yaml:"name" json:"name"`
	// Protocol restricts the rule to a specific translator format (e.g., "gemini", "responses").
	Protocol string `yaml:"protocol" json:"protocol"`
}

// CloakConfig configures request cloaking for non-Claude-Code clients.
// Cloaking disguises API requests to appear as originating from the official Claude Code CLI.
type CloakConfig struct {
	// Mode controls cloaking behavior: "auto" (default), "always", or "never".
	// - "auto": cloak only when client is not Claude Code (based on User-Agent)
	// - "always": always apply cloaking regardless of client
	// - "never": never apply cloaking
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`

	// StrictMode controls how system prompts are handled when cloaking.
	// - false (default): prepend Claude Code prompt to user system messages
	// - true: strip all user system messages, keep only Claude Code prompt
	StrictMode bool `yaml:"strict-mode,omitempty" json:"strict-mode,omitempty"`

	// SensitiveWords is a list of words to obfuscate with zero-width characters.
	// This can help bypass certain content filters.
	SensitiveWords []string `yaml:"sensitive-words,omitempty" json:"sensitive-words,omitempty"`

	// CacheUserID controls whether Claude user_id values are cached per API key.
	// When false, a fresh random user_id is generated for every request.
	CacheUserID *bool `yaml:"cache-user-id,omitempty" json:"cache-user-id,omitempty"`
}
