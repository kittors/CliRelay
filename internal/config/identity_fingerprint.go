package config

import "strings"

const (
	// Defaults are intentionally aligned with upstream CLIProxyAPI's codex-tui behavior.
	// Update these when upstream codex-tui identity changes.
	DefaultCodexFingerprintUserAgent     = "codex-tui/0.118.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex-tui; 0.118.0)"
	DefaultCodexFingerprintVersion       = ""
	DefaultCodexFingerprintOriginator    = "codex-tui"
	DefaultCodexFingerprintWebsocketBeta = "responses_websockets=2026-02-06"
	DefaultCodexFingerprintBetaFeatures  = ""
	DefaultCodexFingerprintSessionMode   = "per-request"

	DefaultClaudeFingerprintCLIVersion              = "2.1.161"
	DefaultClaudeFingerprintEntrypoint              = "cli"
	DefaultClaudeFingerprintAnthropicBeta           = "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,prompt-caching-scope-2026-01-05,effort-2025-11-24,context-management-2025-06-27,extended-cache-ttl-2025-04-11"
	DefaultClaudeFingerprintStainlessPackageVersion = "0.94.0"
	DefaultClaudeFingerprintStainlessRuntimeVersion = "v24.3.0"
	DefaultClaudeFingerprintStainlessTimeout        = "600"
	DefaultClaudeFingerprintSessionMode             = "per-request"

	DefaultGeminiFingerprintUserAgent      = "google-api-nodejs-client/9.15.1"
	DefaultGeminiFingerprintAPIClient      = "gl-node/22.17.0"
	DefaultGeminiFingerprintClientMetadata = "ideType=IDE_UNSPECIFIED,platform=PLATFORM_UNSPECIFIED,pluginType=GEMINI"

	DefaultXAIFingerprintUserAgent        = "grok-shell/0.2.93 (macos; aarch64)"
	DefaultXAIFingerprintClientIdentifier = "grok-shell"
	DefaultXAIFingerprintClientVersion    = "0.2.93"
)

// IdentityFingerprintConfig groups provider-specific upstream identity settings.
type IdentityFingerprintConfig struct {
	Codex  CodexIdentityFingerprintConfig  `yaml:"codex,omitempty" json:"codex,omitempty"`
	Claude ClaudeIdentityFingerprintConfig `yaml:"claude,omitempty" json:"claude,omitempty"`
	Gemini GeminiIdentityFingerprintConfig `yaml:"gemini,omitempty" json:"gemini,omitempty"`
	XAI    XAIIdentityFingerprintConfig    `yaml:"xai,omitempty" json:"xai,omitempty"`
}

// CodexIdentityFingerprintConfig configures Codex upstream identity headers.
type CodexIdentityFingerprintConfig struct {
	Enabled       bool              `yaml:"enabled" json:"enabled"`
	UserAgent     string            `yaml:"user-agent,omitempty" json:"user-agent,omitempty"`
	Version       string            `yaml:"version,omitempty" json:"version,omitempty"`
	Originator    string            `yaml:"originator,omitempty" json:"originator,omitempty"`
	WebsocketBeta string            `yaml:"websocket-beta,omitempty" json:"websocket-beta,omitempty"`
	BetaFeatures  string            `yaml:"x-codex-beta-features,omitempty" json:"x-codex-beta-features,omitempty"`
	SessionMode   string            `yaml:"session-mode,omitempty" json:"session-mode,omitempty"`
	SessionID     string            `yaml:"session-id,omitempty" json:"session-id,omitempty"`
	CustomHeaders map[string]string `yaml:"custom-headers,omitempty" json:"custom-headers,omitempty"`
}

// DefaultCodexIdentityFingerprint returns the recommended Codex identity template.
func DefaultCodexIdentityFingerprint() CodexIdentityFingerprintConfig {
	return CodexIdentityFingerprintConfig{
		Enabled:       false,
		UserAgent:     DefaultCodexFingerprintUserAgent,
		Version:       DefaultCodexFingerprintVersion,
		Originator:    DefaultCodexFingerprintOriginator,
		WebsocketBeta: DefaultCodexFingerprintWebsocketBeta,
		BetaFeatures:  DefaultCodexFingerprintBetaFeatures,
		SessionMode:   DefaultCodexFingerprintSessionMode,
		CustomHeaders: map[string]string{},
	}
}

