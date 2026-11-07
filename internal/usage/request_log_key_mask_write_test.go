package usage

import (
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// storedKeyColumns reads the three identity columns of the single row a test
// wrote for the given model.
func storedKeyColumns(t *testing.T, model string) (apiKey, apiKeyID, apiKeyName string) {
	t.Helper()
	if err := getDB().QueryRow(
		`SELECT api_key, api_key_id, api_key_name FROM request_logs WHERE model = ?`, model,
	).Scan(&apiKey, &apiKeyID, &apiKeyName); err != nil {
		t.Fatalf("read request_logs row for %s: %v", model, err)
	}
	return apiKey, apiKeyID, apiKeyName
}

func TestRequestLogWriteStoresMaskAndKeyIDOnly(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	assertRequestLogWriteStoresMaskAndKeyIDOnly(t)
}

// assertRequestLogWriteStoresMaskAndKeyIDOnly runs on SQLite and on PostgreSQL.
func assertRequestLogWriteStoresMaskAndKeyIDOnly(t *testing.T) {
	t.Helper()

	const (
		registered = "sk-write-registered-0001"
		ghost      = "sk-write-unregistered-0002"
		explicitID = "sk-write-explicit-id-0003"
	)
	registerTestAPIKey(t, "write-key-id", registered, "Writer")

	now := time.Now().UTC()
	stats := TokenStats{TotalTokens: 1}
	InsertLog(registered, "request-time name", "model-registered", "src", "ch", "auth", false, now, 1, 1, stats, "", "")
	InsertLog(ghost, "Ghost", "model-ghost", "src", "ch", "auth", false, now, 1, 1, stats, "", "")
	InsertLogWithDetailsIdentity(explicitID, "explicit-key-id", "Explicit", "model-explicit", "src", "ch", "auth", false, now, 1, 1, stats, "", "", "")
	InsertLog("POST /v1/models", "", "model-route", "src", "ch", "auth", false, now, 1, 1, stats, "", "")
	InsertLog("warmup", "", "model-origin", "src", "ch", "auth", false, now, 1, 1, stats, "", "")
	InsertLog("", "", "model-nokey", "src", "ch", "auth", false, now, 1, 1, stats, "", "")

	cases := []struct {
		model, wantKey, wantID, wantName string
	}{
		// The row is tied to the key by id and shows only the mask; the name is the
		// key's current one.
		{"model-registered", MaskAPIKey(registered), "write-key-id", "Writer"},
		// A secret nobody stores is masked as well and has no id; the request-time
		// name stays as the only label the row can carry.
		{"model-ghost", MaskAPIKey(ghost), "", "Ghost"},
		// An id attached by the caller is kept even when the secret is unknown here.
		{"model-explicit", MaskAPIKey(explicitID), "explicit-key-id", "Explicit"},
		// Routes and internal origins are not secrets and stay readable: the
		// "system call" classification and the system filter are built on them.
		{"model-route", "POST /v1/models", "", ""},
		{"model-origin", "warmup", "", ""},
		{"model-nokey", "", "", ""},
	}
	for _, tc := range cases {
		gotKey, gotID, gotName := storedKeyColumns(t, tc.model)
		if gotKey != tc.wantKey || gotID != tc.wantID || gotName != tc.wantName {
			t.Errorf("%s stored (api_key, api_key_id, api_key_name) = (%q, %q, %q), want (%q, %q, %q)",
				tc.model, gotKey, gotID, gotName, tc.wantKey, tc.wantID, tc.wantName)
		}
	}

	// Nothing a secret could still be found in, in any column.
	for _, secret := range []string{registered, ghost, explicitID} {
		var leaked int
		if err := getDB().QueryRow(
			`SELECT COUNT(*) FROM request_logs WHERE (api_key || api_key_id || api_key_name) LIKE ?`, "%"+secret+"%",
		).Scan(&leaked); err != nil {
			t.Fatalf("scan for %q: %v", secret, err)
		}
		if leaked != 0 {
			t.Fatalf("%d request_logs rows still contain the secret %q", leaked, secret)
		}
	}
}

func TestRequestLogWriteMasksASecretThatReadsLikeALabel(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})

	// Custom keys can be any string, including one that looks like an origin
	// label. A stored key is a secret whatever it reads like.
	registerTestAPIKey(t, "label-like-key-id", "warmup", "Looks like a label")
	InsertLog("warmup", "", "model-label-like", "src", "ch", "auth", false, time.Now().UTC(), 1, 1, TokenStats{TotalTokens: 1}, "", "")

	gotKey, gotID, _ := storedKeyColumns(t, "model-label-like")
	if gotKey != MaskAPIKey("warmup") || gotID != "label-like-key-id" {
		t.Fatalf("stored (api_key, api_key_id) = (%q, %q), want the mask and the key id", gotKey, gotID)
	}
	rows, err := QueryLogs(LogQueryParams{Page: 1, Size: 10, Days: 1, APIKeyIDs: []string{"label-like-key-id"}})
	if err != nil {
		t.Fatalf("QueryLogs: %v", err)
	}
	if len(rows.Items) != 1 || rows.Items[0].APIKey != MaskAPIKey("warmup") {
		t.Fatalf("items = %#v, want one row showing the mask", rows.Items)
	}
}

