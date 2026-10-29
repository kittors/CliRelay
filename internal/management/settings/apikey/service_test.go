package apikey

import (
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

func setupTestDB(t *testing.T) {
	t.Helper()
	tmpFile, err := os.CreateTemp("", "apikey-settings-*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	dbPath := tmpFile.Name()
	_ = tmpFile.Close()
	t.Cleanup(func() {
		usage.CloseDB()
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
}

func TestReplaceKeysNormalizesAndListsEnabledKeys(t *testing.T) {
	setupTestDB(t)
	svc := NewService(nil)

	if err := svc.ReplaceKeys([]string{" sk-one ", "", "sk-two"}); err != nil {
		t.Fatalf("ReplaceKeys() error = %v, want nil", err)
	}
	if err := usage.UpsertAPIKey(usage.APIKeyRow{Key: "sk-disabled", Disabled: true}); err != nil {
		t.Fatalf("UpsertAPIKey(disabled): %v", err)
	}

	if got := svc.EnabledKeys(); !reflect.DeepEqual(got, []string{"sk-one", "sk-two"}) {
		t.Fatalf("EnabledKeys() = %#v, want sk-one/sk-two", got)
	}
}

func TestPatchAndDeleteKey(t *testing.T) {
	setupTestDB(t)
	svc := NewService(nil)

	if err := svc.PatchKey("", " sk-created "); err != nil {
		t.Fatalf("PatchKey(create) error = %v, want nil", err)
	}
	if got := usage.GetAPIKey("sk-created"); got == nil {
		t.Fatal("PatchKey(create) did not persist new key")
	}
	if err := svc.PatchKey(" sk-created ", " sk-renamed "); err != nil {
		t.Fatalf("PatchKey(rename) error = %v, want nil", err)
	}
	if got := usage.GetAPIKey("sk-created"); got != nil {
		t.Fatal("PatchKey(rename) kept old key")
	}
	if got := usage.GetAPIKey("sk-renamed"); got == nil {
		t.Fatal("PatchKey(rename) did not persist new key")
	}
	if err := svc.DeleteKey(" sk-renamed "); err != nil {
		t.Fatalf("DeleteKey() error = %v, want nil", err)
	}
	if got := usage.GetAPIKey("sk-renamed"); got != nil {
		t.Fatal("DeleteKey() kept deleted key")
	}
	if err := svc.DeleteKey(" "); !errors.Is(err, ErrMissingValue) {
		t.Fatalf("DeleteKey(blank) error = %v, want ErrMissingValue", err)
	}
}

func TestPatchEntryRenamePreservesStableID(t *testing.T) {
	setupTestDB(t)
	svc := NewService(nil)

	if err := usage.UpsertAPIKey(usage.APIKeyRow{ID: "key-1", Key: "sk-old", Name: "Old"}); err != nil {
		t.Fatalf("UpsertAPIKey(sk-old): %v", err)
	}

	newKey := "sk-new"
	newName := "Renamed"
	if err := svc.PatchEntry(&[]string{"key-1"}[0], nil, nil, EntryPatch{
		Key:  &newKey,
		Name: &newName,
	}); err != nil {
		t.Fatalf("PatchEntry() error = %v", err)
	}

	if got := usage.GetAPIKey("sk-old"); got != nil {
		t.Fatalf("old key should not remain addressable by key, got %#v", got)
	}
	got := usage.GetAPIKey("sk-new")
	if got == nil {
		t.Fatal("expected renamed API key to exist")
	}
	if got.ID != "key-1" {
		t.Fatalf("stable id = %q, want key-1", got.ID)
	}
	if got.Name != "Renamed" {
		t.Fatalf("name = %q, want Renamed", got.Name)
	}
}

func TestPatchEntryUpdatesDailySpendingLimit(t *testing.T) {
	setupTestDB(t)
	svc := NewService(nil)

	if err := usage.UpsertAPIKey(usage.APIKeyRow{ID: "key-1", Key: "sk-cost", DailySpendingLimit: 1}); err != nil {
		t.Fatalf("UpsertAPIKey(sk-cost): %v", err)
	}

	limit := 4.5
	if err := svc.PatchEntry(&[]string{"key-1"}[0], nil, nil, EntryPatch{DailySpendingLimit: &limit}); err != nil {
		t.Fatalf("PatchEntry() error = %v", err)
	}

	got := usage.GetAPIKey("sk-cost")
	if got == nil {
		t.Fatal("expected patched key")
	}
	// Fractional USD is ceiled to a whole dollar on save.
	if got.DailySpendingLimit != 5 {
		t.Fatalf("DailySpendingLimit = %v, want 5 (ceil whole USD)", got.DailySpendingLimit)
	}
}

func TestReplacePermissionProfilesValidatesAndSanitizes(t *testing.T) {
	setupTestDB(t)
	svc := NewService(func(channels []string) ([]string, error) {
		out := make([]string, 0, len(channels))
		for _, channel := range channels {
			if channel == "drop" {
				continue
			}
			out = append(out, channel)
		}
		return out, nil
	})

	err := svc.ReplacePermissionProfiles([]usage.APIKeyPermissionProfileRow{{
		ID:              " standard ",
		Name:            " Standard ",
		AllowedChannels: []string{"keep", "drop"},
	}})
	if err != nil {
		t.Fatalf("ReplacePermissionProfiles() error = %v, want nil", err)
	}

	got := svc.PermissionProfiles()
	if len(got) != 1 {
		t.Fatalf("PermissionProfiles() len = %d, want 1", len(got))
	}
	if got[0].ID != "standard" || got[0].Name != "Standard" {
		t.Fatalf("profile identity = %#v, want trimmed values", got[0])
	}
	if !reflect.DeepEqual(got[0].AllowedChannels, []string{"keep"}) {
		t.Fatalf("AllowedChannels = %#v, want keep", got[0].AllowedChannels)
	}
}

func TestReplacePermissionProfilesRejectsMissingIdentity(t *testing.T) {
	setupTestDB(t)
	svc := NewService(nil)

	if err := svc.ReplacePermissionProfiles([]usage.APIKeyPermissionProfileRow{{Name: "Name"}}); !errors.Is(err, ErrInvalidProfileID) {
		t.Fatalf("missing id error = %v, want ErrInvalidProfileID", err)
	}
	if err := svc.ReplacePermissionProfiles([]usage.APIKeyPermissionProfileRow{{ID: "standard"}}); !errors.Is(err, ErrInvalidProfileName) {
		t.Fatalf("missing name error = %v, want ErrInvalidProfileName", err)
	}
}

func TestRenameAndRemoveChannelRestrictions(t *testing.T) {
	setupTestDB(t)
	svc := NewService(nil)

	if err := usage.UpsertAPIKey(usage.APIKeyRow{
		Key:             "sk-channel",
		AllowedChannels: []string{"Team Old", "Other"},
	}); err != nil {
		t.Fatalf("UpsertAPIKey(sk-channel): %v", err)
	}
	if err := svc.ReplacePermissionProfiles([]usage.APIKeyPermissionProfileRow{{
		ID:              "standard",
		Name:            "Standard",
		AllowedChannels: []string{"Team Old", "Other"},
	}}); err != nil {
		t.Fatalf("ReplacePermissionProfiles() error = %v", err)
	}

	oldNameSet := map[string]struct{}{"team old": {}}
	if err := svc.RenameAllowedChannelRestrictions(oldNameSet, "Team New"); err != nil {
		t.Fatalf("RenameAllowedChannelRestrictions() error = %v", err)
	}
	if err := svc.RenamePermissionProfileChannelRestrictions(oldNameSet, "Team New"); err != nil {
		t.Fatalf("RenamePermissionProfileChannelRestrictions() error = %v", err)
	}

	key := usage.GetAPIKey("sk-channel")
	if key == nil || !reflect.DeepEqual(key.AllowedChannels, []string{"Team New", "Other"}) {
		t.Fatalf("renamed key channels = %#v, want Team New/Other", key)
	}
	profiles := svc.PermissionProfiles()
	if len(profiles) != 1 || !reflect.DeepEqual(profiles[0].AllowedChannels, []string{"Team New", "Other"}) {
		t.Fatalf("renamed profile channels = %#v, want Team New/Other", profiles)
	}

	removeSet := map[string]struct{}{"team new": {}}
	if err := svc.RemoveAllowedChannelRestrictions(removeSet); err != nil {
		t.Fatalf("RemoveAllowedChannelRestrictions() error = %v", err)
	}
	if err := svc.RemovePermissionProfileChannelRestrictions(removeSet); err != nil {
		t.Fatalf("RemovePermissionProfileChannelRestrictions() error = %v", err)
	}

	key = usage.GetAPIKey("sk-channel")
	if key == nil || !reflect.DeepEqual(key.AllowedChannels, []string{"Other"}) {
		t.Fatalf("removed key channels = %#v, want Other", key)
	}
	profiles = svc.PermissionProfiles()
	if len(profiles) != 1 || !reflect.DeepEqual(profiles[0].AllowedChannels, []string{"Other"}) {
		t.Fatalf("removed profile channels = %#v, want Other", profiles)
	}
}

func TestReplaceEntriesSanitizesAndValidates(t *testing.T) {
	setupTestDB(t)
	svc := NewService(
		func(channels []string) ([]string, error) {
			return []string{"known-channel"}, nil
		},
		WithChannelGroupValidator(func(groups []string) ([]string, error) {
			return groups, nil
		}),
		WithEntryValidator(func(entry config.APIKeyEntry) error {
			if !reflect.DeepEqual(entry.AllowedChannelGroups, []string{"pro"}) {
				t.Fatalf("AllowedChannelGroups = %#v, want normalized pro", entry.AllowedChannelGroups)
			}
			if !reflect.DeepEqual(entry.AllowedChannels, []string{"known-channel"}) {
				t.Fatalf("AllowedChannels = %#v, want sanitized channel", entry.AllowedChannels)
			}
			return nil
		}),
	)

	err := svc.ReplaceEntries([]config.APIKeyEntry{{
		Key:                  " sk-entry ",
		Name:                 " Entry ",
		AllowedChannels:      []string{"drop-me"},
		AllowedChannelGroups: []string{" PRO ", "pro"},
	}})
	if err != nil {
		t.Fatalf("ReplaceEntries() error = %v, want nil", err)
	}

	got := usage.GetAPIKey("sk-entry")
	if got == nil {
		t.Fatal("expected API key entry after replace")
	}
	if !reflect.DeepEqual(got.AllowedChannels, []string{"known-channel"}) {
		t.Fatalf("stored AllowedChannels = %#v, want sanitized channel", got.AllowedChannels)
	}
	if !reflect.DeepEqual(got.AllowedChannelGroups, []string{"pro"}) {
		t.Fatalf("stored AllowedChannelGroups = %#v, want normalized pro", got.AllowedChannelGroups)
	}
}

func TestPatchEntryValidationFailureKeepsOriginalKey(t *testing.T) {
	setupTestDB(t)
	svc := NewService(
		nil,
		WithEntryValidator(func(entry config.APIKeyEntry) error {
			return errors.New("invalid restriction")
		}),
	)

	if err := usage.UpsertAPIKey(usage.APIKeyRow{Key: "sk-old", Name: "Old"}); err != nil {
		t.Fatalf("UpsertAPIKey(sk-old): %v", err)
	}

	newKey := "sk-new"
	name := "Renamed"
	err := svc.PatchEntry(nil, nil, &[]string{"sk-old"}[0], EntryPatch{
		Key:  &newKey,
		Name: &name,
	})
	if !errors.Is(err, ErrInvalidEntry) {
		t.Fatalf("PatchEntry() error = %v, want ErrInvalidEntry", err)
	}
	if got := usage.GetAPIKey("sk-old"); got == nil || got.Name != "Old" {
		t.Fatalf("original key changed unexpectedly: %#v", got)
	}
	if got := usage.GetAPIKey("sk-new"); got != nil {
		t.Fatalf("new key should not exist after failed patch: %#v", got)
	}
}

func TestDeleteEntryByIndexDeletesLogsWhenRequested(t *testing.T) {
	setupTestDB(t)
	deletedIDs := make([]string, 0, 1)
	svc := NewService(
		nil,
		WithLogsDeleter(func(apiKeyID string) (int64, error) {
			deletedIDs = append(deletedIDs, apiKeyID)
			return 3, nil
		}),
	)

	if err := usage.UpsertAPIKey(usage.APIKeyRow{Key: "sk-delete"}); err != nil {
		t.Fatalf("UpsertAPIKey(sk-delete): %v", err)
	}
	// The history is removed by the key's id: by then the key row is gone, so
	// its secret can no longer be resolved.
	wantID := usage.GetAPIKey("sk-delete").ID

	result, err := svc.DeleteEntry("", nil, &[]int{0}[0], true)
	if err != nil {
		t.Fatalf("DeleteEntry() error = %v, want nil", err)
	}
	if result.LogsDeleted != 3 {
		t.Fatalf("LogsDeleted = %d, want 3", result.LogsDeleted)
	}
	if !reflect.DeepEqual(deletedIDs, []string{wantID}) {
		t.Fatalf("deletedIDs = %#v, want the key id %q", deletedIDs, wantID)
	}
	if got := usage.GetAPIKey("sk-delete"); got != nil {
		t.Fatalf("DeleteEntry() kept deleted key: %#v", got)
	}
}

func TestListEntriesHidesSoftDeletedOwnedKeys(t *testing.T) {
	setupTestDB(t)
	svc := NewService(nil)
	if err := usage.UpsertAPIKey(usage.APIKeyRow{
		ID: "owned-active", Key: "sk-owned-active", Name: "active", EndUserID: "eu-1",
	}); err != nil {
		t.Fatalf("UpsertAPIKey(active): %v", err)
	}
	if err := usage.UpsertAPIKey(usage.APIKeyRow{
		ID: "owned-tombstone", Key: "sk-deleted-abc", Name: "dead", EndUserID: "eu-1", Disabled: true,
	}); err != nil {
		t.Fatalf("UpsertAPIKey(tombstone): %v", err)
	}
	if err := usage.UpsertAPIKey(usage.APIKeyRow{
		ID: "owned-disabled", Key: "sk-owned-disabled", Name: "paused", EndUserID: "eu-1", Disabled: true,
	}); err != nil {
		t.Fatalf("UpsertAPIKey(disabled): %v", err)
	}
	if err := usage.UpsertAPIKey(usage.APIKeyRow{
		ID: "solo-disabled", Key: "sk-solo-disabled", Name: "solo", Disabled: true,
	}); err != nil {
		t.Fatalf("UpsertAPIKey(solo): %v", err)
	}

	entries, err := svc.ListEntriesWithDailySpending()
	if err != nil {
		t.Fatalf("ListEntriesWithDailySpending: %v", err)
	}
	foundTombstone, foundDisabled, foundActive, foundSolo := false, false, false, false
	for _, e := range entries {
		switch e.Key {
		case "sk-deleted-abc":
			foundTombstone = true
		case "sk-owned-disabled":
			foundDisabled = true
		case "sk-owned-active":
			foundActive = true
		case "sk-solo-disabled":
			foundSolo = true
		}
	}
	if foundTombstone {
		t.Fatalf("ListEntries leaked tombstoned owned key: %#v", entries)
	}
	if !foundDisabled || !foundActive || !foundSolo {
		t.Fatalf("ListEntries missing expected keys: disabled=%v active=%v solo=%v entries=%#v",
			foundDisabled, foundActive, foundSolo, entries)
	}
}

func TestListEntriesAttachesDailySpendingRuntime(t *testing.T) {
	setupTestDB(t)
	svc := NewService(nil)
	if err := usage.UpsertAPIKey(usage.APIKeyRow{ID: "key-1", Key: "sk-list", DailySpendingLimit: 100}); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}
	// Daily spending reads usage rollup, not raw request_logs.
	usage.InsertLogWithDetailsIdentity(
		"sk-list", "key-1", "list", "model", "test", "", "", false,
		usage.CutoffStartUTC(1).Add(time.Hour), 1, 0,
		usage.TokenStats{}, "", "", "",
	)
	// Force cost on the inserted row + day/lifetime rollup for deterministic assertions.
	db := usage.RuntimeDB()
	if _, err := db.Exec(`UPDATE request_logs SET cost = 20 WHERE api_key_id = 'key-1'`); err != nil {
		t.Fatalf("update log cost: %v", err)
	}
	if _, err := db.Exec(`UPDATE usage_rollup_buckets SET cost_total = 20 WHERE api_key_id = 'key-1'`); err != nil {
		t.Fatalf("update rollup cost: %v", err)
	}

	entries, err := svc.ListEntriesWithDailySpending()
	if err != nil {
		t.Fatalf("ListEntriesWithDailySpending: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len = %d", len(entries))
	}
	if entries[0].DailySpendingUsed != 20 {
		t.Fatalf("used = %v, want 20", entries[0].DailySpendingUsed)
	}
	if entries[0].DailySpendingRemaining == nil || *entries[0].DailySpendingRemaining != 80 {
		t.Fatalf("remaining = %v, want 80", entries[0].DailySpendingRemaining)
	}

	// unlimited
	zero := 0.0
	if err := svc.PatchEntry(&[]string{"key-1"}[0], nil, nil, EntryPatch{DailySpendingLimit: &zero}); err != nil {
		t.Fatalf("patch unlimited: %v", err)
	}
	entries, err = svc.ListEntriesWithDailySpending()
	if err != nil {
		t.Fatalf("ListEntriesWithDailySpending unlimited: %v", err)
	}
	if entries[0].DailySpendingRemaining != nil {
		t.Fatalf("unlimited remaining should be nil, got %v", *entries[0].DailySpendingRemaining)
	}
}

func TestResetDailySpendingAndRejectsUnlimited(t *testing.T) {
	setupTestDB(t)
	svc := NewService(nil)
	if err := usage.UpsertAPIKey(usage.APIKeyRow{ID: "key-1", Key: "sk-r", DailySpendingLimit: 100}); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}
	usage.InsertLogWithDetailsIdentity(
		"sk-r", "key-1", "r", "model", "test", "", "", false,
		usage.CutoffStartUTC(1).Add(time.Hour), 1, 0,
		usage.TokenStats{}, "", "", "",
	)
	db := usage.RuntimeDB()
	if _, err := db.Exec(`UPDATE request_logs SET cost = 20 WHERE api_key_id = 'key-1'`); err != nil {
		t.Fatalf("update log cost: %v", err)
	}
	if _, err := db.Exec(`UPDATE usage_rollup_buckets SET cost_total = 20 WHERE api_key_id = 'key-1'`); err != nil {
		t.Fatalf("update rollup cost: %v", err)
	}

	id := "key-1"
	actor := DailySpendingResetActor{UserID: "u1", Username: "alice", Kind: "user"}
	result, err := svc.ResetDailySpending(&id, nil, actor)
	if err != nil {
		t.Fatalf("ResetDailySpending: %v", err)
	}
	if result.DailySpendingUsed != 0 {
		t.Fatalf("used after reset = %v", result.DailySpendingUsed)
	}
	if result.DailySpendingRemaining == nil || *result.DailySpendingRemaining != 100 {
		t.Fatalf("remaining after reset = %v", result.DailySpendingRemaining)
	}
	if result.DailySpendingResetCount != 1 {
		t.Fatalf("reset count = %d, want 1", result.DailySpendingResetCount)
	}

	// request logs must remain
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM request_logs WHERE api_key_id = ?`, "key-1").Scan(&count); err != nil {
		t.Fatalf("count logs: %v", err)
	}
	if count != 1 {
		t.Fatalf("logs count = %d, want 1 (reset must not delete logs)", count)
	}

	got, err := usage.QueryTodayCostByKey("sk-r")
	if err != nil {
		t.Fatalf("QueryTodayCostByKey: %v", err)
	}
	if got != 0 {
		t.Fatalf("middleware-facing cost = %v, want 0", got)
	}

	events, err := svc.ListDailySpendingResetHistory(&id, nil, 10)
	if err != nil {
		t.Fatalf("ListDailySpendingResetHistory: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].ActorUsername != "alice" || events[0].EffectiveUsedBefore != 20 {
		t.Fatalf("event = %+v", events[0])
	}
	if events[0].RawTodayCost != 20 {
		t.Fatalf("raw_today_cost = %v, want 20", events[0].RawTodayCost)
	}

	// unlimited rejects reset
	zero := 0.0
	if err := svc.PatchEntry(&id, nil, nil, EntryPatch{DailySpendingLimit: &zero}); err != nil {
		t.Fatalf("clear limit: %v", err)
	}
	if _, err := svc.ResetDailySpending(&id, nil, actor); !errors.Is(err, ErrDailySpendingLimitMissing) {
		t.Fatalf("err = %v, want ErrDailySpendingLimitMissing", err)
	}
}

func TestEffectiveRowAppliesProfileDailySpendingLimit(t *testing.T) {
	setupTestDB(t)
	if err := usage.ReplaceAllAPIKeyPermissionProfiles([]usage.APIKeyPermissionProfileRow{{
		ID:                 "p1",
		Name:               "P1",
		DailyLimit:         5,
		DailySpendingLimit: 12.5, // ceiled to whole USD on save
	}}); err != nil {
		t.Fatalf("profiles: %v", err)
	}
	if err := usage.UpsertAPIKey(usage.APIKeyRow{
		ID:                  "key-1",
		Key:                 "sk-profile",
		PermissionProfileID: "p1",
		SpendingLimit:       77,
		DailySpendingLimit:  1, // stale key copy; profile wins
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	svc := NewService(nil)
	entries := svc.ListEntries()
	if len(entries) != 1 {
		t.Fatalf("len = %d", len(entries))
	}
	if entries[0].DailySpendingLimit != 13 {
		t.Fatalf("DailySpendingLimit = %v, want 13 from profile (ceil whole USD)", entries[0].DailySpendingLimit)
	}
	if entries[0].DailyLimit != 5 {
		t.Fatalf("DailyLimit from profile = %v, want 5", entries[0].DailyLimit)
	}
}
