package config

import (
	"encoding/json"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// Defaults follow the identity the official kimi-cli sends. They match the
	// values the Kimi executor has always hardcoded, so an account with no learned
	// fingerprint and no operator preset keeps its previous outbound identity.
	DefaultKimiFingerprintUserAgent = "KimiCLI/1.10.6"
	DefaultKimiFingerprintPlatform  = "kimi_cli"
	DefaultKimiFingerprintVersion   = "1.10.6"
)

// KimiIdentityFingerprintConfig configures the kimi-cli identity headers sent to
// the Kimi coding gateway.
//
// Device name and model have no compiled-in default on purpose. They describe the
// machine the client runs on, and the only value the proxy could invent is its own
// hostname — which is both wrong and identical across every account on the host.
// Leaving them empty lets the runtime keep its existing host-derived fallback while
// a learned observation, when present, replaces it with the real client's value.
type KimiIdentityFingerprintConfig struct {
	Enabled       bool              `yaml:"enabled" json:"enabled"`
	UserAgent     string            `yaml:"user-agent,omitempty" json:"user-agent,omitempty"`
	Platform      string            `yaml:"x-msh-platform,omitempty" json:"x-msh-platform,omitempty"`
	Version       string            `yaml:"x-msh-version,omitempty" json:"x-msh-version,omitempty"`
	DeviceName    string            `yaml:"x-msh-device-name,omitempty" json:"x-msh-device-name,omitempty"`
	DeviceModel   string            `yaml:"x-msh-device-model,omitempty" json:"x-msh-device-model,omitempty"`
	CustomHeaders map[string]string `yaml:"custom-headers,omitempty" json:"custom-headers,omitempty"`
	enabledSet    bool
}

// DefaultKimiIdentityFingerprint returns the recommended kimi-cli identity template.
func DefaultKimiIdentityFingerprint() KimiIdentityFingerprintConfig {
	return KimiIdentityFingerprintConfig{
		Enabled:       true,
		UserAgent:     DefaultKimiFingerprintUserAgent,
		Platform:      DefaultKimiFingerprintPlatform,
		Version:       DefaultKimiFingerprintVersion,
		CustomHeaders: map[string]string{},
	}
}

// NormalizeKimiIdentityFingerprint trims user input and applies the default enablement.
func NormalizeKimiIdentityFingerprint(in KimiIdentityFingerprintConfig) KimiIdentityFingerprintConfig {
	return defaultKimiIdentityFingerprintEnabled(CleanKimiIdentityFingerprint(in))
}

// CleanKimiIdentityFingerprint trims explicit overrides while preserving empty fields.
func CleanKimiIdentityFingerprint(in KimiIdentityFingerprintConfig) KimiIdentityFingerprintConfig {
	out := in
	out.UserAgent = strings.TrimSpace(out.UserAgent)
	out.Platform = strings.TrimSpace(out.Platform)
	out.Version = strings.TrimSpace(out.Version)
	out.DeviceName = strings.TrimSpace(out.DeviceName)
	out.DeviceModel = strings.TrimSpace(out.DeviceModel)
	out.CustomHeaders = cleanIdentityFingerprintHeaders(out.CustomHeaders)
	return out
}

// WithKimiHeaderDefaults folds the legacy kimi-header-defaults block into the
// fingerprint preset layer.
//
// Both settings describe the same three headers. Deployments configured before
// the fingerprint existed must keep their values, so the legacy block acts as a
// preset the fingerprint's own preset can still override, and both stay below a
// learned observation.
func WithKimiHeaderDefaults(fp KimiIdentityFingerprintConfig, legacy KimiHeaderDefaults) KimiIdentityFingerprintConfig {
	out := CleanKimiIdentityFingerprint(fp)
	if out.UserAgent == "" {
		out.UserAgent = strings.TrimSpace(legacy.UserAgent)
	}
	if out.Platform == "" {
		out.Platform = strings.TrimSpace(legacy.Platform)
	}
	if out.Version == "" {
		out.Version = strings.TrimSpace(legacy.Version)
	}
	return out
}

func defaultKimiIdentityFingerprintEnabled(in KimiIdentityFingerprintConfig) KimiIdentityFingerprintConfig {
	if !in.enabledSet {
		in.Enabled = true
	}
	return in
}

func kimiLegacyDefaultDisabled(fp KimiIdentityFingerprintConfig) bool {
	if fp.Enabled || len(fp.CustomHeaders) > 0 ||
		strings.TrimSpace(fp.DeviceName) != "" || strings.TrimSpace(fp.DeviceModel) != "" {
		return false
	}
	defaults := DefaultKimiIdentityFingerprint()
	return emptyOrEqual(fp.UserAgent, defaults.UserAgent) &&
		emptyOrEqual(fp.Platform, defaults.Platform) &&
		emptyOrEqual(fp.Version, defaults.Version)
}

func (fp *KimiIdentityFingerprintConfig) UnmarshalJSON(data []byte) error {
	type alias KimiIdentityFingerprintConfig
	var out alias
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	*fp = KimiIdentityFingerprintConfig(out)
	fp.enabledSet = jsonObjectHasKey(data, "enabled")
	return nil
}

func (fp *KimiIdentityFingerprintConfig) UnmarshalYAML(value *yaml.Node) error {
	type alias KimiIdentityFingerprintConfig
	var out alias
	if err := value.Decode(&out); err != nil {
		return err
	}
	*fp = KimiIdentityFingerprintConfig(out)
	fp.enabledSet = yamlMappingHasKey(value, "enabled")
	return nil
}
