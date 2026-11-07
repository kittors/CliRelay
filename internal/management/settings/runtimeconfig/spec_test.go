package runtimeconfig

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestIdentityFingerprintRuntimeSettingMigratesLegacyEmptyDisabledProviders(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	spec := identityFingerprintSpec(t)
	raw := json.RawMessage(`{
		"codex": {"enabled": true},
		"claude": {"enabled": false},
		"gemini": {"enabled": false}
	}`)

	if !spec.Apply(cfg, raw) {
		t.Fatal("Apply returned false")
	}

	if !cfg.IdentityFingerprint.Codex.Enabled ||
		!cfg.IdentityFingerprint.Claude.Enabled ||
		!cfg.IdentityFingerprint.Gemini.Enabled ||
		!cfg.IdentityFingerprint.XAI.Enabled {
		t.Fatalf("IdentityFingerprint = %#v, want legacy empty disabled providers defaulted on", cfg.IdentityFingerprint)
	}
}

func TestIdentityFingerprintRuntimeSettingVersionedPayloadPreservesExplicitDisabledProviders(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	spec := identityFingerprintSpec(t)
	raw := json.RawMessage(`{
		"runtime-setting-version": 2,
		"codex": {"enabled": true},
		"claude": {"enabled": false},
		"gemini": {"enabled": false},
		"xai": {"enabled": false}
	}`)

	if !spec.Apply(cfg, raw) {
		t.Fatal("Apply returned false")
	}

	if !cfg.IdentityFingerprint.Codex.Enabled {
		t.Fatalf("Codex.Enabled = false, want true")
	}
	if cfg.IdentityFingerprint.Claude.Enabled ||
		cfg.IdentityFingerprint.Gemini.Enabled ||
		cfg.IdentityFingerprint.XAI.Enabled {
		t.Fatalf("IdentityFingerprint = %#v, want versioned explicit false preserved", cfg.IdentityFingerprint)
	}
}

func TestIdentityFingerprintRuntimeSettingValueWritesVersionedPayload(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(IdentityFingerprintRuntimeSettingValue(config.IdentityFingerprintConfig{}))
	if err != nil {
		t.Fatalf("Marshal IdentityFingerprintRuntimeSettingValue: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("Unmarshal payload: %v", err)
	}
	if payload["runtime-setting-version"] != float64(identityFingerprintRuntimeSettingVersion) {
		t.Fatalf("runtime-setting-version = %#v, want %d", payload["runtime-setting-version"], identityFingerprintRuntimeSettingVersion)
	}
}

func identityFingerprintSpec(t *testing.T) Spec {
	t.Helper()
	for _, spec := range Specs() {
		if spec.Key == RuntimeSettingIdentityFingerprint {
			return spec
		}
	}
	t.Fatal("identity-fingerprint spec not found")
	return Spec{}
}

// The global disable-cooling switch is cluster-wide: a panel save lands on one
// node, so like the retry settings it lives in runtime_settings.
func TestDisableCoolingIsARuntimeSetting(t *testing.T) {
	t.Parallel()

	spec, ok := SpecByKey(RuntimeSettingDisableCooling)
	if !ok {
		t.Fatal("disable-cooling is not a runtime setting")
	}
	if spec.Meaningful(&config.Config{}) {
		t.Fatal("the default, cooling on, counts as a value to import")
	}
	on := &config.Config{DisableCooling: true}
	if !spec.Meaningful(on) {
		t.Fatal("disable-cooling: true is not imported from config.yaml")
	}
	raw, err := Canonical(spec, on)
	if err != nil || string(raw) != "true" {
		t.Fatalf("canonical = %s, %v; want true", raw, err)
	}
	restored := &config.Config{}
	if !spec.Apply(restored, raw) || !restored.DisableCooling {
		t.Fatal("the stored value did not restore disable-cooling")
	}
}
