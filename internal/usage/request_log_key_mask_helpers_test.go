package usage

import (
	"testing"
	"time"
)

// registerTestAPIKey stores a standalone key, so requests that carry its secret
// are attributed to id the way production requests are.
func registerTestAPIKey(t *testing.T, id, secret, name string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if err := UpsertAPIKey(APIKeyRow{
		TenantID: systemTenantID, ID: id, Key: secret, Name: name, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("upsert api key %s: %v", id, err)
	}
}

// insertRawRequestLog writes a request_logs row the way an older build stored
// it, bypassing the write path that now masks api_key.
func insertRawRequestLog(t *testing.T, apiKey, apiKeyID, apiKeyName string, at time.Time) {
	t.Helper()
	if _, err := getDB().Exec(
		`INSERT INTO request_logs
			(timestamp, api_key, api_key_id, api_key_name, model, source, channel_name, auth_index,
			 failed, latency_ms, first_token_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens, cost)
		 VALUES (?, ?, ?, ?, 'gpt-test', 'source', 'channel', 'auth-1', 0, 123, 45, 10, 20, 0, 0, 30, 0)`,
		at.UTC().Format(time.RFC3339Nano), apiKey, apiKeyID, apiKeyName,
	); err != nil {
		t.Fatalf("insert raw request_log: %v", err)
	}
}
