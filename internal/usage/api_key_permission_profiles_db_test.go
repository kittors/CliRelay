package usage

import "testing"

func TestAPIKeyPermissionProfilesReplaceAndList(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	usageDBMu.Lock()
	db := usageDB
	usageDBMu.Unlock()
	if db == nil {
		t.Fatal("expected test db")
	}
	initAPIKeyPermissionProfilesTable(db)

	profiles := []APIKeyPermissionProfileRow{
		{
			ID:                   "mixed-gpt-opencode",
			Name:                 "混合 gpt+opencode 模型",
			DailyLimit:           15000,
			TotalQuota:           0,
			ConcurrencyLimit:     0,
			RPMLimit:             0,
			TPMLimit:             0,
			AllowedChannelGroups: []string{"chatgpt-mix", "opencode"},
			AllowedChannels:      []string{},
			AllowedModels:        []string{},
			SystemPrompt:         "保持简洁",
		},
	}

	if err := ReplaceAllAPIKeyPermissionProfiles(profiles); err != nil {
		t.Fatalf("ReplaceAllAPIKeyPermissionProfiles: %v", err)
	}

	got := ListAPIKeyPermissionProfiles()
	if len(got) != 1 {
		t.Fatalf("ListAPIKeyPermissionProfiles len = %d, want 1", len(got))
	}
	if got[0].ID != profiles[0].ID || got[0].Name != profiles[0].Name {
		t.Fatalf("profile identity = %#v, want %#v", got[0], profiles[0])
	}
	if got[0].DailyLimit != 15000 || got[0].TotalQuota != 0 || got[0].ConcurrencyLimit != 0 || got[0].RPMLimit != 0 || got[0].TPMLimit != 0 {
		t.Fatalf("limits = %#v, want daily 15000 and other limits unlimited", got[0])
	}
	if len(got[0].AllowedChannelGroups) != 2 || got[0].AllowedChannelGroups[0] != "chatgpt-mix" || got[0].AllowedChannelGroups[1] != "opencode" {
		t.Fatalf("allowed-channel-groups = %#v, want [chatgpt-mix opencode]", got[0].AllowedChannelGroups)
	}
	if got[0].AllowedChannels == nil || got[0].AllowedModels == nil {
		t.Fatalf("empty restriction lists should round-trip as empty slices: %#v", got[0])
	}
	if got[0].SystemPrompt != profiles[0].SystemPrompt {
		t.Fatalf("system-prompt = %q, want %q", got[0].SystemPrompt, profiles[0].SystemPrompt)
	}

	if err := ReplaceAllAPIKeyPermissionProfiles(nil); err != nil {
		t.Fatalf("ReplaceAllAPIKeyPermissionProfiles(clear): %v", err)
	}
	if got := ListAPIKeyPermissionProfiles(); len(got) != 0 {
		t.Fatalf("ListAPIKeyPermissionProfiles after clear len = %d, want 0", len(got))
	}
}
