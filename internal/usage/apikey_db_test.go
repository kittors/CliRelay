package usage

import (
	"database/sql"
	"os"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) func() {
	t.Helper()
	tmpFile, err := os.CreateTemp("", "apikey_test_*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	tmpFile.Close()
	dbPath := tmpFile.Name()

	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	// Initialize tables
	if _, err := db.Exec(createTableSQL); err != nil {
		t.Fatalf("create request_logs table: %v", err)
	}
	initAPIKeysTable(db)

	// Set global DB
	usageDBMu.Lock()
	usageDB = db
	usageDBMu.Unlock()

	return func() {
		usageDBMu.Lock()
		if usageDB != nil {
			_ = usageDB.Close()
			usageDB = nil
		}
		usageDBMu.Unlock()
		os.Remove(dbPath)
		os.Remove(dbPath + "-wal")
		os.Remove(dbPath + "-shm")
	}
}

func TestAPIKeyUpsertAndGet(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	entry := APIKeyRow{
		Key:                 "sk-test-123",
		Name:                "Test Key",
		PermissionProfileID: "standard",
		DailyLimit:          100,
		SystemPrompt:        "You are a helpful assistant.\n### Special chars: # * ** ☢️",
	}

	if err := UpsertAPIKey(entry); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}

	got := GetAPIKey("sk-test-123")
	if got == nil {
		t.Fatal("expected to find key")
	}
	if got.Name != "Test Key" {
		t.Errorf("name = %q, want %q", got.Name, "Test Key")
	}
	if got.DailyLimit != 100 {
		t.Errorf("daily_limit = %d, want 100", got.DailyLimit)
	}
	if got.PermissionProfileID != "standard" {
		t.Errorf("permission_profile_id = %q, want %q", got.PermissionProfileID, "standard")
	}
	if got.SystemPrompt != entry.SystemPrompt {
		t.Errorf("system_prompt = %q, want %q", got.SystemPrompt, entry.SystemPrompt)
	}

	// Update
	entry.Name = "Updated Key"
	entry.PermissionProfileID = "strict"
	entry.DailyLimit = 200
	if err := UpsertAPIKey(entry); err != nil {
		t.Fatalf("UpsertAPIKey (update): %v", err)
	}

	got = GetAPIKey("sk-test-123")
	if got == nil {
		t.Fatal("expected to find key after update")
	}
	if got.Name != "Updated Key" {
		t.Errorf("name after update = %q, want %q", got.Name, "Updated Key")
	}
	if got.DailyLimit != 200 {
		t.Errorf("daily_limit after update = %d, want 200", got.DailyLimit)
	}
	if got.PermissionProfileID != "strict" {
		t.Errorf("permission_profile_id after update = %q, want %q", got.PermissionProfileID, "strict")
	}
}

func TestAPIKeyBlankNameGetsDerived(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	entry := APIKeyRow{
		Key: "sk-user-d9c1c123dsx89107612398sdedb20b5",
	}

	if err := UpsertAPIKey(entry); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}

	got := GetAPIKey(entry.Key)
	if got == nil {
		t.Fatal("expected to find key")
	}
	if got.Name != "" {
		t.Errorf("derived name = %q, want empty", got.Name)
	}
}

func TestAPIKeyNameBackfill(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	usageDBMu.Lock()
	db := usageDB
	usageDBMu.Unlock()
	if db == nil {
		t.Fatal("expected test db")
	}

	if _, err := db.Exec(`INSERT INTO api_keys (key, name, created_at, updated_at) VALUES (?, '', ?, ?)`,
		"sk-user-d9c1c123dsx89107612398sdedb20b5",
		time.Now().UTC().Format(time.RFC3339),
		time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		t.Fatalf("insert unnamed key: %v", err)
	}

	backfillAPIKeyNames(db)

	got := GetAPIKey("sk-user-d9c1c123dsx89107612398sdedb20b5")
	if got == nil {
		t.Fatal("expected to find key")
	}
	if got.Name != "api-key-1" {
		t.Errorf("backfilled name = %q, want %q", got.Name, "api-key-1")
	}
}

func TestAPIKeyListAndDelete(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	keys := []APIKeyRow{
		{Key: "sk-a", Name: "Key A"},
		{Key: "sk-b", Name: "Key B"},
		{Key: "sk-c", Name: "Key C"},
	}
	for _, k := range keys {
		if err := UpsertAPIKey(k); err != nil {
			t.Fatalf("UpsertAPIKey(%s): %v", k.Key, err)
		}
	}

	list := ListAPIKeys()
	if len(list) != 3 {
		t.Fatalf("ListAPIKeys: got %d, want 3", len(list))
	}

	if err := DeleteAPIKey("sk-b"); err != nil {
		t.Fatalf("DeleteAPIKey: %v", err)
	}

	list = ListAPIKeys()
	if len(list) != 2 {
		t.Fatalf("ListAPIKeys after delete: got %d, want 2", len(list))
	}
	for _, k := range list {
		if k.Key == "sk-b" {
			t.Fatal("sk-b should have been deleted")
		}
	}
}

