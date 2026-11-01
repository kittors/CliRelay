package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/alerting"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/identity"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// stubSources answers the alert sources from a fixed list.
type stubSources struct {
	accounts []alerting.AccountSample
	err      error
}

func (s stubSources) Accounts(context.Context, string, time.Time) ([]alerting.AccountSample, error) {
	return s.accounts, s.err
}

func (s stubSources) Spenders(context.Context, string, time.Time) ([]alerting.SpendSample, error) {
	return nil, nil
}

func poolAccount(id, provider string, used float64, resetIn time.Duration, now time.Time) alerting.AccountSample {
	return alerting.AccountSample{
		ID: id, Subject: "subject-" + id, Provider: provider, Name: id,
		Windows: []alerting.WindowSample{{
			Key: "code_week", Label: "Weekly", Span: 7 * 24 * time.Hour, UsedPercent: used,
			ResetAt: now.Add(resetIn), ObservedAt: now.Add(-10 * time.Minute), AccountWide: true,
		}},
	}
}

func TestComputePoolCapacityBuildsTheContract(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	sources := stubSources{accounts: []alerting.AccountSample{
		poolAccount("a", "codex", 60, 48*time.Hour, now),
		poolAccount("b", "codex", 64, 72*time.Hour, now),
	}}
	var asked struct {
		subjects, windows []string
		since             time.Time
	}
	readings := func(subjects, windows []string, since time.Time) ([]usage.AIAccountQuotaReading, error) {
		asked.subjects, asked.windows, asked.since = subjects, windows, since
		var out []usage.AIAccountQuotaReading
		for _, subject := range subjects {
			// 4 points an hour down to what the sample says is left (40 and 36), plus a
			// reading with no percentage that must be ignored.
			left := map[string]float64{"subject-a": 40, "subject-b": 36}[subject]
			for _, step := range []struct {
				ago  time.Duration
				plus float64
			}{{4 * time.Hour, 16}, {2 * time.Hour, 8}, {0, 0}} {
				out = append(out, usage.AIAccountQuotaReading{
					AuthSubjectID: subject, QuotaKey: "code_week", RemainingPercent: ptr(left + step.plus),
					ResetAt: now.Add(48 * time.Hour), RecordedAt: now.Add(-step.ago),
				})
			}
			out = append(out, usage.AIAccountQuotaReading{AuthSubjectID: subject, QuotaKey: "code_week", RecordedAt: now.Add(-time.Hour)})
		}
		return out, nil
	}

	resp, err := computePoolCapacity(context.Background(), sources, readings, "tenant-1", now)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.GeneratedAt.Equal(now) {
		t.Fatalf("generated at %v, want %v", resp.GeneratedAt, now)
	}
	if got := now.Add(-alerting.PoolLookback); !asked.since.Equal(got) {
		t.Fatalf("history asked since %v, want the six-hour lookback %v", asked.since, got)
	}
	if len(asked.subjects) != 2 || len(asked.windows) != 1 || asked.windows[0] != "code_week" {
		t.Fatalf("history scope = %v / %v, want both accounts and the one window", asked.subjects, asked.windows)
	}
	if len(resp.Pools) != 1 {
		t.Fatalf("pools = %+v", resp.Pools)
	}
	p := resp.Pools[0]
	if p.Provider != "codex" || p.WindowKey != "code_week" || p.WindowLabel != "Weekly" || p.WindowSeconds != 604800 {
		t.Fatalf("pool identity = %+v", p)
	}
	if p.Accounts != 2 || p.AccountsWithData != 2 || p.RemainingPercent != 38 {
		t.Fatalf("pool = %+v, want 2/2 accounts at 38%% (the mean of 40 and 36)", p)
	}
	if p.BurnPercentPerHour == nil || *p.BurnPercentPerHour != 4 {
		t.Fatalf("burn = %v, want 4 points an hour", p.BurnPercentPerHour)
	}
	if p.EarliestResetAt == nil || !p.EarliestResetAt.Equal(now.Add(48*time.Hour)) {
		t.Fatalf("earliest reset = %v", p.EarliestResetAt)
	}
	want := now.Add(time.Duration(9.5 * float64(time.Hour)))
	if p.ProjectedExhaustionAt == nil || p.ProjectedExhaustionAt.Sub(want).Abs() > time.Minute {
		t.Fatalf("projected exhaustion = %v, want about %v (38 / 4 hours)", p.ProjectedExhaustionAt, want)
	}
	if p.ObservedAt == nil || !p.ObservedAt.Equal(now.Add(-10*time.Minute)) {
		t.Fatalf("observed at = %v", p.ObservedAt)
	}
}

