package middleware

import (
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/accesspolicy"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota/inflight"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/postgrestest"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// These tests force a database read to return its pre-commit value after the
// writer has committed and the real settled hook has released the first hold.
// The second admission must still include the hold captured before that read.
func TestPostgresQuotaSnapshotCannotMissConcurrentSettlement(t *testing.T) {
	tenant, keyID, key, model := initOrderingPostgres(t)
	policy := &accesspolicy.Policy{TenantID: tenant, APIKeyID: keyID,
		KeyLimits: matrixWith(quota.MetricRequests, quota.PeriodWeek, 1)}
	installPolicy(t, policy, nil, nil, quota.Matrix{}, quota.Matrix{})
	accesspolicy.Publish(map[string]*accesspolicy.Policy{key: policy})
	queryKeySnapshotFunc = usage.QueryAPIKeyQuotaUsageSnapshot
	b := newBurst(t, map[string]map[string]string{key: {"tenant-id": tenant, "api-key-id": keyID}})
	first := b.hold(key, model, true)
	first.finish(t)
	owner := inflight.KeyName(keyID)
	if got := quotaLedger.Held(owner).Requests; got != 1 {
		t.Fatalf("setup: held requests = %v, want 1", got)
	}
	commit := orderingPostgresCommit(t, tenant, keyID, key, model, first.requestID)
	var reads int
	queryKeySnapshotFunc = func(tenantID, apiKeyID string) (quota.UsageSnapshot, error) {
		old, err := usage.QueryAPIKeyQuotaUsageSnapshot(tenantID, apiKeyID)
		if err != nil {
			return old, err
		}
		reads++
		if old.Total.Requests.Week != 0 {
			t.Fatalf("pre-commit requests = %v, want 0", old.Total.Requests.Week)
		}
		commit()
		if got := quotaLedger.Held(owner).Requests; got != 1 {
			t.Fatalf("settled held requests = %v, want only the second reservation", got)
		}
		return old, nil
	}
	assertRefused(t, b.send(key, model), "1", "request limit")
	if reads != 1 {
		t.Fatalf("snapshot reads = %d, want 1", reads)
	}
	persisted, err := usage.QueryAPIKeyQuotaUsageSnapshot(tenant, keyID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Total.Requests.Week != 1 {
		t.Fatalf("persisted requests = %v, want 1", persisted.Total.Requests.Week)
	}
}

func TestPostgresPeriodSpendingCannotMissConcurrentSettlement(t *testing.T) {
	tenant, keyID, key, model := initOrderingPostgres(t)
	resetQuotaMiddlewareState(t)
	queryPeriodByKeyFunc = usage.QueryPeriodSpendingByAPIKeyIDForTenant
	b := newBurst(t, map[string]map[string]string{key: {
		"tenant-id": tenant, "api-key-id": keyID, "key-period-spending-limit-day": "1",
	}})
	owner := inflight.KeyName(keyID)
	teach(inflight.Owner{Key: owner}, 1, 100)
	first := b.hold(key, model, true)
	first.finish(t)
	if got := quotaLedger.Held(owner).Cost; got != 1 {
		t.Fatalf("setup: held cost = %v, want 1", got)
	}
	commit := orderingPostgresCommit(t, tenant, keyID, key, model, first.requestID)
	var reads int
	queryPeriodByKeyFunc = func(tenantID, apiKeyID string) (quota.PeriodSpendingUsage, error) {
		old, err := usage.QueryPeriodSpendingByAPIKeyIDForTenant(tenantID, apiKeyID)
		if err != nil {
			return old, err
		}
		reads++
		if old.Day != 0 {
			t.Fatalf("pre-commit cost = %v, want 0", old.Day)
		}
		commit()
		if got := quotaLedger.Held(owner).Cost; got != 1 {
			t.Fatalf("settled held cost = %v, want only the second reservation", got)
		}
		return old, nil
	}
	assertRefused(t, b.send(key, model), "1", "spending limit")
	if reads != 1 {
		t.Fatalf("period reads = %d, want 1", reads)
	}
	persisted, err := usage.QueryPeriodSpendingByAPIKeyIDForTenant(tenant, keyID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Day != 1 {
		t.Fatalf("persisted day cost = %v, want 1", persisted.Day)
	}
}

func initOrderingPostgres(t *testing.T) (tenant, keyID, key, model string) {
	t.Helper()
	dsn := os.Getenv("CLIRELAY_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	postgrestest.LockSharedRuntimeDB(t, dsn)
	usage.CloseDB()
	t.Cleanup(usage.CloseDB)
	if err := usage.InitPostgres(config.PostgresConfig{DSN: dsn, MaxOpenConns: 4, MaxIdleConns: 1}, config.RequestLogStorageConfig{}, time.UTC); err != nil {
		t.Fatalf("InitPostgres: %v", err)
	}
	// Unique identities avoid depending on, or truncating, another test's rows.
	tenant = "00000000-0000-0000-0000-000000000001"
	keyID = uuid.NewString()
	key = "sk-ordering-" + keyID
	model = "ordering-" + keyID
	now := time.Now().UTC().Format(time.RFC3339)
	if err := usage.UpsertAPIKey(usage.APIKeyRow{ID: keyID, Key: key, TenantID: tenant, Name: "quota ordering test", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := usage.UpsertModelConfig(usage.ModelConfigRow{ModelID: model, Enabled: true, PricingMode: "token", InputPricePerMillion: 10000}); err != nil {
		t.Fatal(err)
	}
	return
}

func orderingPostgresCommit(t *testing.T, tenant, keyID, key, model, requestID string) func() {
	t.Helper()
	var settled atomic.Int64
	usage.SetRequestLogSettledHook(func(id string, billed bool, cost float64, tokens int64) {
		if id != requestID || !billed || cost != 1 || tokens != 100 {
			t.Errorf("settled = (%q, %v, %v, %d), want (%q, true, 1, 100)", id, billed, cost, tokens, requestID)
		}
		quotaLedger.Settle(id, billed, cost, tokens)
		settled.Add(1)
	})
	t.Cleanup(func() { usage.SetRequestLogSettledHook(nil) })
	return func() {
		t.Helper()
		usage.InsertRequestLog(usage.RequestLogEntry{
			RequestID: requestID, IdempotencyKey: requestID,
			TrustedTenantID: tenant, APIKeyID: keyID, APIKey: key, Model: model,
			Timestamp: time.Now(), Tokens: usage.TokenStats{InputTokens: 100, TotalTokens: 100},
		})
		if got := settled.Load(); got != 1 {
			t.Fatalf("settled callbacks = %d, want 1", got)
		}
		var count int
		if err := usage.RuntimeDB().QueryRow(`SELECT COUNT(*) FROM request_logs WHERE api_key_id = ?`, keyID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("persisted first request rows = %d, want 1", count)
		}
	}
}
