package aiaccountstatus

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	_ "modernc.org/sqlite"
)

// A tenant's accounts are resolved in one batch, and the batch used to carry a
// single list of preferred quota keys assembled from every provider in it. Any
// provider whose windows were not on that list — antigravity names its buckets
// after whatever the upstream calls them — had all of its cycles filtered out,
// so its cards reported no usage for the period while the other providers' cards
// were fine.
func TestListStatusKeepsCycleUsageForMixedProviders(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { usage.CloseDB(); _ = os.Remove(dbPath) })

	const tenant = "tenant-mixed"
	codexAuth := &coreauth.Auth{
		ID: "codex-1", Provider: "codex", FileName: "codex-1.json",
		Metadata: map[string]any{"account_id": "codex-account"},
	}
	antigravityAuth := &coreauth.Auth{
		ID: "antigravity-1", Provider: "antigravity", FileName: "antigravity-1.json",
		Metadata: map[string]any{"account_id": "antigravity-account"},
	}
	manager := newTestManager(t, tenant, codexAuth, antigravityAuth)

	admin, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()

	checked := time.Now().UTC().Truncate(time.Second)
	resetAt := checked.Add(72 * time.Hour)
	cycleStart := resetAt.Add(-7 * 24 * time.Hour)
	percent := 61.0

	accounts := []struct {
		auth     *coreauth.Auth
		provider string
		quotaKey string
		requests int64
		tokens   int64
	}{
		{codexAuth, "codex", "code_week", 4, 4000},
		{antigravityAuth, "antigravity", "antigravity:gemini_weekly", 7, 7000},
	}
	subjects := make(map[string]string, len(accounts))
	for _, account := range accounts {
		identity := usage.ResolveAuthSubjectIdentity(account.auth)
		if identity == nil {
			t.Fatalf("no identity for %s", account.auth.ID)
		}
		subjects[account.auth.ID] = identity.ID
		if err := usage.UpsertAIAccountTenantBinding(account.auth, identity); err != nil {
			t.Fatal(err)
		}
		if err := usage.UpsertAIAccountSubjectStatus(usage.AIAccountSubjectStatusRecord{
			AuthSubjectID: identity.ID, Provider: account.provider,
			LastProbeState: string(RefreshSuccess), HealthStatus: "ok",
			Quotas:            []usage.QuotaWindowDTO{{QuotaKey: account.quotaKey, Percent: &percent}},
			UpstreamCheckedAt: &checked, UpdatedAt: checked,
		}); err != nil {
			t.Fatal(err)
		}
		reset := resetAt
		if err := usage.RecordAIAccountSubjectQuotaPoints(identity.ID, account.provider, []usage.QuotaSnapshotPoint{{
			RecordedAt: checked, Provider: account.provider, QuotaKey: account.quotaKey,
			QuotaLabel: account.quotaKey, Percent: &percent, ResetAt: &reset, WindowSeconds: 604800,
		}}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(`
			INSERT INTO ai_account_subject_usage_buckets
				(auth_subject_id, bucket_kind, bucket_start, request_count, success_count,
				 failure_count, cost_total, total_tokens, first_event_at, updated_at)
			VALUES (?, 'cycle', ?, ?, ?, 0, 0, ?, ?, ?)
		`, identity.ID, cycleStart.Format(time.RFC3339Nano), account.requests, account.requests,
			account.tokens, cycleStart.Format(time.RFC3339Nano), checked.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}

	response, listErr := New(&config.Config{}, manager, nil, nil).ListStatus(tenant, nil, nil)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(response.Items) != len(accounts) {
		t.Fatalf("items=%d, want %d", len(response.Items), len(accounts))
	}
	bySubject := make(map[string]AccountStatusView, len(response.Items))
	for _, item := range response.Items {
		bySubject[item.AuthSubjectID] = item
	}
	for _, account := range accounts {
		item, ok := bySubject[subjects[account.auth.ID]]
		if !ok {
			t.Fatalf("%s missing from status list", account.provider)
		}
		if !item.Usage.CycleKnown || item.Usage.CycleRequestTotal != account.requests {
			t.Fatalf("%s cycle usage = %+v, want %d requests",
				account.provider, item.Usage, account.requests)
		}
	}
}
