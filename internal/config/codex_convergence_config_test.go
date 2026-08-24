package config

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestNormalizeCodexIdentityFingerprintConvergenceDefaults(t *testing.T) {
	got := NormalizeCodexIdentityFingerprint(CodexIdentityFingerprintConfig{})
	if got.ConvergenceMode != CodexFingerprintConvergenceDevice {
		t.Fatalf("convergence mode = %q, want the device default", got.ConvergenceMode)
	}

	got = NormalizeCodexIdentityFingerprint(CodexIdentityFingerprintConfig{ConvergenceMode: "nonsense"})
	if got.ConvergenceMode != CodexFingerprintConvergenceDevice {
		t.Fatalf("convergence mode = %q, want an invalid value to fall back", got.ConvergenceMode)
	}

	got = NormalizeCodexIdentityFingerprint(CodexIdentityFingerprintConfig{ConvergenceMode: "  OFF  "})
	if got.ConvergenceMode != CodexFingerprintConvergenceOff {
		t.Fatalf("convergence mode = %q, want off to survive trimming and case", got.ConvergenceMode)
	}
}

func TestCodexIdentityFingerprintConvergenceRoundTrip(t *testing.T) {
	raw := []byte(`
enabled: true
convergence-mode: full
installation-id: 11111111-2222-3333-4444-555555555555
tls-fingerprint:
  enabled: true
  profile: firefox
`)
	var fromYAML CodexIdentityFingerprintConfig
	if err := yaml.Unmarshal(raw, &fromYAML); err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}
	if fromYAML.ConvergenceMode != CodexFingerprintConvergenceFull {
		t.Fatalf("convergence mode = %q, want full", fromYAML.ConvergenceMode)
	}
	if fromYAML.InstallationID != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("installation id = %q, want the configured value", fromYAML.InstallationID)
	}
	if !fromYAML.TLSFingerprint.Enabled || fromYAML.TLSFingerprint.Profile != "firefox" {
		t.Fatalf("tls fingerprint = %+v, want enabled firefox", fromYAML.TLSFingerprint)
	}

	encoded, err := json.Marshal(NormalizeCodexIdentityFingerprint(fromYAML))
	if err != nil {
		t.Fatalf("json marshal: %v", err)
	}
	var fromJSON CodexIdentityFingerprintConfig
	if err = json.Unmarshal(encoded, &fromJSON); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	if fromJSON.ConvergenceMode != CodexFingerprintConvergenceFull {
		t.Fatalf("convergence mode did not survive the JSON round trip: %q", fromJSON.ConvergenceMode)
	}
	if !fromJSON.TLSFingerprint.Enabled || fromJSON.TLSFingerprint.Profile != "firefox" {
		t.Fatalf("tls fingerprint did not survive the JSON round trip: %+v", fromJSON.TLSFingerprint)
	}
}

func TestCleanTLSFingerprintDropsUnknownProfile(t *testing.T) {
	got := CleanTLSFingerprint(TLSFingerprintConfig{Enabled: true, Profile: "netscape"})
	if got.Profile != "" {
		t.Fatalf("profile = %q, want an unknown client name dropped", got.Profile)
	}
	if !got.Enabled {
		t.Fatal("dropping an unknown profile must not silently disable the feature")
	}

	got = CleanTLSFingerprint(TLSFingerprintConfig{Enabled: true, Profile: "  ChRoMe "})
	if got.Profile != "chrome" {
		t.Fatalf("profile = %q, want it normalized to chrome", got.Profile)
	}
}

// Legacy runtime payloads predate these fields; they must still be recognised as
// the old "disabled default" so upgrading does not resurrect a stale config.
func TestCodexLegacyDefaultDetectionWithNewFields(t *testing.T) {
	legacy := CodexIdentityFingerprintConfig{}
	if !codexLegacyDefaultDisabled(legacy) {
		t.Fatal("an empty legacy payload must still be treated as the disabled default")
	}

	configured := CodexIdentityFingerprintConfig{ConvergenceMode: CodexFingerprintConvergenceOff}
	if codexLegacyDefaultDisabled(configured) {
		t.Fatal("an explicitly configured convergence mode must not be discarded as legacy")
	}

	pinned := CodexIdentityFingerprintConfig{InstallationID: "abc"}
	if codexLegacyDefaultDisabled(pinned) {
		t.Fatal("a pinned installation id must not be discarded as legacy")
	}

	tlsOn := CodexIdentityFingerprintConfig{TLSFingerprint: TLSFingerprintConfig{Enabled: true}}
	if codexLegacyDefaultDisabled(tlsOn) {
		t.Fatal("an enabled TLS fingerprint must not be discarded as legacy")
	}
}