// ClaudeIdentityFingerprintConfig configures Claude Code-style Anthropic OAuth identity.
type ClaudeIdentityFingerprintConfig struct {
	Enabled                 bool              `yaml:"enabled" json:"enabled"`
	CLIVersion              string            `yaml:"cli-version,omitempty" json:"cli-version,omitempty"`
	Entrypoint              string            `yaml:"entrypoint,omitempty" json:"entrypoint,omitempty"`
	UserAgent               string            `yaml:"user-agent,omitempty" json:"user-agent,omitempty"`
	AnthropicBeta           string            `yaml:"anthropic-beta,omitempty" json:"anthropic-beta,omitempty"`
	StainlessPackageVersion string            `yaml:"stainless-package-version,omitempty" json:"stainless-package-version,omitempty"`
	StainlessRuntimeVersion string            `yaml:"stainless-runtime-version,omitempty" json:"stainless-runtime-version,omitempty"`
	StainlessTimeout        string            `yaml:"stainless-timeout,omitempty" json:"stainless-timeout,omitempty"`
	SessionMode             string            `yaml:"session-mode,omitempty" json:"session-mode,omitempty"`
	SessionID               string            `yaml:"session-id,omitempty" json:"session-id,omitempty"`
	DeviceID                string            `yaml:"device-id,omitempty" json:"device-id,omitempty"`
	CustomHeaders           map[string]string `yaml:"custom-headers,omitempty" json:"custom-headers,omitempty"`
}

// GeminiIdentityFingerprintConfig configures Gemini CLI upstream identity headers.
type GeminiIdentityFingerprintConfig struct {
	Enabled        bool              `yaml:"enabled" json:"enabled"`
	UserAgent      string            `yaml:"user-agent,omitempty" json:"user-agent,omitempty"`
	APIClient      string            `yaml:"x-goog-api-client,omitempty" json:"x-goog-api-client,omitempty"`
	ClientMetadata string            `yaml:"client-metadata,omitempty" json:"client-metadata,omitempty"`
	CustomHeaders  map[string]string `yaml:"custom-headers,omitempty" json:"custom-headers,omitempty"`
}

// XAIIdentityFingerprintConfig configures Grok/xAI upstream identity headers.
type XAIIdentityFingerprintConfig struct {
	Enabled            bool              `yaml:"enabled" json:"enabled"`
	UserAgent          string            `yaml:"user-agent,omitempty" json:"user-agent,omitempty"`
	ClientIdentifier   string            `yaml:"x-grok-client-identifier,omitempty" json:"x-grok-client-identifier,omitempty"`
	ClientVersion      string            `yaml:"x-grok-client-version,omitempty" json:"x-grok-client-version,omitempty"`
	GrokConversationID string            `yaml:"x-grok-conv-id,omitempty" json:"x-grok-conv-id,omitempty"`
	CustomHeaders      map[string]string `yaml:"custom-headers,omitempty" json:"custom-headers,omitempty"`
}

// DefaultClaudeIdentityFingerprint returns the recommended Claude Code identity template.
func DefaultClaudeIdentityFingerprint() ClaudeIdentityFingerprintConfig {
	cliVersion := DefaultClaudeFingerprintCLIVersion
	entrypoint := DefaultClaudeFingerprintEntrypoint
	return ClaudeIdentityFingerprintConfig{
		Enabled:                 false,
		CLIVersion:              cliVersion,
		Entrypoint:              entrypoint,
		UserAgent:               BuildClaudeFingerprintUserAgent(cliVersion, entrypoint),
		AnthropicBeta:           DefaultClaudeFingerprintAnthropicBeta,
		StainlessPackageVersion: DefaultClaudeFingerprintStainlessPackageVersion,
		StainlessRuntimeVersion: DefaultClaudeFingerprintStainlessRuntimeVersion,
		StainlessTimeout:        DefaultClaudeFingerprintStainlessTimeout,
		SessionMode:             DefaultClaudeFingerprintSessionMode,
		CustomHeaders:           map[string]string{},
	}
}

// DefaultGeminiIdentityFingerprint returns the recommended Gemini CLI identity template.
func DefaultGeminiIdentityFingerprint() GeminiIdentityFingerprintConfig {
	return GeminiIdentityFingerprintConfig{
		Enabled:        false,
		UserAgent:      DefaultGeminiFingerprintUserAgent,
		APIClient:      DefaultGeminiFingerprintAPIClient,
		ClientMetadata: DefaultGeminiFingerprintClientMetadata,
		CustomHeaders:  map[string]string{},
	}
}

// DefaultXAIIdentityFingerprint returns the conservative Grok identity template.
func DefaultXAIIdentityFingerprint() XAIIdentityFingerprintConfig {
	return XAIIdentityFingerprintConfig{
		Enabled:          false,
		UserAgent:        DefaultXAIFingerprintUserAgent,
		ClientIdentifier: DefaultXAIFingerprintClientIdentifier,
		ClientVersion:    DefaultXAIFingerprintClientVersion,
		CustomHeaders:    map[string]string{},
	}
}

