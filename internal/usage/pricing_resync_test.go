package usage

import (
	"context"
	"database/sql"
	"math"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

const resyncTenant = "00000000-0000-0000-0000-0000000000bb"

func initPricingResyncTestDB(t *testing.T) {
	t.Helper()
	initTestUsageDB(t, config.RequestLogStorageConfig{})
	resetModelCatalogAutofillListForTest()
	t.Cleanup(resetModelCatalogAutofillListForTest)
}

func priceModel(t *testing.T, tenantID, model string, in, out, cached float64) {
	t.Helper()
	if err := UpsertModelConfigForTenant(tenantID, ModelConfigRow{
		ModelID: model, Enabled: true, PricingMode: "token",
		InputPricePerMillion: in, OutputPricePerMillion: out, CachedPricePerMillion: cached,
		Source: "user",
	}); err != nil {
		t.Fatalf("price %s: %v", model, err)
	}
	ReloadPricingCache()
}

func logResyncRequest(t *testing.T, keyID, model string, tokens TokenStats, at time.Time) {
	t.Helper()
	InsertRequestLog(RequestLogEntry{
		TrustedTenantID: resyncTenant, APIKey: "sk-" + keyID, APIKeyID: keyID, APIKeyName: keyID,
		Model: model, Source: "codex", Timestamp: at, Tokens: tokens,
	})
}

// runResync queues a job, lets its settle delay pass at once (it still scans
// up to its upper time, after every request of the test) and runs every due job.
func runResync(t *testing.T, tenantID string, models []string, mode string) PricingResyncJob {
	t.Helper()
	id, err := enqueuePricingResyncDB(getDB(), tenantID, models, mode, PricingResyncOriginManual, time.Now())
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := getDB().Exec(`UPDATE pricing_resync_jobs SET not_before = ? WHERE id = ?`,
		time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), id); err != nil {
		t.Fatalf("make job due: %v", err)
	}
	runDuePricingResyncJobs(context.Background(), getDB())
	jobs, err := ListPricingResyncJobs(tenantID, 100)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	for _, job := range jobs {
		if job.ID == id {
			return job
		}
	}
	t.Fatalf("job %d not listed", id)
	return PricingResyncJob{}
}

func requestCosts(t *testing.T, model string) []float64 {
	t.Helper()
	rows, err := getDB().Query(`SELECT cost FROM request_logs WHERE model = ? ORDER BY id`, model)
	if err != nil {
		t.Fatalf("costs: %v", err)
	}
	defer rows.Close()
	var out []float64
	for rows.Next() {
		var cost float64
		if err := rows.Scan(&cost); err != nil {
			t.Fatalf("scan cost: %v", err)
		}
		out = append(out, cost)
	}
	return out
}

func bucketTotals(t *testing.T, kind, model string) (cost float64, requests int64) {
	t.Helper()
	if err := getDB().QueryRow(`SELECT COALESCE(SUM(cost_total), 0), COALESCE(SUM(request_count), 0)
		FROM usage_rollup_buckets WHERE bucket_kind = ? AND model = ?`, kind, model).Scan(&cost, &requests); err != nil {
		t.Fatalf("bucket totals: %v", err)
	}
	return cost, requests
}