func TestPoolCapacityJSONLeavesOutWhatIsNotKnown(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	// One account, no history, no announced reset.
	account := poolAccount("a", "codex", 50, 0, now)
	account.Windows[0].ResetAt = time.Time{}
	account.Windows[0].ObservedAt = time.Time{}
	resp, err := computePoolCapacity(context.Background(), stubSources{accounts: []alerting.AccountSample{account}},
		func(_, _ []string, _ time.Time) ([]usage.AIAccountQuotaReading, error) { return nil, nil }, "t", now)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Pools []map[string]any `json:"pools"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil || len(decoded.Pools) != 1 {
		t.Fatalf("body = %s (%v)", raw, err)
	}
	pool := decoded.Pools[0]
	for _, absent := range []string{"earliest_reset_at", "burn_percent_per_hour", "projected_exhaustion_at", "observed_at"} {
		// The panel reads "absent" as "not estimated", and 0 as a measurement.
		if _, present := pool[absent]; present {
			t.Errorf("%s is present (%v), want it left out", absent, pool[absent])
		}
	}
	for _, present := range []string{"provider", "window_key", "window_label", "window_seconds", "accounts", "accounts_with_data", "remaining_percent"} {
		if _, ok := pool[present]; !ok {
			t.Errorf("%s is missing from %s", present, raw)
		}
	}
}

func TestPoolCapacityIsAnEmptyListNotNull(t *testing.T) {
	resp, err := computePoolCapacity(context.Background(), stubSources{}, func(_, _ []string, _ time.Time) ([]usage.AIAccountQuotaReading, error) {
		t.Fatal("with no accounts there is nothing to ask the history about")
		return nil, nil
	}, "t", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(resp)
	if !strings.Contains(string(raw), `"pools":[]`) {
		t.Fatalf("body = %s, want \"pools\":[]", raw)
	}
}

func TestPoolCapacityPropagatesFailures(t *testing.T) {
	now := time.Now()
	boom := errors.New("database is down")
	if _, err := computePoolCapacity(context.Background(), stubSources{err: boom}, nil, "t", now); !errors.Is(err, boom) {
		t.Fatalf("sources error = %v, want it returned", err)
	}
	failing := func(_, _ []string, _ time.Time) ([]usage.AIAccountQuotaReading, error) { return nil, boom }
	sources := stubSources{accounts: []alerting.AccountSample{poolAccount("a", "codex", 10, time.Hour, now)}}
	if _, err := computePoolCapacity(context.Background(), sources, failing, "t", now); !errors.Is(err, boom) {
		t.Fatalf("history error = %v, want it returned", err)
	}
}

func okResponse(n int) poolCapacityResponse {
	return poolCapacityResponse{GeneratedAt: time.Unix(int64(n), 0).UTC(), Pools: []poolCapacityPool{}}
}

func TestPoolCapacityCacheReusesAnAnswerForAShortTime(t *testing.T) {
	var cache poolCapacityCache
	var calls atomic.Int32
	compute := func(context.Context, time.Time) (poolCapacityResponse, error) {
		return okResponse(int(calls.Add(1))), nil
	}
	now := time.Now()
	first, err := cache.get(context.Background(), "tenant-a", now, compute)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := cache.get(context.Background(), "tenant-a", now.Add(poolCapacityTTL-time.Second), compute)
	if calls.Load() != 1 || !again.GeneratedAt.Equal(first.GeneratedAt) {
		t.Fatalf("computed %d times, want the second call served from the cache", calls.Load())
	}
	if _, _ = cache.get(context.Background(), "tenant-a", now.Add(poolCapacityTTL), compute); calls.Load() != 2 {
		t.Fatalf("computed %d times, want a fresh answer once the TTL passed", calls.Load())
	}
	// Another tenant never gets this tenant's answer.
	if _, _ = cache.get(context.Background(), "tenant-b", now.Add(poolCapacityTTL), compute); calls.Load() != 3 {
		t.Fatalf("computed %d times, want a separate answer per tenant", calls.Load())
	}
	// A clock that stepped back does not make an answer from the future valid.
	if _, _ = cache.get(context.Background(), "tenant-a", now.Add(-time.Hour), compute); calls.Load() != 4 {
		t.Fatalf("computed %d times, want a recompute when the clock went backwards", calls.Load())
	}
}

func TestPoolCapacityCacheDoesNotKeepAFailure(t *testing.T) {
	var cache poolCapacityCache
	var calls atomic.Int32
	compute := func(context.Context, time.Time) (poolCapacityResponse, error) {
		if calls.Add(1) == 1 {
			return poolCapacityResponse{}, errors.New("transient")
		}
		return okResponse(2), nil
	}
	now := time.Now()
	if _, err := cache.get(context.Background(), "t", now, compute); err == nil {
		t.Fatal("the failure was swallowed")
	}
	if _, err := cache.get(context.Background(), "t", now, compute); err != nil || calls.Load() != 2 {
		t.Fatalf("err = %v after %d computations, want the retry to compute again and succeed", err, calls.Load())
	}
}

// singleflight re-raises a panic of the computation on a goroutine nothing can
// recover, so an unguarded bug there would end the process, not one request.
func TestPoolCapacityCacheSurvivesAPanickingComputation(t *testing.T) {
	var cache poolCapacityCache
	now := time.Now()
	_, err := cache.get(context.Background(), "t", now, func(context.Context, time.Time) (poolCapacityResponse, error) {
		var accounts []alerting.AccountSample
		_ = accounts[3] // an index out of range
		return poolCapacityResponse{}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("err = %v, want the panic reported as an error", err)
	}
	resp, err := cache.get(context.Background(), "t", now, func(context.Context, time.Time) (poolCapacityResponse, error) {
		return okResponse(5), nil
	})
	if err != nil || !resp.GeneratedAt.Equal(time.Unix(5, 0).UTC()) {
		t.Fatalf("after the panic: %+v %v, want the next request to work", resp, err)
	}
}

func TestPoolCapacityCacheSharesOneComputationBetweenConcurrentCallers(t *testing.T) {
	var cache poolCapacityCache
	var calls atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	compute := func(context.Context, time.Time) (poolCapacityResponse, error) {
		calls.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return okResponse(7), nil
	}
	now := time.Now()
	const callers = 8
	var wg sync.WaitGroup
	results := make([]poolCapacityResponse, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := cache.get(context.Background(), "t", now, compute)
			if err != nil {
				t.Errorf("caller %d: %v", i, err)
			}
			results[i] = resp
		}()
	}
	<-started
	time.Sleep(50 * time.Millisecond) // let the other callers reach the wait
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("computed %d times for %d concurrent callers, want 1", calls.Load(), callers)
	}
	for i, r := range results {
		if !r.GeneratedAt.Equal(time.Unix(7, 0).UTC()) {
			t.Fatalf("caller %d got %+v", i, r)
		}
	}
}

func TestPoolCapacityCacheLetsAWaiterGiveUpWithoutAbortingTheComputation(t *testing.T) {
	var cache poolCapacityCache
	release := make(chan struct{})
	started := make(chan struct{})
	computeCtxErr := make(chan error, 1)
	compute := func(ctx context.Context, _ time.Time) (poolCapacityResponse, error) {
		close(started)
		<-release
		computeCtxErr <- ctx.Err()
		return okResponse(9), nil
	}
	now := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	gone := make(chan error, 1)
	go func() {
		_, err := cache.get(ctx, "t", now, compute)
		gone <- err
	}()
	<-started
	cancel()
	if err := <-gone; !errors.Is(err, context.Canceled) {
		t.Fatalf("the caller that went away got %v, want context.Canceled", err)
	}
	close(release)
	// The computation finished for whoever asks next, undisturbed by the cancel.
	var resp poolCapacityResponse
	deadline := time.Now().Add(2 * time.Second)
	for {
		var err error
		resp, err = cache.get(context.Background(), "t", now, func(context.Context, time.Time) (poolCapacityResponse, error) {
			t.Error("the result of the first computation should have been cached")
			return okResponse(0), nil
		})
		if err == nil && resp.GeneratedAt.Equal(time.Unix(9, 0).UTC()) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the finished computation never became available: %+v %v", resp, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := <-computeCtxErr; err != nil {
		t.Fatalf("the computation saw its context end (%v) because one caller left", err)
	}
}

func TestPoolCapacityCacheStaysBounded(t *testing.T) {
	var cache poolCapacityCache
	now := time.Now()
	compute := func(context.Context, time.Time) (poolCapacityResponse, error) { return okResponse(1), nil }
	for i := 0; i < poolCapacityMaxTenants+50; i++ {
		// Each tenant asked a minute after the one before: everything older is stale.
		tenant := string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i/676))
		if _, err := cache.get(context.Background(), tenant, now.Add(time.Duration(i)*time.Minute), compute); err != nil {
			t.Fatal(err)
		}
	}
	cache.mu.Lock()
	size := len(cache.entries)
	cache.mu.Unlock()
	if size > poolCapacityMaxTenants+1 {
		t.Fatalf("the cache holds %d tenants, want it bounded near %d", size, poolCapacityMaxTenants)
	}
}

// The handler end to end, on a real database: accounts in the auth manager, the
// status the probes stored, and the readings they recorded.
func TestGetPoolCapacityOnStoredData(t *testing.T) {
	initAlertSourcesDB(t)
	const tenant = "tenant-pool"
	now := time.Now().UTC().Truncate(time.Second)
	reset := now.Add(48 * time.Hour)

	manager := coreauth.NewManager(nil, nil, nil)
	var subjects []string
	for _, id := range []string{"a", "b"} {
		auth := &coreauth.Auth{
			ID: "codex-" + id, TenantID: tenant, Provider: "codex", Label: id, Status: coreauth.StatusActive,
			Metadata: map[string]any{"account_id": "acct-" + id, "chatgpt_user_id": "user-" + id},
		}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
		subjects = append(subjects, usage.ResolveAuthSubjectIdentity(auth).ID)
	}
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: "codex-silent", TenantID: tenant, Provider: "codex", Label: "never probed", Status: coreauth.StatusActive,
		Metadata: map[string]any{"account_id": "acct-silent", "chatgpt_user_id": "user-silent"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: "codex-key", TenantID: tenant, Provider: "codex", Label: "a key", Status: coreauth.StatusActive,
		Attributes: map[string]string{"api_key": "sk-never-in-a-pool-response"},
	}); err != nil {
		t.Fatal(err)
	}

	left := map[string]float64{subjects[0]: 40, subjects[1]: 36}
	for subject, remaining := range left {
		for _, step := range []struct {
			ago  time.Duration
			plus float64
		}{{4 * time.Hour, 16}, {2 * time.Hour, 8}, {time.Minute, 0}} {
			if err := usage.RecordAIAccountSubjectQuotaPoints(subject, "codex", []usage.QuotaSnapshotPoint{{
				RecordedAt: now.Add(-step.ago), Provider: "codex", QuotaKey: "code_week", QuotaLabel: "Weekly",
				Percent: ptr(remaining + step.plus), ResetAt: &reset, WindowSeconds: 604800,
			}}); err != nil {
				t.Fatal(err)
			}
		}
		checked := now.Add(-time.Minute)
		if err := usage.UpsertAIAccountSubjectStatus(usage.AIAccountSubjectStatusRecord{
			AuthSubjectID: subject, Provider: "codex", UpstreamCheckedAt: &checked,
			Quotas: []usage.QuotaWindowDTO{{QuotaKey: "code_week", QuotaLabel: "Weekly", Percent: ptr(remaining), ResetAt: &reset, WindowSeconds: 604800}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	gin.SetMode(gin.TestMode)
	h := &Handler{authManager: manager}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(managementPrincipalKey, identity.Principal{EffectiveTenant: identity.Tenant{ID: tenant}})
	})
	router.GET("/ai-accounts/pool-capacity", h.GetPoolCapacity)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ai-accounts/pool-capacity", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "sk-never-in-a-pool-response") {
		t.Fatalf("the response carries a key: %s", rec.Body.String())
	}
	var resp poolCapacityResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Pools) != 1 {
		t.Fatalf("pools = %+v, want the one Codex weekly pool", resp.Pools)
	}
	p := resp.Pools[0]
	if p.Provider != "codex" || p.WindowKey != "code_week" || p.Accounts != 3 || p.AccountsWithData != 2 || p.RemainingPercent != 38 {
		t.Fatalf("pool = %+v, want 2 of 3 accounts (the silent one is a gap, the API key is not an account) at 38%%", p)
	}
	if p.BurnPercentPerHour == nil || *p.BurnPercentPerHour < 3.9 || *p.BurnPercentPerHour > 4.1 {
		t.Fatalf("burn = %v, want about 4 points an hour", p.BurnPercentPerHour)
	}
	if p.ProjectedExhaustionAt == nil || p.ProjectedExhaustionAt.Before(now.Add(9*time.Hour)) || p.ProjectedExhaustionAt.After(now.Add(10*time.Hour)) {
		t.Fatalf("projected exhaustion = %v, want about 9.5 hours from now", p.ProjectedExhaustionAt)
	}
	if p.EarliestResetAt == nil || !p.EarliestResetAt.Equal(reset) {
		t.Fatalf("earliest reset = %v, want %v", p.EarliestResetAt, reset)
	}

	// A second tenant has no accounts: an empty list, and not the first one's pools.
	other := gin.New()
	other.Use(func(c *gin.Context) {
		c.Set(managementPrincipalKey, identity.Principal{EffectiveTenant: identity.Tenant{ID: "tenant-without-accounts"}})
	})
	other.GET("/ai-accounts/pool-capacity", h.GetPoolCapacity)
	rec = httptest.NewRecorder()
	other.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ai-accounts/pool-capacity", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"pools":[]`) {
		t.Fatalf("other tenant: %d %s, want an empty list", rec.Code, rec.Body.String())
	}
}

func TestPoolCapacityFailureIsAPlain500(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(managementPrincipalKey, identity.Principal{EffectiveTenant: identity.Tenant{ID: "tenant-x"}})
	})
	router.GET("/ai-accounts/pool-capacity", func(c *gin.Context) {
		h.servePoolCapacity(c, func(context.Context, string, time.Time) (poolCapacityResponse, error) {
			return poolCapacityResponse{}, errors.New(`pq: password authentication failed for user "cliproxy" host=10.9.8.7`)
		})
	})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ai-accounts/pool-capacity", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Error.Code != "pool_capacity_failed" {
		t.Fatalf("body = %s (%v)", rec.Body.String(), err)
	}
	for _, leaked := range []string{"password", "cliproxy", "10.9.8.7"} {
		if strings.Contains(rec.Body.String(), leaked) {
			t.Fatalf("the 500 body leaks %q: %s", leaked, rec.Body.String())
		}
	}
}
