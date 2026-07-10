package usage

import (
	"errors"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identityfingerprint"
)

func TestIdentityFingerprintAccountPolicySelectsAndRepairsActiveProfile(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	accountKey := "codex-policy-account"
	for _, record := range []*identityfingerprint.LearnedRecord{
		{
			Provider:      identityfingerprint.ProviderCodex,
			AccountKey:    accountKey,
			ProfileKey:    "codex_cli_rs",
			ProfileFamily: identityfingerprint.ProfileFamilyCLI,
			ClientProduct: "codex_cli_rs",
			ClientVariant: "codex_cli_rs",
			Version:       "0.144.1",
			Fields: map[string]string{
				identityfingerprint.FieldUserAgent:       "codex_cli_rs/0.144.1 (Mac OS 26.5.2; arm64) unknown",
				identityfingerprint.FieldCodexOriginator: "codex_cli_rs",
				identityfingerprint.FieldCodexVersion:    "0.144.1",
			},
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), LastSeenAt: time.Now().UTC(),
		},
		{
			Provider:      identityfingerprint.ProviderCodex,
			AccountKey:    accountKey,
			ProfileKey:    identityfingerprint.ProfileKeyCodexDesktop,
			ProfileFamily: identityfingerprint.ProfileFamilyDesktop,
			ClientProduct: "codex",
			ClientVariant: "Codex Desktop",
			Version:       "0.144.0",
			Fields: map[string]string{
				identityfingerprint.FieldUserAgent:       "Codex Desktop/0.144.0-alpha.4",
				identityfingerprint.FieldCodexOriginator: "Codex Desktop",
				identityfingerprint.FieldCodexVersion:    "0.144.0",
			},
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), LastSeenAt: time.Now().UTC(),
		},
	} {
		if err := UpsertIdentityFingerprint(record); err != nil {
			t.Fatalf("UpsertIdentityFingerprint(%s): %v", record.ProfileKey, err)
		}
	}

	initial, err := GetIdentityFingerprintAccountPolicy(identityfingerprint.ProviderCodex, accountKey)
	if err != nil {
		t.Fatalf("GetIdentityFingerprintAccountPolicy: %v", err)
	}
	if initial.Strategy != identityfingerprint.AccountStrategyCLIPreferred || initial.Revision != 0 {
		t.Fatalf("initial policy = %+v", initial)
	}

	saved, err := SaveIdentityFingerprintAccountPolicy(identityfingerprint.AccountPolicy{
		Provider:         identityfingerprint.ProviderCodex,
		AccountKey:       accountKey,
		Strategy:         identityfingerprint.AccountStrategyActiveProfile,
		ActiveProfileKey: identityfingerprint.ProfileKeyCodexDesktop,
	}, initial.Revision)
	if err != nil {
		t.Fatalf("SaveIdentityFingerprintAccountPolicy: %v", err)
	}
	if saved.Revision != 1 || saved.ActiveProfileKey != identityfingerprint.ProfileKeyCodexDesktop {
		t.Fatalf("saved policy = %+v", saved)
	}
	if _, err := SaveIdentityFingerprintAccountPolicy(identityfingerprint.AccountPolicy{
		Provider: identityfingerprint.ProviderCodex, AccountKey: accountKey, Strategy: identityfingerprint.AccountStrategyCLIPreferred,
	}, 0); !errors.Is(err, ErrIdentityFingerprintPolicyConflict) {
		t.Fatalf("stale revision 0 update error = %v, want conflict", err)
	}

	deleted, repaired, err := DeleteIdentityFingerprintProfileAndRepairPolicy(
		identityfingerprint.ProviderCodex,
		accountKey,
		identityfingerprint.ProfileKeyCodexDesktop,
	)
	if err != nil {
		t.Fatalf("DeleteIdentityFingerprintProfileAndRepairPolicy: %v", err)
	}
	if deleted != 1 || repaired.Strategy != identityfingerprint.AccountStrategyCLIPreferred || repaired.ActiveProfileKey != "" || repaired.Revision != 2 {
		t.Fatalf("deleted=%d repaired=%+v", deleted, repaired)
	}
}

func TestIdentityFingerprintAccountPolicyRejectsMixedProfile(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	accountKey := "codex-mixed-policy-account"
	record := &identityfingerprint.LearnedRecord{
		Provider: identityfingerprint.ProviderCodex, AccountKey: accountKey,
		ProfileKey: identityfingerprint.ProfileKeyCodexDesktop, ProfileFamily: identityfingerprint.ProfileFamilyDesktop,
		Fields: map[string]string{
			identityfingerprint.FieldUserAgent:       "codex_cli_rs/0.144.1",
			identityfingerprint.FieldCodexOriginator: "Codex Desktop",
		},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), LastSeenAt: time.Now().UTC(),
	}
	if err := UpsertIdentityFingerprint(record); err != nil {
		t.Fatalf("UpsertIdentityFingerprint: %v", err)
	}
	_, err := SaveIdentityFingerprintAccountPolicy(identityfingerprint.AccountPolicy{
		Provider: identityfingerprint.ProviderCodex, AccountKey: accountKey,
		Strategy: identityfingerprint.AccountStrategyActiveProfile, ActiveProfileKey: identityfingerprint.ProfileKeyCodexDesktop,
	}, 0)
	if !errors.Is(err, ErrIdentityFingerprintProfileNotSelectable) {
		t.Fatalf("mixed profile selection error = %v, want not selectable", err)
	}
}