func sum(values []float64) float64 {
	total := 0.0
	for _, v := range values {
		total += v
	}
	return total
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

var resyncTokens = TokenStats{InputTokens: 120_000, OutputTokens: 900, CachedTokens: 100_000, TotalTokens: 120_900,
	CacheReported: true, CacheReadTokens: 100_000, CacheReadIncludedInInput: true}

func TestPricingResyncFillsCostsLoggedBeforeThePriceAndMovesEveryTotal(t *testing.T) {
	initPricingResyncTestDB(t)
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		logResyncRequest(t, "key-a", "fresh-model", resyncTokens, now.Add(-time.Duration(i+1)*time.Minute))
	}
	if got := requestCosts(t, "fresh-model"); sum(got) != 0 {
		t.Fatalf("logged without a price: costs %v", got)
	}
	_, requestsBefore := bucketTotals(t, rollupBucketDay, "fresh-model")

	priceModel(t, systemTenantID, "fresh-model", 2, 10, 0.1)
	job := runResync(t, systemTenantID, []string{"fresh-model"}, PricingResyncFill)

	want := CalculateCostV2ForTenant(resyncTenant, "fresh-model", resyncTokens)
	costs := requestCosts(t, "fresh-model")
	for _, cost := range costs {
		if !near(cost, want) {
			t.Fatalf("filled cost %v, want %v", cost, want)
		}
	}
	if job.Status != pricingResyncDone || job.Changed != 3 || !near(job.CostDelta, sum(costs)) {
		t.Fatalf("job = %+v", job)
	}
	// Every total the request was counted in moved by the same amount, and only
	// the cost did.
	for _, kind := range []string{rollupBucketMinute, rollupBucketQuotaMinuteUTC, rollupBucketHour, rollupBucketDay, rollupBucketLifetime} {
		cost, requests := bucketTotals(t, kind, "fresh-model")
		if !near(cost, sum(costs)) || requests != requestsBefore {
			t.Fatalf("%s bucket: cost %v requests %d, want %v and %d", kind, cost, requests, sum(costs), requestsBefore)
		}
	}
}

func TestPricingResyncWithAnUnchangedPriceChangesNothing(t *testing.T) {
	initPricingResyncTestDB(t)
	priceModel(t, systemTenantID, "steady-model", 2, 10, 0.1)
	now := time.Now().UTC()
	logResyncRequest(t, "key-a", "steady-model", resyncTokens, now.Add(-2*time.Minute))
	logResyncRequest(t, "key-a", "steady-model", TokenStats{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}, now.Add(-time.Minute))
	before := requestCosts(t, "steady-model")
	dayBefore, _ := bucketTotals(t, rollupBucketDay, "steady-model")

	job := runResync(t, systemTenantID, []string{"steady-model"}, PricingResyncReprice)
	if job.Changed != 0 || job.CostDelta != 0 || job.Scanned != 2 {
		t.Fatalf("reprice at the same price = %+v", job)
	}
	after := requestCosts(t, "steady-model")
	dayAfter, _ := bucketTotals(t, rollupBucketDay, "steady-model")
	if !near(sum(before), sum(after)) || !near(dayBefore, dayAfter) || dayBefore == 0 {
		t.Fatalf("costs %v -> %v, day bucket %v -> %v", before, after, dayBefore, dayAfter)
	}
}

func TestPricingResyncFillLeavesCostedRequestsAloneAndRepriceUpdatesThem(t *testing.T) {
	initPricingResyncTestDB(t)
	priceModel(t, systemTenantID, "edited-model", 2, 10, 0.1)
	now := time.Now().UTC()
	logResyncRequest(t, "key-a", "edited-model", resyncTokens, now.Add(-2*time.Minute))
	oldCost := requestCosts(t, "edited-model")[0]

	priceModel(t, systemTenantID, "edited-model", 4, 20, 0.2)
	if job := runResync(t, systemTenantID, []string{"edited-model"}, PricingResyncFill); job.Changed != 0 {
		t.Fatalf("fill touched a costed request: %+v", job)
	}
	if got := requestCosts(t, "edited-model")[0]; !near(got, oldCost) {
		t.Fatalf("fill changed %v to %v", oldCost, got)
	}

	job := runResync(t, systemTenantID, []string{"edited-model"}, PricingResyncReprice)
	newCost := requestCosts(t, "edited-model")[0]
	if !near(newCost, 2*oldCost) || job.Changed != 1 || !near(job.CostDelta, newCost-oldCost) {
		t.Fatalf("reprice: %v -> %v, job %+v", oldCost, newCost, job)
	}
	if day, requests := bucketTotals(t, rollupBucketDay, "edited-model"); !near(day, newCost) || requests != 1 {
		t.Fatalf("day bucket after reprice: cost %v requests %d", day, requests)
	}
}

