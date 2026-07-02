package config

import "github.com/router-for-me/CLIProxyAPI/v6/internal/codexadmission"

// CodexOAuthAdmissionConfig stores global fixed allowed-client presets for Codex OAuth admission.
type CodexOAuthAdmissionConfig struct {
	AllowedClientPresets []string `yaml:"allowed-clients,omitempty" json:"allowed_clients,omitempty"`
}

// CleanCodexOAuthAdmission normalizes persisted Codex OAuth admission settings.
func CleanCodexOAuthAdmission(in CodexOAuthAdmissionConfig) CodexOAuthAdmissionConfig {
	return CodexOAuthAdmissionConfig{
		AllowedClientPresets: codexadmission.FilterKnownAllowedClientPresets(in.AllowedClientPresets),
	}
}

// SanitizeCodexOAuthAdmission normalizes Codex OAuth admission settings in-place.
func (cfg *Config) SanitizeCodexOAuthAdmission() {
	if cfg == nil {
		return
	}
	cfg.CodexOAuthAdmission = CleanCodexOAuthAdmission(cfg.CodexOAuthAdmission)
}
