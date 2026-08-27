package identityfingerprint

import (
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func kimiClientHeaders() http.Header {
	headers := http.Header{}
	headers.Set("User-Agent", "KimiCLI/1.12.0")
	headers.Set("X-Msh-Platform", "kimi_cli")
	headers.Set("X-Msh-Version", "1.12.0")
	headers.Set("X-Msh-Device-Name", "starship")
	headers.Set("X-Msh-Device-Model", "darwin arm64")
	headers.Set("X-Msh-Device-Id", "device-from-client")
	return headers
}

func TestExtractKimiObservationFromRealClientHeaders(t *testing.T) {
	obs, ok := ExtractObservation(LearnInput{
		Provider:   ProviderKimi,
		AccountKey: "acct",
		Headers:    kimiClientHeaders(),
		ObservedAt: time.Date(2026, 8, 27, 1, 2, 3, 0, time.UTC),
	})
	if !ok {
		t.Fatal("ExtractObservation returned false for a real kimi-cli request")
	}
	if obs.ClientProduct != "kimicli" || obs.Version != "1.12.0" {
		t.Fatalf("product/version = %s/%s, want kimicli/1.12.0", obs.ClientProduct, obs.Version)
	}
	if obs.ProfileKey != ProfileKeyDefault {
		t.Fatalf("profile key = %q, want the single default profile", obs.ProfileKey)
	}
	for field, want := range map[string]string{
		FieldUserAgent:       "KimiCLI/1.12.0",
		FieldKimiPlatform:    "kimi_cli",
		FieldKimiVersion:     "1.12.0",
		FieldKimiDeviceName:  "starship",
		FieldKimiDeviceModel: "darwin arm64",
		// Without the device id every credential that has none of its own goes out
		// with the same host fallback string, which is the cross-account collision
		// this observation exists to remove.
		FieldKimiDeviceID: "device-from-client",
	} {
		if got := obs.Fields[field]; got != want {
			t.Fatalf("field %s = %q, want %q", field, got, want)
		}
	}
}

func TestExtractKimiObservationFallsBackToPlatformHeader(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-Msh-Platform", "kimi_cli")
	headers.Set("X-Msh-Version", "1.9.2")

	obs, ok := ExtractObservation(LearnInput{Provider: ProviderKimi, AccountKey: "acct", Headers: headers})
	if !ok {
		t.Fatal("a request identified only by X-Msh-Platform should still be learnable")
	}
	if obs.ClientProduct != "kimi_cli" || obs.Version != "1.9.2" {
		t.Fatalf("product/version = %s/%s, want kimi_cli/1.9.2", obs.ClientProduct, obs.Version)
	}
}

func TestExtractKimiObservationRejectsForeignClients(t *testing.T) {
	headers := http.Header{}
	headers.Set("User-Agent", "curl/8.7.1")

	if _, ok := ExtractObservation(LearnInput{Provider: ProviderKimi, AccountKey: "acct", Headers: headers}); ok {
		t.Fatal("a caller that is not the kimi client must not pin the account to its identity")
	}
}

func TestResolveKimiPrefersLearnedOverPresetOverDefault(t *testing.T) {
	learned := &LearnedRecord{
		Provider:   ProviderKimi,
		AccountKey: "acct",
		ProfileKey: ProfileKeyDefault,
		Fields: map[string]string{
			FieldUserAgent:      "KimiCLI/1.12.0",
			FieldKimiVersion:    "1.12.0",
			FieldKimiDeviceName: "starship",
			FieldKimiDeviceID:   "device-from-client",
		},
	}
	preset := config.KimiIdentityFingerprintConfig{
		Enabled:   true,
		UserAgent: "KimiCLI/1.11.0",
		Platform:  "kimi_cli_custom",
	}

	resolved, effective := ResolveKimi(preset, learned)

	if resolved.UserAgent != "KimiCLI/1.12.0" {
		t.Fatalf("user agent = %q, want the learned value", resolved.UserAgent)
	}
	if resolved.Platform != "kimi_cli_custom" {
		t.Fatalf("platform = %q, want the operator preset when nothing was learned", resolved.Platform)
	}
	if resolved.DeviceModel != "" {
		t.Fatalf("device model = %q, want empty so the runtime keeps its host fallback", resolved.DeviceModel)
	}
	if effective.Fields[FieldUserAgent].Source != FieldSourceLearned {
		t.Fatalf("user agent source = %q, want learned", effective.Fields[FieldUserAgent].Source)
	}
	if effective.Fields[FieldKimiPlatform].Source != FieldSourcePreset {
		t.Fatalf("platform source = %q, want preset", effective.Fields[FieldKimiPlatform].Source)
	}
	if effective.Version != "1.12.0" {
		t.Fatalf("effective version = %q, want the learned client version", effective.Version)
	}
	if got := KimiLearnedDeviceID(learned); got != "device-from-client" {
		t.Fatalf("learned device id = %q, want the observed value", got)
	}
}

func TestResolveKimiWithoutLearnedKeepsPreviousHardcodedIdentity(t *testing.T) {
	resolved, effective := ResolveKimi(config.KimiIdentityFingerprintConfig{Enabled: true}, nil)

	if resolved.UserAgent != config.DefaultKimiFingerprintUserAgent ||
		resolved.Platform != config.DefaultKimiFingerprintPlatform ||
		resolved.Version != config.DefaultKimiFingerprintVersion {
		t.Fatalf("resolved = %+v, want the builtin kimi-cli template", resolved)
	}
	if effective.Fields[FieldUserAgent].Source != FieldSourceBuiltinDefault {
		t.Fatalf("user agent source = %q, want builtin_default", effective.Fields[FieldUserAgent].Source)
	}
	if _, ok := effective.Fields[FieldKimiDeviceID]; ok {
		t.Fatal("device id must only appear once it has been observed")
	}
}

// The legacy kimi-header-defaults block predates the fingerprint and configures
// the same three headers, so an existing deployment must not have its values
// replaced by the builtin template.
func TestWithKimiHeaderDefaultsKeepsLegacyOverrides(t *testing.T) {
	legacy := config.KimiHeaderDefaults{UserAgent: "KimiCLI/1.8.0", Platform: "kimi_cli", Version: "1.8.0"}

	merged := config.WithKimiHeaderDefaults(config.KimiIdentityFingerprintConfig{Enabled: true}, legacy)
	resolved, _ := ResolveKimi(merged, nil)
	if resolved.UserAgent != "KimiCLI/1.8.0" || resolved.Version != "1.8.0" {
		t.Fatalf("resolved = %+v, want the legacy header defaults", resolved)
	}

	explicit := config.KimiIdentityFingerprintConfig{Enabled: true, UserAgent: "KimiCLI/1.13.0"}
	resolved, _ = ResolveKimi(config.WithKimiHeaderDefaults(explicit, legacy), nil)
	if resolved.UserAgent != "KimiCLI/1.13.0" {
		t.Fatalf("user agent = %q, want the fingerprint preset to win over the legacy block", resolved.UserAgent)
	}
	if resolved.Version != "1.8.0" {
		t.Fatalf("version = %q, want the legacy block to still fill unset fields", resolved.Version)
	}
}

func TestNormalizeKimiIdentityFingerprintDefaultsToEnabled(t *testing.T) {
	out := config.NormalizeKimiIdentityFingerprint(config.KimiIdentityFingerprintConfig{})
	if !out.Enabled {
		t.Fatal("kimi fingerprint must default to enabled like every other provider")
	}
}