func TestRequestLogWriteStillHandsTheSecretToTheTPMLimiter(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	registerTestAPIKey(t, "tpm-key-id", "sk-tpm-secret-0001", "TPM")

	var seen []string
	SetTokenUsageCallback(func(apiKey, _ string, _ int64) { seen = append(seen, apiKey) })
	t.Cleanup(func() { SetTokenUsageCallback(nil) })

	InsertLog("sk-tpm-secret-0001", "", "model-tpm", "src", "ch", "auth", false, time.Now().UTC(), 1, 1, TokenStats{TotalTokens: 5}, "", "")

	// The limiter counts per key in memory and is told which key by its secret;
	// only the persisted row is masked.
	if len(seen) != 1 || seen[0] != "sk-tpm-secret-0001" {
		t.Fatalf("token usage callback saw %q, want the unmasked key once", seen)
	}
	if gotKey, _, _ := storedKeyColumns(t, "model-tpm"); strings.Contains(gotKey, "tpm-secret") {
		t.Fatalf("stored api_key = %q, want a mask", gotKey)
	}
}

func TestRequestLogContentStaysReachableThroughTheKeyID(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{
		StoreContent:           true,
		ContentRetentionDays:   30,
		CleanupIntervalMinutes: 1440,
	})
	registerTestAPIKey(t, "content-key-id", "sk-content-secret-0001", "Content")
	registerTestAPIKey(t, "content-other-id", "sk-content-secret-0002", "Other")

	now := time.Now().UTC()
	InsertLog("sk-content-secret-0001", "", "model-content", "src", "ch", "auth", false, now, 1, 1, TokenStats{TotalTokens: 1}, `{"q":"mine"}`, `{"a":"mine"}`)
	InsertLog("sk-content-secret-0002", "", "model-content-other", "src", "ch", "auth", false, now, 1, 1, TokenStats{TotalTokens: 1}, `{"q":"theirs"}`, `{"a":"theirs"}`)

	mine, err := QueryLogs(LogQueryParams{Page: 1, Size: 10, Days: 1, APIKeyIDs: []string{"content-key-id"}})
	if err != nil || len(mine.Items) != 1 {
		t.Fatalf("QueryLogs: items=%v err=%v", mine.Items, err)
	}
	// A public lookup proves a log is the caller's by the key it presented; the
	// presented secret resolves to the id, the stored mask is not compared.
	if _, err := QueryLogContentForKey(mine.Items[0].ID, "sk-content-secret-0001"); err != nil {
		t.Fatalf("the key's own log must be readable through its secret: %v", err)
	}
	theirs, err := QueryLogs(LogQueryParams{Page: 1, Size: 10, Days: 1, APIKeyIDs: []string{"content-other-id"}})
	if err != nil || len(theirs.Items) != 1 {
		t.Fatalf("QueryLogs(other): items=%v err=%v", theirs.Items, err)
	}
	if _, err := QueryLogContentForKey(theirs.Items[0].ID, "sk-content-secret-0001"); err == nil {
		t.Fatalf("another key's log must not be readable through this secret")
	}
	if _, err := QueryLogContentForKey(mine.Items[0].ID, "sk-content-never-stored"); err == nil {
		t.Fatalf("a secret that matches no stored key must not unlock a log")
	}
	if _, err := QueryLogContentPartForKey(theirs.Items[0].ID, "sk-content-secret-0001", "input"); err == nil {
		t.Fatalf("another key's log part must not be readable through this secret")
	}
	if part, err := QueryLogContentPartForKey(mine.Items[0].ID, "sk-content-secret-0001", "input"); err != nil || part.Content != `{"q":"mine"}` {
		t.Fatalf("own log part = %#v, err = %v", part, err)
	}
}