// SanitizeIdentityFingerprint normalizes provider identity fingerprint config.
func (cfg *Config) SanitizeIdentityFingerprint() {
	if cfg == nil {
		return
	}
	cfg.IdentityFingerprint.Codex = CleanCodexIdentityFingerprint(cfg.IdentityFingerprint.Codex)
	cfg.IdentityFingerprint.Claude = CleanClaudeIdentityFingerprint(cfg.IdentityFingerprint.Claude)
	cfg.IdentityFingerprint.Gemini = CleanGeminiIdentityFingerprint(cfg.IdentityFingerprint.Gemini)
	cfg.IdentityFingerprint.XAI = CleanXAIIdentityFingerprint(cfg.IdentityFingerprint.XAI)
}

// NormalizeCodexIdentityFingerprint trims user input and applies safe defaults
// for fields that participate in Codex upstream identity.
func NormalizeCodexIdentityFingerprint(in CodexIdentityFingerprintConfig) CodexIdentityFingerprintConfig {
	out := CleanCodexIdentityFingerprint(in)

	if out.UserAgent == "" {
		out.UserAgent = DefaultCodexFingerprintUserAgent
	}
	if out.Version == "" {
		out.Version = DefaultCodexFingerprintVersion
	}
	if out.Originator == "" {
		out.Originator = DefaultCodexFingerprintOriginator
	}
	if out.WebsocketBeta == "" {
		out.WebsocketBeta = DefaultCodexFingerprintWebsocketBeta
	}
	if out.BetaFeatures == "" {
		out.BetaFeatures = DefaultCodexFingerprintBetaFeatures
	}
	if out.SessionMode == "" {
		out.SessionMode = DefaultCodexFingerprintSessionMode
	}
	if out.SessionMode != "server-stable" && out.SessionMode != "fixed" && out.SessionMode != "per-request" {
		out.SessionMode = DefaultCodexFingerprintSessionMode
	}

	return out
}

// CleanCodexIdentityFingerprint trims explicit overrides while preserving empty fields
// as "automatic learning" markers.
func CleanCodexIdentityFingerprint(in CodexIdentityFingerprintConfig) CodexIdentityFingerprintConfig {
	out := in
	out.UserAgent = strings.TrimSpace(out.UserAgent)
	out.Version = strings.TrimSpace(out.Version)
	out.Originator = strings.TrimSpace(out.Originator)
	out.WebsocketBeta = strings.TrimSpace(out.WebsocketBeta)
	out.BetaFeatures = strings.TrimSpace(out.BetaFeatures)
	out.SessionMode = strings.TrimSpace(strings.ToLower(out.SessionMode))
	out.SessionID = strings.TrimSpace(out.SessionID)
	if out.SessionMode != "" && out.SessionMode != "server-stable" && out.SessionMode != "fixed" && out.SessionMode != "per-request" {
		out.SessionMode = DefaultCodexFingerprintSessionMode
	}
	out.CustomHeaders = cleanIdentityFingerprintHeaders(out.CustomHeaders)
	return out
}

// BuildClaudeFingerprintUserAgent builds the Claude Code User-Agent value from
// the CLI version and entrypoint dimensions.
func BuildClaudeFingerprintUserAgent(cliVersion, entrypoint string) string {
	cliVersion = strings.TrimSpace(cliVersion)
	entrypoint = strings.TrimSpace(entrypoint)
	if cliVersion == "" {
		cliVersion = DefaultClaudeFingerprintCLIVersion
	}
	if entrypoint == "" {
		entrypoint = DefaultClaudeFingerprintEntrypoint
	}
	return "claude-cli/" + cliVersion + " (external, " + entrypoint + ")"
}