func TestPricingResyncReachesModelsPricedThroughABaseID(t *testing.T) {
	initPricingResyncTestDB(t)
	logResyncRequest(t, "key-a", "family-6-sol", resyncTokens, time.Now().UTC().Add(-time.Minute))
	// Pricing the base ID prices the variant (modelPricingLookupModelIDs).
	priceModel(t, systemTenantID, "family-6", 2, 10, 0.1)
	job := runResync(t, systemTenantID, []string{"family-6"}, PricingResyncFill)
	if job.Changed != 1 || requestCosts(t, "family-6-sol")[0] == 0 {
		t.Fatalf("variant not filled through its base: %+v", job)
	}
}

func TestPricingResyncTwiceChangesNothingTheSecondTime(t *testing.T) {
	initPricingResyncTestDB(t)
	logResyncRequest(t, "key-a", "twice-model", resyncTokens, time.Now().UTC().Add(-time.Minute))
	priceModel(t, systemTenantID, "twice-model", 2, 10, 0.1)
	first := runResync(t, systemTenantID, []string{"twice-model"}, PricingResyncReprice)
	day, _ := bucketTotals(t, rollupBucketDay, "twice-model")
	second := runResync(t, systemTenantID, []string{"twice-model"}, PricingResyncReprice)
	dayAgain, _ := bucketTotals(t, rollupBucketDay, "twice-model")
	if first.Changed != 1 || second.Changed != 0 || !near(day, dayAgain) {
		t.Fatalf("first %+v second %+v, day %v -> %v", first, second, day, dayAgain)
	}
}

func TestPricingResyncSkipsAConcurrentlyChangedRow(t *testing.T) {
	initPricingResyncTestDB(t)
	logResyncRequest(t, "key-a", "raced-model", resyncTokens, time.Now().UTC().Add(-time.Minute))
	priceModel(t, systemTenantID, "raced-model", 2, 10, 0.1)
	job := PricingResyncJob{ID: 1, TenantID: systemTenantID, ModelIDs: []string{"raced-model"}, Mode: PricingResyncReprice,
		UpperTime: time.Now().Add(time.Minute).UTC().Format(time.RFC3339)}
	if _, err := getDB().Exec(`INSERT INTO pricing_resync_jobs (id, tenant_id, model_ids, mode, status, not_before, upper_time, created_at, updated_at)
		VALUES (1, ?, '["raced-model"]', 'reprice', 'running', ?, ?, ?, ?)`, systemTenantID, job.UpperTime, job.UpperTime, job.UpperTime, job.UpperTime); err != nil {
		t.Fatalf("job row: %v", err)
	}
	scope, err := pricingResyncAffectedModels(getDB(), job, time.Now())
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	rows, err := loadPricingResyncRows(context.Background(), getDB(), job, scope, 0, time.Now())
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows %v, %v", rows, err)
	}
	candidates, _ := pricingResyncCandidates(rows, job.Mode, newPricingResolver())
	// Another writer changes the cost after the batch was read.
	if _, err := getDB().Exec(`UPDATE request_logs SET cost = 0.5 WHERE id = ?`, rows[0].ID); err != nil {
		t.Fatalf("concurrent write: %v", err)
	}
	dayBefore, _ := bucketTotals(t, rollupBucketDay, "raced-model")
	if err := applyPricingResyncBatch(context.Background(), getDB(), 1, candidates, 1, 0, rows[0].ID, time.Now()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := requestCosts(t, "raced-model")[0]; got != 0.5 {
		t.Fatalf("guarded update overwrote the concurrent cost: %v", got)
	}
	if dayAfter, _ := bucketTotals(t, rollupBucketDay, "raced-model"); !near(dayBefore, dayAfter) {
		t.Fatalf("a skipped row moved the totals: %v -> %v", dayBefore, dayAfter)
	}
}

