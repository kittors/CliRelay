package config

import "strings"

// SanitizeIdentityFingerprint normalizes provider identity fingerprint config.
func (cfg *Config) SanitizeIdentityFingerprint() {
	if cfg == nil {
		return
	}
	cfg.IdentityFingerprint.Codex = NormalizeCodexIdentityFingerprint(cfg.IdentityFingerprint.Codex)
	cfg.IdentityFingerprint.Claude = NormalizeClaudeIdentityFingerprint(cfg.IdentityFingerprint.Claude)
}

// NormalizeCodexIdentityFingerprint trims user input and applies safe defaults
// for fields that participate in Codex upstream identity.
func NormalizeCodexIdentityFingerprint(in CodexIdentityFingerprintConfig) CodexIdentityFingerprintConfig {
	out := in
	out.UserAgent = strings.TrimSpace(out.UserAgent)
	out.Version = strings.TrimSpace(out.Version)
	out.Originator = strings.TrimSpace(out.Originator)
	out.WebsocketBeta = strings.TrimSpace(out.WebsocketBeta)
	out.SessionMode = strings.TrimSpace(strings.ToLower(out.SessionMode))
	out.SessionID = strings.TrimSpace(out.SessionID)

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
	if out.SessionMode == "" {
		out.SessionMode = DefaultCodexFingerprintSessionMode
	}
	if out.SessionMode != "server-stable" && out.SessionMode != "fixed" && out.SessionMode != "per-request" {
		out.SessionMode = DefaultCodexFingerprintSessionMode
	}

	if len(out.CustomHeaders) > 0 {
		cleaned := make(map[string]string, len(out.CustomHeaders))
		for key, value := range out.CustomHeaders {
			key = strings.TrimSpace(key)
			value = strings.TrimSpace(value)
			if key == "" || value == "" {
				continue
			}
			cleaned[key] = value
		}
		out.CustomHeaders = cleaned
	} else {
		out.CustomHeaders = map[string]string{}
	}
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

	if len(out.CustomHeaders) > 0 {
		cleaned := make(map[string]string, len(out.CustomHeaders))
		for key, value := range out.CustomHeaders {
			key = strings.TrimSpace(key)
			value = strings.TrimSpace(value)
			if key == "" || value == "" {
				continue
			}
			cleaned[key] = value
		}
		out.CustomHeaders = cleaned
	} else {
		out.CustomHeaders = map[string]string{}
	}
	return out
}
