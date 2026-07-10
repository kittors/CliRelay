package identityfingerprint

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestCodexProfileKeySeparatesCLIAndDesktop(t *testing.T) {
	tests := []struct {
		name       string
		ua         string
		originator string
		wantKey    string
		wantFamily string
		wantOK     bool
	}{
		{
			name:       "cli",
			ua:         "codex_cli_rs/0.144.1 (Mac OS 26.5.2; arm64) unknown",
			originator: "codex_cli_rs",
			wantKey:    "codex_cli_rs",
			wantFamily: ProfileFamilyCLI,
			wantOK:     true,
		},
		{
			name:       "desktop",
			ua:         "Codex Desktop/0.144.0-alpha.4 (Mac OS 26.5.2; arm64) unknown (Codex Desktop; 26.707.31123)",
			originator: "Codex Desktop",
			wantKey:    ProfileKeyCodexDesktop,
			wantFamily: ProfileFamilyDesktop,
			wantOK:     true,
		},
		{
			name:       "conflicting signals",
			ua:         "codex_cli_rs/0.144.1 (Mac OS 26.5.2; arm64) unknown",
			originator: "Codex Desktop",
			wantFamily: ProfileFamilyUnknown,
			wantOK:     false,
		},
		{
			name:       "recognized ua with unknown originator",
			ua:         "codex_cli_rs/0.144.1 (Mac OS 26.5.2; arm64) unknown",
			originator: "unknown-client",
			wantFamily: ProfileFamilyUnknown,
			wantOK:     false,
		},
		{
			name:       "recognized originator with unknown ua",
			ua:         "unknown-codex-wrapper/1.0.0",
			originator: "codex_cli_rs",
			wantFamily: ProfileFamilyUnknown,
			wantOK:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, family, ok := CodexProfileKey(tt.ua, tt.originator)
			if key != tt.wantKey || family != tt.wantFamily || ok != tt.wantOK {
				t.Fatalf("CodexProfileKey() = %q/%q/%v, want %q/%q/%v", key, family, ok, tt.wantKey, tt.wantFamily, tt.wantOK)
			}
		})
	}
}

func TestNormalizeAccountPolicyClearsInactiveProfile(t *testing.T) {
	policy := NormalizeAccountPolicy(ProviderCodex, "acct", AccountPolicy{
		Strategy:         AccountStrategyCLIPreferred,
		ActiveProfileKey: ProfileKeyCodexDesktop,
	})
	if policy.ActiveProfileKey != "" {
		t.Fatalf("cli preferred policy retained active profile %q", policy.ActiveProfileKey)
	}
}

func TestSelectCodexProfileUsesExactlyOneProfile(t *testing.T) {
	now := time.Now().UTC()
	profiles := []LearnedRecord{
		{
			Provider: ProviderCodex, AccountKey: "acct", ProfileKey: ProfileKeyCodexDesktop, ProfileFamily: ProfileFamilyDesktop, LastSeenAt: now,
			Fields: map[string]string{FieldUserAgent: "Codex Desktop/0.144.0-alpha.4", FieldCodexOriginator: "Codex Desktop"},
		},
		{
			Provider: ProviderCodex, AccountKey: "acct", ProfileKey: "codex_cli_rs", ProfileFamily: ProfileFamilyCLI, LastSeenAt: now.Add(-time.Hour),
			Fields: map[string]string{FieldUserAgent: "codex_cli_rs/0.144.1", FieldCodexOriginator: "codex_cli_rs"},
		},
	}

	selection := SelectCodexProfile(profiles, AccountPolicy{Provider: ProviderCodex, AccountKey: "acct", Strategy: AccountStrategyCLIPreferred})
	if selection.Profile == nil || selection.Profile.ProfileKey != "codex_cli_rs" || selection.Reason != "cli_preferred" {
		t.Fatalf("CLI selection = %+v", selection)
	}

	selection = SelectCodexProfile(profiles, AccountPolicy{
		Provider:         ProviderCodex,
		AccountKey:       "acct",
		Strategy:         AccountStrategyActiveProfile,
		ActiveProfileKey: ProfileKeyCodexDesktop,
	})
	if selection.Profile == nil || selection.Profile.ProfileKey != ProfileKeyCodexDesktop || selection.Reason != "active_profile" {
		t.Fatalf("active selection = %+v", selection)
	}
}

