package config

import (
	"encoding/json"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	DefaultAntigravityFingerprintVersion   = "4.3.0"
	DefaultAntigravityFingerprintUserAgent = "vscode/1.X.X (Antigravity/" + DefaultAntigravityFingerprintVersion + ")"
)

// AntigravityIdentityFingerprintConfig configures Antigravity upstream identity headers.
type AntigravityIdentityFingerprintConfig struct {
	Enabled       bool              `yaml:"enabled" json:"enabled"`
	UserAgent     string            `yaml:"user-agent,omitempty" json:"user-agent,omitempty"`
	Version       string            `yaml:"version,omitempty" json:"version,omitempty"`
	CustomHeaders map[string]string `yaml:"custom-headers,omitempty" json:"custom-headers,omitempty"`
	enabledSet    bool
}

// DefaultAntigravityIdentityFingerprint returns the recommended Antigravity identity template.
func DefaultAntigravityIdentityFingerprint() AntigravityIdentityFingerprintConfig {
	return AntigravityIdentityFingerprintConfig{
		Enabled:       true,
		UserAgent:     DefaultAntigravityFingerprintUserAgent,
		Version:       DefaultAntigravityFingerprintVersion,
		CustomHeaders: map[string]string{},
	}
}

// NormalizeAntigravityIdentityFingerprint ensures an unconfigured block is populated with defaults.
func NormalizeAntigravityIdentityFingerprint(in AntigravityIdentityFingerprintConfig) AntigravityIdentityFingerprintConfig {
	out := in
	if !out.enabledSet {
		out.Enabled = true
	}
	out.UserAgent = strings.TrimSpace(out.UserAgent)
	out.Version = strings.TrimSpace(out.Version)
	if out.UserAgent == "" {
		out.UserAgent = DefaultAntigravityFingerprintUserAgent
	}
	if out.Version == "" {
		out.Version = DefaultAntigravityFingerprintVersion
	}
	if out.CustomHeaders == nil {
		out.CustomHeaders = map[string]string{}
	}
	return out
}

// CleanAntigravityIdentityFingerprint strips empty fields and whitespace.
func CleanAntigravityIdentityFingerprint(in AntigravityIdentityFingerprintConfig) AntigravityIdentityFingerprintConfig {
	out := in
	out.UserAgent = strings.TrimSpace(out.UserAgent)
	out.Version = strings.TrimSpace(out.Version)
	if len(out.CustomHeaders) > 0 {
		cleaned := make(map[string]string, len(out.CustomHeaders))
		for k, v := range out.CustomHeaders {
			kTrim := strings.TrimSpace(k)
			vTrim := strings.TrimSpace(v)
			if kTrim != "" && vTrim != "" {
				cleaned[kTrim] = vTrim
			}
		}
		out.CustomHeaders = cleaned
	}
	return out
}

func (fp *AntigravityIdentityFingerprintConfig) UnmarshalJSON(data []byte) error {
	type raw AntigravityIdentityFingerprintConfig
	var dest raw
	if err := json.Unmarshal(data, &dest); err != nil {
		return err
	}
	*fp = AntigravityIdentityFingerprintConfig(dest)
	var flagCheck struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.Unmarshal(data, &flagCheck); err == nil && flagCheck.Enabled != nil {
		fp.enabledSet = true
	}
	return nil
}

func (fp *AntigravityIdentityFingerprintConfig) UnmarshalYAML(value *yaml.Node) error {
	type raw AntigravityIdentityFingerprintConfig
	var dest raw
	if err := value.Decode(&dest); err != nil {
		return err
	}
	*fp = AntigravityIdentityFingerprintConfig(dest)
	for i := 0; i < len(value.Content)-1; i += 2 {
		if value.Content[i].Value == "enabled" {
			fp.enabledSet = true
			break
		}
	}
	return nil
}
