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