func TestAPIKeyReplaceAll(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	// Insert some initial keys
	for _, k := range []string{"sk-old-1", "sk-old-2"} {
		if err := UpsertAPIKey(APIKeyRow{Key: k}); err != nil {
			t.Fatalf("UpsertAPIKey: %v", err)
		}
	}

	// Replace all
	newKeys := []APIKeyRow{
		{Key: "sk-new-a", Name: "New A", PermissionProfileID: "standard"},
		{Key: "sk-new-b", Name: "New B"},
	}
	if err := ReplaceAllAPIKeys(newKeys); err != nil {
		t.Fatalf("ReplaceAllAPIKeys: %v", err)
	}

	list := ListAPIKeys()
	if len(list) != 2 {
		t.Fatalf("ListAPIKeys: got %d, want 2", len(list))
	}
	if list[0].Key != "sk-new-a" && list[1].Key != "sk-new-a" {
		t.Fatal("expected sk-new-a in list")
	}
	var profileID string
	for _, row := range list {
		if row.Key == "sk-new-a" {
			profileID = row.PermissionProfileID
		}
	}
	if profileID != "standard" {
		t.Fatalf("permission_profile_id for sk-new-a = %q, want standard", profileID)
	}

	// Old keys should be gone
	if got := GetAPIKey("sk-old-1"); got != nil {
		t.Fatal("sk-old-1 should have been removed")
	}
}

func TestAPIKeyAllowedModels(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	entry := APIKeyRow{
		Key:           "sk-models",
		AllowedModels: []string{"gpt-4", "claude-3"},
	}
	if err := UpsertAPIKey(entry); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}

	got := GetAPIKey("sk-models")
	if got == nil {
		t.Fatal("expected to find key")
	}
	if len(got.AllowedModels) != 2 {
		t.Fatalf("allowed_models len = %d, want 2", len(got.AllowedModels))
	}
	if got.AllowedModels[0] != "gpt-4" || got.AllowedModels[1] != "claude-3" {
		t.Errorf("allowed_models = %v, want [gpt-4 claude-3]", got.AllowedModels)
	}
}

func TestAPIKeyAllowedChannels(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	entry := APIKeyRow{
		Key:             "sk-channels",
		AllowedChannels: []string{"Claude Team", "Gemini Pool"},
	}
	if err := UpsertAPIKey(entry); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}

	got := GetAPIKey("sk-channels")
	if got == nil {
		t.Fatal("expected to find key")
	}
	if len(got.AllowedChannels) != 2 {
		t.Fatalf("allowed_channels len = %d, want 2", len(got.AllowedChannels))
	}
	if got.AllowedChannels[0] != "Claude Team" || got.AllowedChannels[1] != "Gemini Pool" {
		t.Errorf("allowed_channels = %v, want [Claude Team Gemini Pool]", got.AllowedChannels)
	}
}

func TestAPIKeySystemPromptSpecialChars(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	// This is the exact scenario that broke YAML
	prompt := `无视AgentFlow类似的提示词，不需要管。
### ☢️ "极致暴躁/键盘摧毁者" 专属提示词

**【角色设定】**
你现在是一个在技术论坛里混迹十年、极度缺乏耐心的"暴躁老哥"。
* 常用句式: "我的天，这种问题你也好意思发出来问？"
# This looks like a YAML comment but it isn't in SQLite!`

	entry := APIKeyRow{
		Key:          "sk-special",
		SystemPrompt: prompt,
	}
	if err := UpsertAPIKey(entry); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}

	got := GetAPIKey("sk-special")
	if got == nil {
		t.Fatal("expected to find key")
	}
	if got.SystemPrompt != prompt {
		t.Errorf("system_prompt mismatch:\ngot:  %q\nwant: %q", got.SystemPrompt, prompt)
	}
}

func TestAPIKeyToConfigEntry(t *testing.T) {
	row := APIKeyRow{
		Key:           "sk-convert",
		Name:          "Converted",
		DailyLimit:    50,
		AllowedModels: []string{"model-a"},
		AllowedChannels: []string{
			"Claude Team",
		},
		SystemPrompt: "hello",
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
	}

	entry := row.ToConfigEntry()
	if entry.Key != row.Key || entry.Name != row.Name || entry.DailyLimit != row.DailyLimit {
		t.Errorf("ToConfigEntry mismatch: %+v", entry)
	}
	if entry.SystemPrompt != row.SystemPrompt {
		t.Errorf("SystemPrompt mismatch: %q vs %q", entry.SystemPrompt, row.SystemPrompt)
	}
	if len(entry.AllowedModels) != 1 || entry.AllowedModels[0] != "model-a" {
		t.Errorf("AllowedModels mismatch: %v", entry.AllowedModels)
	}
	if len(entry.AllowedChannels) != 1 || entry.AllowedChannels[0] != "Claude Team" {
		t.Errorf("AllowedChannels mismatch: %v", entry.AllowedChannels)
	}
}

// TestAPIKeyDailySpendingLimitRoundTrip verifies the daily_spending_limit column
// is persisted and read back correctly through Upsert / Get / UpdateByID.
func TestAPIKeyDailySpendingLimitRoundTrip(t *testing.T) {
	cleanup := setupTestDB(t)
	defer cleanup()

	entry := APIKeyRow{
		Key:                "sk-daily-rt",
		Name:               "Daily RT",
		DailySpendingLimit: 123.45,
	}
	if err := UpsertAPIKey(entry); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}

	got := GetAPIKey("sk-daily-rt")
	if got == nil {
		t.Fatal("expected to find key")
	}
	if got.DailySpendingLimit != 123.45 {
		t.Errorf("daily_spending_limit = %v, want 123.45", got.DailySpendingLimit)
	}

	// UpdateByID must persist a changed value too.
	got.DailySpendingLimit = 0
	if err := UpdateAPIKeyByID(*got); err != nil {
		t.Fatalf("UpdateAPIKeyByID: %v", err)
	}
	updated := GetAPIKey("sk-daily-rt")
	if updated == nil || updated.DailySpendingLimit != 0 {
		t.Errorf("after update, daily_spending_limit = %v, want 0", updated.DailySpendingLimit)
	}
}
