package usage

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

// Rows written before the observation guard was tightened hold whatever
// User-Agent the calling SDK sent, and replaying one of those upstream costs the
// account every request ("no valid license", #3501). Startup has to clear them;
// a genuine client re-learns its own identity on the next request.
func TestPurgeForeignAntigravityFingerprints(t *testing.T) {
	initTestUsageDB(t, config.RequestLogStorageConfig{})

	seed := func(provider, accountKey, fieldsJSON string) {
		t.Helper()
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := getDB().Exec(`
			INSERT INTO identity_fingerprints (
				tenant_id, provider, account_key, profile_key, auth_subject_id,
				client_product, client_variant, version, fields_json, observed_headers_json,
				created_at, updated_at, last_seen_at
			) VALUES (?, ?, ?, 'default', ?, '', '', '', ?, '{}', ?, ?, ?)
		`, systemTenantID, provider, accountKey, accountKey, fieldsJSON, now, now, now); err != nil {
			t.Fatal(err)
		}
	}

	seed("antigravity", "acct-node", `{"user-agent":"node"}`)
	seed("antigravity", "acct-curl", `{"user-agent":"curl/8.7.1"}`)
	seed("antigravity", "acct-empty", `{}`)
	seed("antigravity", "acct-corrupt", `not json`)
	seed("antigravity", "acct-real", `{"user-agent":"vscode/1.96.0 (Antigravity/4.3.0)","antigravity-version":"4.3.0"}`)
	// A foreign User-Agent on another provider is that provider's business.
	seed("codex", "acct-codex", `{"user-agent":"node"}`)

	purgeForeignAntigravityFingerprints(getDB())

	survivors := map[string]bool{}
	rows, err := getDB().Query(`SELECT provider, account_key FROM identity_fingerprints WHERE tenant_id = ?`, systemTenantID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var provider, accountKey string
		if err = rows.Scan(&provider, &accountKey); err != nil {
			t.Fatal(err)
		}
		survivors[provider+"/"+accountKey] = true
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}

	for _, purged := range []string{
		"antigravity/acct-node",
		"antigravity/acct-curl",
		"antigravity/acct-empty",
		"antigravity/acct-corrupt",
	} {
		if survivors[purged] {
			t.Errorf("%s survived, want it purged", purged)
		}
	}
	for _, kept := range []string{"antigravity/acct-real", "codex/acct-codex"} {
		if !survivors[kept] {
			t.Errorf("%s was purged, want it kept", kept)
		}
	}
}
