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
