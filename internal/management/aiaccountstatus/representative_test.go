package aiaccountstatus

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	managementapitools "github.com/router-for-me/CLIProxyAPI/v6/internal/management/apitools"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestPreferAuthRepresentativeIsDeterministicAndPrefersHealthyFresherCredential(t *testing.T) {
	now := time.Now().UTC()
	staleGo := &coreauth.Auth{
		ID:              "tenant-1/codex-rojas-go.json",
		TenantID:        "tenant-1",
		Provider:        "codex",
		FileName:        "codex-rojas-go.json",
		Status:          coreauth.StatusError,
		StatusMessage:   "re-login required: token_revoked",
		LastRefreshedAt: now.Add(-10 * 24 * time.Hour),
		Metadata: map[string]any{
			"email":           "rojas@example.com",
			"account_id":      "acct-rojas",
			"chatgpt_user_id": "user-rojas",
			"last_refresh":    now.Add(-10 * 24 * time.Hour).Format(time.RFC3339),
			coreauth.CodexOAuthHealthMetadataKey: map[string]any{
				"status":        "reauth_required",
				"reauth_reason": "token_revoked",
			},
		},
	}
	healthyPro := &coreauth.Auth{
		ID:              "tenant-1/codex-rojas-pro.json",
		TenantID:        "tenant-1",
		Provider:        "codex",
		FileName:        "codex-rojas-pro.json",
		Status:          coreauth.StatusActive,
		LastRefreshedAt: now.Add(-1 * time.Minute),
		Metadata: map[string]any{
			"email":           "rojas@example.com",
			"account_id":      "acct-rojas",
			"chatgpt_user_id": "user-rojas",
			"last_refresh":    now.Add(-1 * time.Minute).Format(time.RFC3339),
		},
	}

	if got := preferAuthRepresentative(staleGo, healthyPro); got != healthyPro {
		t.Fatalf("prefer(stale, healthy) = %s, want %s", got.ID, healthyPro.ID)
	}
	if got := preferAuthRepresentative(healthyPro, staleGo); got != healthyPro {
		t.Fatalf("prefer(healthy, stale) = %s, want %s", got.ID, healthyPro.ID)
	}

	// Between two healthy aliases, the fresher last_refresh must win symmetrically.
	olderHealthy := healthyPro.Clone()
	olderHealthy.ID = "tenant-1/codex-rojas-a.json"
	olderHealthy.LastRefreshedAt = now.Add(-3 * time.Hour)
	olderHealthy.Metadata["last_refresh"] = now.Add(-3 * time.Hour).Format(time.RFC3339)

	if got := preferAuthRepresentative(olderHealthy, healthyPro); got != healthyPro {
		t.Fatalf("prefer(olderHealthy, fresherHealthy) = %s, want %s", got.ID, healthyPro.ID)
	}
	if got := preferAuthRepresentative(healthyPro, olderHealthy); got != healthyPro {
		t.Fatalf("prefer(fresherHealthy, olderHealthy) = %s, want %s", got.ID, healthyPro.ID)
	}

	// An alias with an active 401 cooldown loses to a healthy alias even without codex_oauth_health.
	cooled401 := healthyPro.Clone()
	cooled401.ID = "tenant-1/codex-rojas-000.json"
	cooled401.ModelStates = map[string]*coreauth.ModelState{
		"gpt-5-codex": {
			Unavailable:    true,
			Status:         coreauth.StatusError,
			NextRetryAfter: now.Add(20 * time.Minute),
			LastError:      &coreauth.Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"},
		},
	}
	if got := preferAuthRepresentative(cooled401, healthyPro); got != healthyPro {
		t.Fatalf("prefer(cooled401, healthy) = %s, want %s", got.ID, healthyPro.ID)
	}
	if got := preferAuthRepresentative(healthyPro, cooled401); got != healthyPro {
		t.Fatalf("prefer(healthy, cooled401) = %s, want %s", got.ID, healthyPro.ID)
	}
}