func TestPricingResyncNeverGuessesARowItCannotReproduce(t *testing.T) {
	initPricingResyncTestDB(t)
	tokens := TokenStats{InputTokens: 1000, OutputTokens: 100, TotalTokens: 1100, CacheReported: true, CacheWriteTokens: 800}
	logResyncRequest(t, "key-a", "legacy-model", tokens, time.Now().UTC().Add(-time.Minute))
	// A row logged before one-hour cache writes were stored.
	if _, err := getDB().Exec(`UPDATE request_logs SET cache_write_1h_tokens = NULL WHERE model = 'legacy-model'`); err != nil {
		t.Fatalf("legacy row: %v", err)
	}
	priceModel(t, systemTenantID, "legacy-model", 3, 15, 0.3)
	job := runResync(t, systemTenantID, []string{"legacy-model"}, PricingResyncFill)
	if job.Skipped != 1 || job.Changed != 0 || requestCosts(t, "legacy-model")[0] != 0 {
		t.Fatalf("job %+v costs %v", job, requestCosts(t, "legacy-model"))
	}
}

func TestPricingResyncKeepsARequestBeforeAResetOutOfTheSpendSinceIt(t *testing.T) {
	initPricingResyncTestDB(t)
	if err := UpsertAPIKeyForTenant(resyncTenant, APIKeyRow{ID: "key-reset", Key: "sk-key-reset", Name: "Reset"}); err != nil {
		t.Fatalf("api key: %v", err)
	}
	logResyncRequest(t, "key-reset", "reset-model", resyncTokens, time.Now().UTC().Add(-time.Minute))
	// The operator resets today's spend after the request was made.
	if err := UpsertDailySpendingReset(resyncTenant, "key-reset", 0); err != nil {
		t.Fatalf("reset: %v", err)
	}
	effective := func() float64 {
		t.Helper()
		used, err := QueryTodayEffectiveCostByKeyForTenant(resyncTenant, "sk-key-reset")
		if err != nil {
			t.Fatalf("effective today cost: %v", err)
		}
		return used
	}
	before := effective()
	priceModel(t, systemTenantID, "reset-model", 2, 10, 0.1)
	job := runResync(t, systemTenantID, []string{"reset-model"}, PricingResyncFill)
	if job.Changed != 1 {
		t.Fatalf("job %+v", job)
	}
	if after := effective(); !near(before, after) {
		t.Fatalf("a request from before the reset counts after it: %v -> %v", before, after)
	}
	var baseline float64
	if err := getDB().QueryRow(`SELECT cost_baseline FROM api_key_daily_spending_resets WHERE api_key_id = 'key-reset'`).Scan(&baseline); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if !near(baseline, job.CostDelta) {
		t.Fatalf("baseline %v, want %v", baseline, job.CostDelta)
	}
}

