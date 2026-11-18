package usage

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	log "github.com/sirupsen/logrus"
)

// The pricing resync worker runs on the cluster leader only (a single node is
// always the leader), like the OpenRouter sync: the jobs and the catalog are
// shared, and a second node would only repeat the same reads and writes. Every
// tick it
//   - fills catalog prices for models in use that have none (see
//     model_catalog_autofill.go), at most every modelCatalogAutofillInterval;
//   - reconciles: requests of the last days stored without a cost whose model
//     has a price now get a fill job. This catches every way a price can appear
//     without a job of its own (a startup merge, a seed, a node that logged at
//     the old price after a job had scanned past it);
//   - runs the jobs that are due.

const (
	pricingResyncTick              = 10 * time.Second
	pricingResyncReconcileInterval = 15 * time.Minute
	// pricingResyncJobsPerTick bounds how long one tick can run, so the
	// reconcile and autofill passes get their turn on a busy queue.
	pricingResyncJobsPerTick = 5
)

var (
	pricingResyncWorkerOnce sync.Once
	errPricingResyncStopped = errors.New("usage: pricing resync stopped: no longer the leader")
)

// StartPricingResyncWorker starts the worker; later calls do nothing.
func StartPricingResyncWorker(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	pricingResyncWorkerOnce.Do(func() {
		go runPricingResyncWorker(ctx)
	})
}

func runPricingResyncWorker(ctx context.Context) {
	ticker := time.NewTicker(pricingResyncTick)
	defer ticker.Stop()
	state := &pricingResyncWorkerState{reconciled: map[string]int64{}}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		state.tick(ctx, time.Now())
	}
}

type pricingResyncWorkerState struct {
	lastReconcile time.Time
	lastAutofill  time.Time
	// reconciled remembers, per tenant and model, how many fillable requests the
	// last reconcile queued a job for. The same count again means the job ran and
	// could not change them (for example requests whose recomputed cost is still
	// zero), so they are not queued every pass.
	reconciled map[string]int64
}

func (s *pricingResyncWorkerState) tick(ctx context.Context, now time.Time) {
	if !cluster.Default().IsLeader() {
		return
	}
	db := getDB()
	if db == nil {
		return
	}
	if now.Sub(s.lastAutofill) >= modelCatalogAutofillInterval {
		s.lastAutofill = now
		runModelCatalogAutofill(ctx, now)
	}
	if now.Sub(s.lastReconcile) >= pricingResyncReconcileInterval {
		s.lastReconcile = now
		if err := s.reconcile(ctx, db, now); err != nil && ctx.Err() == nil {
			log.Warnf("usage: pricing resync reconcile: %v", err)
		}
	}
	runDuePricingResyncJobs(ctx, db)
}

func runDuePricingResyncJobs(ctx context.Context, db *sql.DB) {
	leading := func() bool { return cluster.Default().IsLeader() }
	for i := 0; i < pricingResyncJobsPerTick; i++ {
		if ctx.Err() != nil || !leading() {
			return
		}
		job, ok, err := nextDuePricingResyncJob(db, time.Now())
		if err != nil {
			log.Warnf("usage: pricing resync: %v", err)
			return
		}
		if !ok {
			return
		}
		err = runPricingResyncJob(ctx, db, job, leading, time.Now)
		switch {
		case err == nil:
		case ctx.Err() != nil, errors.Is(err, errPricingResyncStopped):
			// Stopped between batches: the job stays running with its cursor and
			// is resumed once it counts as abandoned.
			return
		default:
			log.Warnf("usage: pricing resync job %d failed: %v", job.ID, err)
			finishPricingResyncJob(db, job.ID, pricingResyncFailed, err.Error(), time.Now())
		}
	}
}

// reconcile queues a fill job for each tenant and model with requests stored
// without a cost that its price now covers.
func (s *pricingResyncWorkerState) reconcile(ctx context.Context, db *sql.DB, now time.Time) error {
	rows, err := db.QueryContext(ctx, `
		SELECT tenant_id, model,
		       SUM(CASE WHEN input_tokens > 0 OR output_tokens > 0 OR cached_tokens > 0 THEN 1 ELSE 0 END),
		       COUNT(*)
		FROM request_logs
		WHERE timestamp >= ? AND cost = 0
		  AND NOT (COALESCE(cache_write_tokens, 0) > 0 AND cache_write_1h_tokens IS NULL)
		GROUP BY tenant_id, model`, now.Add(-pricingResyncRetention).UTC().Format(time.RFC3339))
	if err != nil {
		return err
	}
	type pair struct {
		tenant, model string
		withTokens    int64
		total         int64
	}
	pairs := make([]pair, 0)
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.tenant, &p.model, &p.withTokens, &p.total); err != nil {
			_ = rows.Close()
			return err
		}
		p.tenant = normalizeTenantID(p.tenant)
		pairs = append(pairs, p)
	}
	if err := closeRows(rows); err != nil {
		return err
	}
	for _, p := range pairs {
		if p.model == "" {
			continue
		}
		row, ok := resolveModelPricingRowForTenant(p.tenant, p.model)
		if !ok || !row.Enabled || !hasEffectiveModelPricing(row) {
			continue
		}
		fillable := p.withTokens
		if normalizePricingMode(row.PricingMode) == "call" {
			// A per-call price applies to every logged request, as it did live.
			fillable = p.total
		}
		key := p.tenant + "\x00" + p.model
		if fillable == 0 || s.reconciled[key] == fillable {
			continue
		}
		if _, err := enqueuePricingResyncDB(db, p.tenant, []string{p.model}, PricingResyncFill, PricingResyncOriginReconcile, now); err != nil {
			return err
		}
		s.reconciled[key] = fillable
	}
	return nil
}