func TestStartRefreshAndListStatusAlwaysPickHealthyAliasOverRevokedDuplicate(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	if err := usage.InitDB(dbPath, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { usage.CloseDB(); _ = os.Remove(dbPath) })

	now := time.Now().UTC()
	revokedGo := &coreauth.Auth{
		ID:              "tenant-1/codex-rojas-go.json",
		TenantID:        "tenant-1",
		Provider:        "codex",
		FileName:        "codex-rojas-go.json",
		Status:          coreauth.StatusError,
		StatusMessage:   "re-login required: token_revoked",
		LastRefreshedAt: now.Add(-10 * 24 * time.Hour),
		Metadata: map[string]any{
			"email":           "rojas@example.com",
			"account_id":      "acct-rojas",
			"chatgpt_user_id": "user-rojas",
			coreauth.CodexOAuthHealthMetadataKey: map[string]any{
				"status":        "reauth_required",
				"reauth_reason": "token_revoked",
			},
		},
	}
	healthyPro := &coreauth.Auth{
		ID:              "tenant-1/codex-rojas-pro.json",
		TenantID:        "tenant-1",
		Provider:        "codex",
		FileName:        "codex-rojas-pro.json",
		Status:          coreauth.StatusActive,
		LastRefreshedAt: now,
		Metadata: map[string]any{
			"email":           "rojas@example.com",
			"account_id":      "acct-rojas",
			"chatgpt_user_id": "user-rojas",
		},
	}
	manager := newTestManager(t, "tenant-1", revokedGo, healthyPro)
	revokedIndex := revokedGo.EnsureIndex()
	healthyIndex := healthyPro.EnsureIndex()

	var (
		mu       sync.Mutex
		probedID []string
	)
	svc := New(&config.Config{}, manager, func(tenantID string) *managementapitools.Service {
		return managementapitools.NewForTenant(tenantID, &config.Config{}, manager, managementapitools.Dependencies{})
	}, nil)
	svc.SetProbeFunc(func(_ context.Context, _ *managementapitools.Service, _ *config.Config, auth *coreauth.Auth) (ProbeResult, error) {
		mu.Lock()
		probedID = append(probedID, auth.ID)
		mu.Unlock()
		if auth.ID == revokedGo.ID {
			return ProbeResult{}, context.DeadlineExceeded
		}
		pct := 82.0
		return ProbeResult{PlanType: "pro", Quotas: []usage.QuotaWindowDTO{{QuotaKey: "code_week", Percent: &pct}}}, nil
	})

	// Run multiple rounds of full refresh and targeted refresh (including targeting
	// the stale alias index directly) to prove map randomization never picks revokedGo.
	for i := 0; i < 6; i++ {
		req := RefreshRequest{Force: true}
		if i%2 == 1 {
			req.AuthIndexes = []string{revokedIndex}
		}
		accepted := svc.StartRefresh("tenant-1", req)
		if accepted.Accepted != 1 {
			t.Fatalf("round %d: accepted=%d, want 1", i, accepted.Accepted)
		}
		snap := waitJob(t, svc, "tenant-1", accepted.JobID)
		if snap.Failed != 0 || snap.Completed != 1 {
			t.Fatalf("round %d: expected probe success through healthy alias, got %+v", i, snap)
		}
	}

	mu.Lock()
	for i, id := range probedID {
		if id != healthyPro.ID {
			t.Fatalf("probe #%d used %s, want %s", i, id, healthyPro.ID)
		}
	}
	mu.Unlock()

	listResp, err := svc.ListStatus("tenant-1", nil, nil)
	if err != nil {
		t.Fatalf("ListStatus: %v", err)
	}
	if len(listResp.Items) != 1 {
		t.Fatalf("ListStatus items = %d, want 1", len(listResp.Items))
	}
	if listResp.Items[0].AuthIndex != healthyIndex || listResp.Items[0].PlanType != "pro" {
		t.Fatalf("ListStatus picked wrong representative: %+v", listResp.Items[0])
	}
}