func TestSelectCodexProfileSkipsIneligibleHistoricalBundle(t *testing.T) {
	now := time.Now().UTC()
	profiles := []LearnedRecord{
		{
			Provider: ProviderCodex, AccountKey: "acct", ProfileKey: ProfileKeyCodexDesktop, ProfileFamily: ProfileFamilyDesktop, LastSeenAt: now,
			Fields: map[string]string{FieldUserAgent: "codex_cli_rs/0.144.1", FieldCodexOriginator: "Codex Desktop"},
		},
		{
			Provider: ProviderCodex, AccountKey: "acct", ProfileKey: "codex_cli_rs", ProfileFamily: ProfileFamilyCLI, LastSeenAt: now.Add(-time.Hour),
			Fields: map[string]string{FieldUserAgent: "codex_cli_rs/0.144.0", FieldCodexOriginator: "codex_cli_rs"},
		},
	}

	selection := SelectCodexProfile(profiles, AccountPolicy{
		Provider: ProviderCodex, AccountKey: "acct", Strategy: AccountStrategyActiveProfile, ActiveProfileKey: ProfileKeyCodexDesktop,
	})
	if selection.Profile == nil || selection.Profile.ProfileKey != "codex_cli_rs" || selection.Reason != "active_profile_missing_cli_preferred" {
		t.Fatalf("selection = %+v, want safe CLI fallback", selection)
	}
	if eligible, reason := CodexProfileOutboundEligibility(&profiles[0]); eligible || reason != "conflicting_identity_fields" {
		t.Fatalf("mixed historical profile eligibility = %v/%q", eligible, reason)
	}
}

func TestSelectCodexProfileReportsNoSelectableProfile(t *testing.T) {
	selection := SelectCodexProfile([]LearnedRecord{{
		Provider: ProviderCodex, AccountKey: "acct", ProfileKey: "codex_quarantined", ProfileFamily: ProfileFamilyUnknown,
		Fields: map[string]string{FieldUserAgent: "codex_cli_rs/0.144.1", FieldCodexOriginator: "Codex Desktop"},
	}}, AccountPolicy{Provider: ProviderCodex, AccountKey: "acct"})
	if selection.Profile != nil || selection.Reason != "no_selectable_profile" {
		t.Fatalf("selection = %+v", selection)
	}
}

func TestResolveCodexSafeFallbackRejectsMixedAccountPreset(t *testing.T) {
	resolved, effective := ResolveCodexSafeFallback(config.CodexIdentityFingerprintConfig{
		Enabled:       true,
		UserAgent:     "codex_cli_rs/0.144.1",
		Originator:    "Codex Desktop",
		Version:       "0.144.0",
		BetaFeatures:  "desktop_only",
		WebsocketBeta: "responses_websockets=desktop",
	})
	if resolved.UserAgent != config.DefaultCodexFingerprintUserAgent || resolved.Originator != config.DefaultCodexFingerprintOriginator {
		t.Fatalf("safe fallback = %#v, want coherent builtin profile", resolved)
	}
	if resolved.Version != config.DefaultCodexFingerprintVersion || resolved.BetaFeatures != config.DefaultCodexFingerprintBetaFeatures {
		t.Fatalf("safe fallback retained mixed fields: %#v", resolved)
	}
	if got := effective.Fields[FieldUserAgent].Source; got != FieldSourceBuiltinDefault {
		t.Fatalf("safe fallback user-agent source = %q", got)
	}
	if effective.ProfileKey != "codex-tui" || effective.ProfileFamily != ProfileFamilyCLI {
		t.Fatalf("safe fallback metadata = %#v", effective)
	}
}

func TestResolveCodexProfileNeverFillsFromAnotherProductPreset(t *testing.T) {
	profile := &LearnedRecord{
		Provider:      ProviderCodex,
		AccountKey:    "acct",
		ProfileKey:    "codex_cli_rs",
		ProfileFamily: ProfileFamilyCLI,
		Version:       "0.144.1",
		Fields: map[string]string{
			FieldUserAgent:       "codex_cli_rs/0.144.1 (Mac OS 26.5.2; arm64) unknown",
			FieldCodexOriginator: "codex_cli_rs",
			FieldCodexVersion:    "0.144.1",
		},
	}
	resolved, effective := ResolveCodexProfile(config.CodexIdentityFingerprintConfig{
		Enabled:       true,
		UserAgent:     "Codex Desktop/0.144.0-alpha.4",
		Originator:    "Codex Desktop",
		Version:       "0.144.0",
		WebsocketBeta: "responses_websockets=desktop",
		BetaFeatures:  "desktop_only",
	}, profile)

	if resolved.UserAgent != profile.Fields[FieldUserAgent] || resolved.Originator != "codex_cli_rs" || resolved.Version != "0.144.1" {
		t.Fatalf("resolved = %#v, want complete CLI bundle", resolved)
	}
	if resolved.WebsocketBeta != "" || resolved.BetaFeatures != "" {
		t.Fatalf("resolved profile mixed Desktop fallback fields: %#v", resolved)
	}
	if effective.ProfileKey != "codex_cli_rs" || effective.ProfileFamily != ProfileFamilyCLI {
		t.Fatalf("effective profile = %#v", effective)
	}
}
