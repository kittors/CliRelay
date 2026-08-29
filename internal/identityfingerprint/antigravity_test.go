package identityfingerprint

import (
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestAntigravityObservationAndResolution(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	headers := http.Header{
		"User-Agent": []string{"vscode/1.96.0 (Antigravity/4.3.0)"},
	}

	obs, ok := ExtractObservation(LearnInput{
		Provider:      ProviderAntigravity,
		AccountKey:    "test-account",
		AuthSubjectID: "sub-123",
		Headers:       headers,
		ObservedAt:    now,
	})
	if !ok {
		t.Fatalf("ExtractObservation failed for Antigravity headers")
	}

	if obs.Version != "4.3.0" {
		t.Errorf("obs.Version = %q, want 4.3.0", obs.Version)
	}
	if obs.Fields[FieldUserAgent] != "vscode/1.96.0 (Antigravity/4.3.0)" {
		t.Errorf("obs.Fields[FieldUserAgent] = %q", obs.Fields[FieldUserAgent])
	}

	mergeRes := MergeObservation(nil, obs)
	if !mergeRes.Changed || mergeRes.Record == nil {
		t.Fatalf("MergeObservation failed to create record: %+v", mergeRes)
	}

	cfg := config.AntigravityIdentityFingerprintConfig{
		Enabled: true,
	}
	resolved, eff := ResolveAntigravity(cfg, mergeRes.Record)
	if !resolved.Enabled || !eff.Enabled {
		t.Errorf("expected resolved and eff to be enabled")
	}
	if resolved.Version != "4.3.0" || eff.Version != "4.3.0" {
		t.Errorf("expected version 4.3.0, got resolved=%q eff=%q", resolved.Version, eff.Version)
	}
	if eff.Fields[FieldUserAgent].Source != FieldSourceLearned {
		t.Errorf("expected UserAgent source to be learned, got %v", eff.Fields[FieldUserAgent].Source)
	}
}

// Any client can reach the proxy, and most send a User-Agent of their own: a
// Node SDK sends a bare "node". Learning one of those pinned the account to an
// identity Antigravity refuses, taking every later request to 403 (#3501).
func TestAntigravityObservationRejectsForeignUserAgents(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	for _, ua := range []string{"", "node", "curl/8.7.1", "python-requests/2.32.3", "vscode/1.96.0"} {
		headers := http.Header{}
		if ua != "" {
			headers.Set("User-Agent", ua)
		}
		obs, ok := ExtractObservation(LearnInput{
			Provider:      ProviderAntigravity,
			AccountKey:    "test-account",
			AuthSubjectID: "sub-123",
			Headers:       headers,
			ObservedAt:    now,
		})
		if ok {
			t.Errorf("ExtractObservation learned non-Antigravity User-Agent %q as %+v", ua, obs.Fields)
		}
	}
}

// Records written before the observation guard was tightened still hold foreign
// identities, and a restored backup can reintroduce one at any time. Resolution
// must ignore them rather than send them upstream.
func TestResolveAntigravityIgnoresForeignLearnedUserAgent(t *testing.T) {
	learned := &LearnedRecord{
		Provider:   ProviderAntigravity,
		AccountKey: "test-account",
		ProfileKey: ProfileKeyDefault,
		Fields:     map[string]string{FieldUserAgent: "node"},
	}

	resolved, eff := ResolveAntigravity(config.AntigravityIdentityFingerprintConfig{Enabled: true}, learned)
	if resolved.UserAgent != config.DefaultAntigravityFingerprintUserAgent {
		t.Errorf("resolved.UserAgent = %q, want built-in default %q", resolved.UserAgent, config.DefaultAntigravityFingerprintUserAgent)
	}
	if got := eff.Fields[FieldUserAgent].Source; got != FieldSourceBuiltinDefault {
		t.Errorf("UserAgent source = %v, want %v", got, FieldSourceBuiltinDefault)
	}
	if resolved.Version != config.DefaultAntigravityFingerprintVersion {
		t.Errorf("resolved.Version = %q, want %q", resolved.Version, config.DefaultAntigravityFingerprintVersion)
	}

	// An operator-configured identity still outranks the built-in default.
	preset := config.AntigravityIdentityFingerprintConfig{
		Enabled:   true,
		UserAgent: "vscode/1.99.0 (Antigravity/4.9.1)",
	}
	resolved, eff = ResolveAntigravity(preset, learned)
	if resolved.UserAgent != preset.UserAgent {
		t.Errorf("resolved.UserAgent = %q, want preset %q", resolved.UserAgent, preset.UserAgent)
	}
	if got := eff.Fields[FieldUserAgent].Source; got != FieldSourcePreset {
		t.Errorf("UserAgent source = %v, want %v", got, FieldSourcePreset)
	}
}