// NormalizeClaudeIdentityFingerprint trims user input and applies safe defaults
// for fields that participate in Claude Code-style Anthropic OAuth identity.
func NormalizeClaudeIdentityFingerprint(in ClaudeIdentityFingerprintConfig) ClaudeIdentityFingerprintConfig {
	out := CleanClaudeIdentityFingerprint(in)

	if out.CLIVersion == "" {
		out.CLIVersion = DefaultClaudeFingerprintCLIVersion
	}
	if out.Entrypoint == "" {
		out.Entrypoint = DefaultClaudeFingerprintEntrypoint
	}
	if out.UserAgent == "" {
		out.UserAgent = BuildClaudeFingerprintUserAgent(out.CLIVersion, out.Entrypoint)
	}
	if out.AnthropicBeta == "" {
		out.AnthropicBeta = DefaultClaudeFingerprintAnthropicBeta
	}
	if out.StainlessPackageVersion == "" {
		out.StainlessPackageVersion = DefaultClaudeFingerprintStainlessPackageVersion
	}
	if out.StainlessRuntimeVersion == "" {
		out.StainlessRuntimeVersion = DefaultClaudeFingerprintStainlessRuntimeVersion
	}
	if out.StainlessTimeout == "" {
		out.StainlessTimeout = DefaultClaudeFingerprintStainlessTimeout
	}
	if out.SessionMode == "" {
		out.SessionMode = DefaultClaudeFingerprintSessionMode
	}
	if out.SessionMode != "server-stable" && out.SessionMode != "fixed" && out.SessionMode != "per-request" {
		out.SessionMode = DefaultClaudeFingerprintSessionMode
	}

	return out
}

// CleanClaudeIdentityFingerprint trims explicit overrides while preserving empty fields
// as "automatic learning" markers.
func CleanClaudeIdentityFingerprint(in ClaudeIdentityFingerprintConfig) ClaudeIdentityFingerprintConfig {
	out := in
	out.CLIVersion = strings.TrimSpace(out.CLIVersion)
	out.Entrypoint = strings.TrimSpace(out.Entrypoint)
	out.UserAgent = strings.TrimSpace(out.UserAgent)
	out.AnthropicBeta = strings.TrimSpace(out.AnthropicBeta)
	out.StainlessPackageVersion = strings.TrimSpace(out.StainlessPackageVersion)
	out.StainlessRuntimeVersion = strings.TrimSpace(out.StainlessRuntimeVersion)
	out.StainlessTimeout = strings.TrimSpace(out.StainlessTimeout)
	out.SessionMode = strings.TrimSpace(strings.ToLower(out.SessionMode))
	out.SessionID = strings.TrimSpace(out.SessionID)
	out.DeviceID = strings.TrimSpace(out.DeviceID)
	if out.SessionMode != "" && out.SessionMode != "server-stable" && out.SessionMode != "fixed" && out.SessionMode != "per-request" {
		out.SessionMode = DefaultClaudeFingerprintSessionMode
	}
	out.CustomHeaders = cleanIdentityFingerprintHeaders(out.CustomHeaders)
	return out
}

// NormalizeGeminiIdentityFingerprint trims user input and applies safe defaults
// for fields that participate in Gemini CLI upstream identity.
func NormalizeGeminiIdentityFingerprint(in GeminiIdentityFingerprintConfig) GeminiIdentityFingerprintConfig {
	out := CleanGeminiIdentityFingerprint(in)
	if out.UserAgent == "" {
		out.UserAgent = DefaultGeminiFingerprintUserAgent
	}
	if out.APIClient == "" {
		out.APIClient = DefaultGeminiFingerprintAPIClient
	}
	if out.ClientMetadata == "" {
		out.ClientMetadata = DefaultGeminiFingerprintClientMetadata
	}
	return out
}

// CleanGeminiIdentityFingerprint trims explicit overrides while preserving empty fields
// as "automatic learning" markers.
func CleanGeminiIdentityFingerprint(in GeminiIdentityFingerprintConfig) GeminiIdentityFingerprintConfig {
	out := in
	out.UserAgent = strings.TrimSpace(out.UserAgent)
	out.APIClient = strings.TrimSpace(out.APIClient)
	out.ClientMetadata = strings.TrimSpace(out.ClientMetadata)
	out.CustomHeaders = cleanIdentityFingerprintHeaders(out.CustomHeaders)
	return out
}

// NormalizeXAIIdentityFingerprint trims user input while preserving empty fields
// as "automatic learning" markers.
func NormalizeXAIIdentityFingerprint(in XAIIdentityFingerprintConfig) XAIIdentityFingerprintConfig {
	return CleanXAIIdentityFingerprint(in)
}

// CleanXAIIdentityFingerprint trims explicit overrides while preserving empty fields.
func CleanXAIIdentityFingerprint(in XAIIdentityFingerprintConfig) XAIIdentityFingerprintConfig {
	out := in
	out.UserAgent = strings.TrimSpace(out.UserAgent)
	out.ClientIdentifier = strings.TrimSpace(out.ClientIdentifier)
	out.ClientVersion = strings.TrimSpace(out.ClientVersion)
	out.GrokConversationID = strings.TrimSpace(out.GrokConversationID)
	out.CustomHeaders = cleanIdentityFingerprintHeaders(out.CustomHeaders)
	return out
}

func cleanIdentityFingerprintHeaders(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || value == "" {
			continue
		}
		out[key] = value
	}
	return out
}
