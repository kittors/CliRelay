package aiaccountstatus

import (
	"context"
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

// One upstream account mounted by two tenants must read the same in both. It did
// not: every seed but account_id folded tenant_id in, so a Google account became
// one subject per tenant. The tenant that had not called it yet showed 0 calls
// beside the whole account's quota, and the operator read that as "this tenant's
// data has not synced".
func TestListStatusReportsOneAccountIdenticallyInEveryTenant(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { usage.CloseDB(); _ = os.Remove(dbPath) })

	const tenantBusy, tenantIdle = "tenant-busy", "tenant-idle"
	busyAuth := &coreauth.Auth{
		ID: tenantBusy + "/antigravity-shared@example.com.json", Provider: "antigravity",
		FileName: "antigravity-busy.json", Metadata: map[string]any{"email": "shared@example.com"},
	}
	idleAuth := &coreauth.Auth{
		ID: tenantIdle + "/antigravity-shared@example.com.json", Provider: "antigravity",
		FileName: "antigravity-idle.json", Metadata: map[string]any{"email": "shared@example.com"},
	}
	busyManager := newTestManager(t, tenantBusy, busyAuth)
	idleManager := newTestManager(t, tenantIdle, idleAuth)

	identity := usage.ResolveAuthSubjectIdentity(busyAuth)
	if identity == nil || identity.ID != usage.ResolveAuthSubjectIdentity(idleAuth).ID {
		t.Fatalf("one account still resolves to two subjects: %+v", identity)
	}
	hook := usage.AIAccountBindingHook{}
	hook.OnAuthLoaded(context.Background(), busyAuth)
	hook.OnAuthLoaded(context.Background(), idleAuth)

	checked := time.Now().UTC().Truncate(time.Second)
	percent := 87.0
	if err := usage.UpsertAIAccountSubjectStatus(usage.AIAccountSubjectStatusRecord{
		AuthSubjectID: identity.ID, Provider: "antigravity",
		LastProbeState: string(RefreshSuccess), HealthStatus: "ok", PlanType: "google-ai-pro",
		Quotas: []usage.QuotaWindowDTO{{
			QuotaKey: "antigravity:gemini_weekly", Percent: &percent, WindowSeconds: 604800,
		}},
		UpstreamCheckedAt: &checked, UpdatedAt: checked,
	}); err != nil {
		t.Fatal(err)
	}
	// Only the busy tenant ever called the account; the subject projection is
	// shared, so the idle tenant must still read the account's totals.
	admin, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.Exec(`
		INSERT INTO ai_account_subject_usage_buckets
			(auth_subject_id, bucket_kind, bucket_start, request_count, success_count,
			 failure_count, cost_total, total_tokens, first_event_at, updated_at)
		VALUES (?, 'lifetime', '1970-01-01', 9, 9, 0, 3, 1234, ?, ?)
	`, identity.ID, checked.Format(time.RFC3339Nano), checked.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	for _, view := range []struct {
		tenant  string
		manager *coreauth.Manager
	}{{tenantBusy, busyManager}, {tenantIdle, idleManager}} {
		response, err := New(&config.Config{}, view.manager, nil, nil).ListStatus(view.tenant, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Items) != 1 {
			t.Fatalf("%s items=%+v", view.tenant, response.Items)
		}
		item := response.Items[0]
		if item.AuthSubjectID != identity.ID {
			t.Fatalf("%s resolved subject %q, want %q", view.tenant, item.AuthSubjectID, identity.ID)
		}
		if item.Usage.RequestTotal != 9 || item.Usage.SuccessTotal != 9 {
			t.Fatalf("%s usage = %+v, want the account total", view.tenant, item.Usage)
		}
		if item.PlanType != "google-ai-pro" {
			t.Fatalf("%s plan = %q", view.tenant, item.PlanType)
		}
	}
}
