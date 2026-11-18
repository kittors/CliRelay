package codex

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"

	internalcodex "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
)

func TestRecordFromTokenStorageUsesPlanAndAccountHashFromIDToken(t *testing.T) {
	idToken := unsignedJWT(t, map[string]any{
		"email": "claims@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-team",
			"chatgpt_plan_type":  " team ",
			"chatgpt_user_id":    "user-team-member",
		},
	})
	storage := &internalcodex.CodexTokenStorage{
		IDToken:   idToken,
		Email:     "codex@example.com",
		AccountID: "acct-storage",
	}

	record := RecordFromTokenStorage(storage)
	if record == nil {
		t.Fatal("RecordFromTokenStorage() = nil")
	}
	wantHash := shortHash("acct-team")
	wantFileName := fmt.Sprintf("codex-%s-codex@example.com-team.json", wantHash)
	if record.ID != wantFileName || record.FileName != wantFileName {
		t.Fatalf("ID/FileName = %q/%q, want %q", record.ID, record.FileName, wantFileName)
	}
	if record.Provider != "codex" || record.Storage != storage {
		t.Fatalf("provider/storage = %q/%#v, want codex/original storage", record.Provider, record.Storage)
	}
	if got, _ := record.Metadata["email"].(string); got != "codex@example.com" {
		t.Fatalf("metadata[email] = %q, want codex@example.com", got)
	}
	if got, _ := record.Metadata["account_id"].(string); got != "acct-storage" {
		t.Fatalf("metadata[account_id] = %q, want acct-storage", got)
	}
	if got, _ := record.Metadata["plan_type"].(string); got != "team" {
		t.Fatalf("metadata[plan_type] = %q, want team", got)
	}
	if got, _ := record.Metadata["chatgpt_user_id"].(string); got != "user-team-member" {
		t.Fatalf("metadata[chatgpt_user_id] = %q, want user-team-member", got)
	}
}

func TestRecordFromTokenStorageFallsBackWhenIDTokenCannotBeParsed(t *testing.T) {
	storage := &internalcodex.CodexTokenStorage{
		IDToken:   "not-a-jwt",
		Email:     "codex@example.com",
		AccountID: "acct-storage",
	}

	record := RecordFromTokenStorage(storage)
	if record == nil {
		t.Fatal("RecordFromTokenStorage() = nil")
	}
	if record.FileName != "codex-codex@example.com.json" {
		t.Fatalf("FileName = %q, want fallback filename", record.FileName)
	}
	if got, _ := record.Metadata["plan_type"].(string); got != "" {
		t.Fatalf("metadata[plan_type] = %q, want empty", got)
	}
}

func TestRecordFromTokenStorageHandlesNilStorage(t *testing.T) {
	if record := RecordFromTokenStorage(nil); record != nil {
		t.Fatalf("RecordFromTokenStorage(nil) = %#v, want nil", record)
	}
}

func unsignedJWT(t *testing.T, claims map[string]any) string {
	t.Helper()

	header := map[string]any{"alg": "none"}
	return encodeJWTPart(t, header) + "." + encodeJWTPart(t, claims) + "."
}

func encodeJWTPart(t *testing.T, value any) string {
	t.Helper()

	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal jwt part: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

func shortHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])[:8]
}