func TestPricingResyncReconcileQueuesOnceForTheSameRequests(t *testing.T) {
	initPricingResyncTestDB(t)
	logResyncRequest(t, "key-a", "late-model", resyncTokens, time.Now().UTC().Add(-time.Minute))
	state := &pricingResyncWorkerState{reconciled: map[string]int64{}}
	// No price yet: nothing to queue.
	if err := state.reconcile(context.Background(), getDB(), time.Now()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if jobs, _ := ListPricingResyncJobs(resyncTenant, 10); len(jobs) != 0 {
		t.Fatalf("queued without a price: %+v", jobs)
	}
	priceModel(t, systemTenantID, "late-model", 2, 10, 0.1)
	for i := 0; i < 2; i++ {
		if err := state.reconcile(context.Background(), getDB(), time.Now()); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	jobs, _ := ListPricingResyncJobs(resyncTenant, 10)
	if len(jobs) != 1 || jobs[0].Mode != PricingResyncFill || jobs[0].Origin != PricingResyncOriginReconcile {
		t.Fatalf("jobs = %+v", jobs)
	}
}

func TestEnqueueMergesEditsIntoThePendingJob(t *testing.T) {
	initPricingResyncTestDB(t)
	now := time.Now()
	first, err := enqueuePricingResyncDB(getDB(), systemTenantID, []string{"b", "a"}, PricingResyncFill, PricingResyncOriginManual, now)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	second, err := enqueuePricingResyncDB(getDB(), systemTenantID, []string{"c", "A"}, PricingResyncFill, PricingResyncOriginAuto, now.Add(time.Second))
	if err != nil || second != first {
		t.Fatalf("second enqueue = %d, %v; want merged into %d", second, err, first)
	}
	reprice, _ := enqueuePricingResyncDB(getDB(), systemTenantID, []string{"a"}, PricingResyncReprice, PricingResyncOriginManual, now)
	if reprice == first {
		t.Fatal("a reprice merged into a fill job")
	}
	jobs, _ := ListPricingResyncJobs(systemTenantID, 10)
	var merged PricingResyncJob
	for _, job := range jobs {
		if job.ID == first {
			merged = job
		}
	}
	if len(merged.ModelIDs) != 3 || merged.ModelIDs[0] != "a" || merged.ModelIDs[2] != "c" || merged.Origin != PricingResyncOriginManual {
		t.Fatalf("merged job = %+v", merged)
	}
	if _, ok, _ := nextDuePricingResyncJob(getDB(), now); ok {
		t.Fatal("a job ran before its settle delay")
	}
}

func TestRestorePricingTokens(t *testing.T) {
	valid := func(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }
	none := sql.NullInt64{}
	cases := []struct {
		name                   string
		input                  int64
		read, write, prompt, h sql.NullInt64
		exact, included        bool
	}{
		{name: "cache not reported", input: 100, read: none, write: none, prompt: valid(100), h: none, exact: true},
		{name: "reads counted in input (OpenAI)", input: 1000, read: valid(800), write: valid(0), prompt: valid(1000), h: valid(0), exact: true, included: true},
		{name: "reads apart from input (Anthropic)", input: 200, read: valid(800), write: valid(0), prompt: valid(1000), h: valid(0), exact: true},
		{name: "no reads", input: 1000, read: valid(0), write: valid(0), prompt: valid(1000), h: valid(0), exact: true},
		{name: "writes without the one-hour share", input: 200, read: valid(0), write: valid(500), prompt: valid(700), h: none, exact: false},
		{name: "prompt fits neither form", input: 200, read: valid(800), write: valid(0), prompt: valid(5), h: valid(0), exact: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tokens := TokenStats{InputTokens: tc.input}
			exact := restorePricingTokens(&tokens, tc.read, tc.write, tc.prompt, tc.h)
			if exact != tc.exact || (exact && tokens.CacheReadIncludedInInput != tc.included) {
				t.Fatalf("exact %v included %v, want %v %v", exact, tokens.CacheReadIncludedInInput, tc.exact, tc.included)
			}
		})
	}
}

// The stored columns give back exactly what the live path priced.
func TestStoredRowsReproduceTheLoggedCost(t *testing.T) {
	initPricingResyncTestDB(t)
	if err := UpsertModelConfigForTenant(systemTenantID, ModelConfigRow{
		ModelID: "cache-model", Enabled: true, PricingMode: "token",
		InputPricePerMillion: 3, OutputPricePerMillion: 15, CachedPricePerMillion: 0.3, CacheWritePricePerMillion: 3.75,
		Source: "user",
	}); err != nil {
		t.Fatalf("price: %v", err)
	}
	ReloadPricingCache()
	cases := []TokenStats{
		resyncTokens,
		{InputTokens: 200, OutputTokens: 50, TotalTokens: 1250, CacheReported: true, CacheReadTokens: 800, CacheWriteTokens: 200, CacheWrite1hTokens: 150},
		{InputTokens: 500, OutputTokens: 50, CachedTokens: 300, TotalTokens: 550},
		{InputTokens: 500, OutputTokens: 50, TotalTokens: 550},
	}
	now := time.Now().UTC()
	for i, tokens := range cases {
		logResyncRequest(t, "key-a", "cache-model", tokens, now.Add(-time.Duration(i+1)*time.Second))
	}
	job := PricingResyncJob{TenantID: systemTenantID, ModelIDs: []string{"cache-model"}, Mode: PricingResyncReprice,
		UpperTime: now.Add(time.Minute).Format(time.RFC3339)}
	scope, _ := pricingResyncAffectedModels(getDB(), job, now)
	rows, err := loadPricingResyncRows(context.Background(), getDB(), job, scope, 0, now)
	if err != nil || len(rows) != len(cases) {
		t.Fatalf("rows %d, %v", len(rows), err)
	}
	prices := newPricingResolver()
	for _, row := range rows {
		if !row.Exact {
			t.Fatalf("row %d not exact: %+v", row.ID, row.Tokens)
		}
		if got := prices.cost(row.TenantID, row.Model, row.Tokens); got != row.Cost {
			t.Fatalf("row %d recomputes to %v, logged %v (tokens %+v)", row.ID, got, row.Cost, row.Tokens)
		}
	}
}

func TestPricingResyncMovesTheAIAccountTotalsByTheSameAmount(t *testing.T) {
	initPricingResyncTestDB(t)
	InsertRequestLog(RequestLogEntry{
		TrustedTenantID: resyncTenant, APIKey: "sk-subject", APIKeyID: "key-subject", AuthSubjectID: "subject-1",
		Model: "subject-model", Source: "codex", Timestamp: time.Now().UTC().Add(-time.Minute), Tokens: resyncTokens,
	})
	subjectTotals := func(kind string) (cost float64, requests int64) {
		t.Helper()
		if err := getDB().QueryRow(`SELECT COALESCE(SUM(cost_total), 0), COALESCE(SUM(request_count), 0)
			FROM ai_account_subject_usage_buckets WHERE auth_subject_id = 'subject-1' AND bucket_kind = ?`, kind).Scan(&cost, &requests); err != nil {
			t.Fatalf("subject totals: %v", err)
		}
		return cost, requests
	}
	_, requestsBefore := subjectTotals("day")
	priceModel(t, systemTenantID, "subject-model", 2, 10, 0.1)
	job := runResync(t, systemTenantID, []string{"subject-model"}, PricingResyncFill)
	for _, kind := range []string{"day", "lifetime"} {
		cost, requests := subjectTotals(kind)
		if !near(cost, job.CostDelta) || requests != requestsBefore || job.CostDelta == 0 {
			t.Fatalf("%s: cost %v requests %d, want %v and %d", kind, cost, requests, job.CostDelta, requestsBefore)
		}
	}
}

func TestPricingResyncRunsOnlyOnTheLeader(t *testing.T) {
	initPricingResyncTestDB(t)
	logResyncRequest(t, "key-a", "follower-model", resyncTokens, time.Now().UTC().Add(-time.Minute))
	priceModel(t, systemTenantID, "follower-model", 2, 10, 0.1)
	installClusterRole(t, false)
	id, err := enqueuePricingResyncDB(getDB(), systemTenantID, []string{"follower-model"}, PricingResyncFill, PricingResyncOriginManual, time.Now())
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := getDB().Exec(`UPDATE pricing_resync_jobs SET not_before = ? WHERE id = ?`, time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), id); err != nil {
		t.Fatalf("make due: %v", err)
	}
	state := &pricingResyncWorkerState{reconciled: map[string]int64{}}
	state.tick(context.Background(), time.Now())
	if jobs := pendingJobs(t, systemTenantID); len(jobs) != 1 || jobs[0].Status != pricingResyncPending || jobs[0].Scanned != 0 {
		t.Fatalf("a follower ran the job: %+v", jobs)
	}
	if costs := requestCosts(t, "follower-model"); costs[0] != 0 {
		t.Fatalf("a follower wrote costs: %v", costs)
	}
}
