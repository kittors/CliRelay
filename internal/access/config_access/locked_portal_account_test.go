package configaccess

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/enduser"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	_ "modernc.org/sqlite"
)

// Locking an end user out of the portal — must_change_password set and a
// password nobody knows, the state the API key backfill creates accounts in and
// the legacy backfill password lock moves accounts to — is only harmless because
// key admission never reads either field: it admits a key by the key's own
// disabled flag and its owner's status. This pins that. If admission ever starts
// consulting portal credentials, those locks become outages.
func TestOwnedKeyAuthenticatesWhileItsAccountCannotSignIn(t *testing.T) {
	if err := usage.InitDB(filepath.Join(t.TempDir(), "usage.db"), config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(usage.CloseDB)
	db := usage.RuntimeDB()
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS end_users (
			id TEXT PRIMARY KEY,
			tenant_id TEXT NOT NULL,
			username TEXT NOT NULL,
			username_normalized TEXT NOT NULL UNIQUE,
			display_name TEXT NOT NULL,
			password_hash TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'active',
			must_change_password INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL DEFAULT '',
			version INTEGER NOT NULL DEFAULT 1
		)
	`); err != nil {
		t.Fatalf("create end_users: %v", err)
	}
	if err := usage.EnsureEndUserQuotaColumns(db); err != nil {
		t.Fatalf("EnsureEndUserQuotaColumns: %v", err)
	}

	tenantID := uuid.NewString()
	userID := uuid.NewString()
	if _, err := db.Exec(`
		INSERT INTO end_users (id, tenant_id, username, username_normalized, display_name, password_hash, status, must_change_password)
		VALUES (?, ?, 'locked_user', 'locked_user', 'Locked User', '!unusable', 'active', 1)
	`, userID, tenantID); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	key, err := enduser.NewService(db).CreateKey(context.Background(), tenantID, userID, "prod")
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	authenticate := func() (string, bool) {
		p := newProvider("test", buildKeyConfigMap(&config.SDKConfig{}))
		req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		req.Header.Set("Authorization", "Bearer "+key.PlaintextKey)
		res, authErr := p.Authenticate(context.Background(), req)
		if authErr != nil {
			return "", false
		}
		return res.Metadata["end-user-id"], true
	}

	if owner, ok := authenticate(); !ok || owner != userID {
		t.Fatalf("key owned by a portal-locked account: admitted=%v owner=%q, want admitted for %q", ok, owner, userID)
	}

	// Control: admission does read the owning account — disabling the account
	// itself turns the key away — so the pass above is not vacuous.
	if _, err := db.Exec(`UPDATE end_users SET status = 'disabled' WHERE id = ?`, userID); err != nil {
		t.Fatalf("disable account: %v", err)
	}
	if _, ok := authenticate(); ok {
		t.Fatal("key of a disabled account was admitted; the check above proves nothing")
	}
}
