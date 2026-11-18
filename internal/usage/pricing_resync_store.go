package usage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// Request costs are worked out once, when a request is logged, with the price
// the model has at that moment. A model that had no price then (a new model the
// catalog had not priced yet) was stored at zero, and a price changed later
// never reached the requests already logged. A pricing resync job recomputes
// them: see pricing_resync.go for how a job runs.

const (
	// PricingResyncFill recomputes only the requests stored without a cost. It is
	// what every automatic price change and, by default, a manual edit does:
	// requests already costed at the old price keep it.
	PricingResyncFill = "fill"
	// PricingResyncReprice recomputes every request of the last seven days with
	// the current price. Only a manual edit asks for it, to correct a wrong price.
	PricingResyncReprice = "reprice"

	pricingResyncPending = "pending"
	pricingResyncRunning = "running"
	pricingResyncDone    = "done"
	pricingResyncFailed  = "failed"

	// PricingResyncOriginManual and the others record what changed the price.
	PricingResyncOriginManual    = "manual"
	PricingResyncOriginAuto      = "auto"
	PricingResyncOriginReconcile = "reconcile"
)

// pricingResyncSettleDelay is how long a job waits before it runs. A price
// edited on one node reaches the others through configsync; until every node
// has reloaded it, some keep logging requests at the old price, and a job that
// ran at once would scan past them. Requests logged after upper_time are left
// to the next reconcile pass.
const pricingResyncSettleDelay = 90 * time.Second

// PricingResyncJob is one queued or finished recomputation.
type PricingResyncJob struct {
	ID         int64    `json:"id"`
	TenantID   string   `json:"tenant_id"`
	ModelIDs   []string `json:"model_ids"`
	Mode       string   `json:"mode"`
	Origin     string   `json:"origin"`
	Status     string   `json:"status"`
	NotBefore  string   `json:"not_before"`
	UpperTime  string   `json:"upper_time"`
	CursorID   int64    `json:"cursor_id"`
	Scanned    int64    `json:"scanned"`
	Changed    int64    `json:"changed"`
	Skipped    int64    `json:"skipped"`
	CostDelta  float64  `json:"cost_delta"`
	LastError  string   `json:"last_error,omitempty"`
	CreatedAt  string   `json:"created_at"`
	UpdatedAt  string   `json:"updated_at"`
	FinishedAt string   `json:"finished_at,omitempty"`
}

const createPricingResyncJobsSQLite = `
CREATE TABLE IF NOT EXISTS pricing_resync_jobs (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  tenant_id   TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  model_ids   TEXT NOT NULL DEFAULT '',
  mode        TEXT NOT NULL DEFAULT 'fill',
  origin      TEXT NOT NULL DEFAULT '',
  status      TEXT NOT NULL DEFAULT 'pending',
  not_before  TIMESTAMP NOT NULL,
  upper_time  TIMESTAMP NOT NULL,
  cursor_id   INTEGER NOT NULL DEFAULT 0,
  scanned     INTEGER NOT NULL DEFAULT 0,
  changed     INTEGER NOT NULL DEFAULT 0,
  skipped     INTEGER NOT NULL DEFAULT 0,
  cost_delta  REAL NOT NULL DEFAULT 0,
  last_error  TEXT NOT NULL DEFAULT '',
  created_at  TIMESTAMP NOT NULL,
  updated_at  TIMESTAMP NOT NULL,
  finished_at TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_pricing_resync_jobs_due ON pricing_resync_jobs(status, not_before);
CREATE INDEX IF NOT EXISTS idx_pricing_resync_jobs_tenant ON pricing_resync_jobs(tenant_id, id DESC);
`

// ensurePricingResyncJobsTable creates the job table on SQLite. PostgreSQL gets
// it from the runtime migration 202611180003_pricing_resync_jobs.
func ensurePricingResyncJobsTable(db *sql.DB) {
	if db == nil || metadataForUsageDB(db).driver == "postgres" {
		return
	}
	if _, err := db.Exec(createPricingResyncJobsSQLite); err != nil {
		log.Warnf("usage: create pricing_resync_jobs: %v", err)
	}
}

func normalizePricingResyncModelIDs(modelIDs []string) []string {
	seen := make(map[string]struct{}, len(modelIDs))
	out := make([]string, 0, len(modelIDs))
	for _, id := range modelIDs {
		id = strings.ToLower(strings.TrimSpace(id))
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// EnqueuePricingResync queues a recomputation of the requests priced from the
// given catalog IDs of a tenant. A pending job of the same tenant and mode that
// has not started takes the new IDs instead, and waits the settle delay again,
// so a burst of edits (or one OpenRouter sync touching many models) runs as one
// job. It returns the id of the job that will do the work.
func EnqueuePricingResync(tenantID string, modelIDs []string, mode, origin string) (int64, error) {
	db := getDB()
	if db == nil {
		return 0, errUsageDBUnavailable
	}
	return enqueuePricingResyncDB(db, tenantID, modelIDs, mode, origin, time.Now())
}

func enqueuePricingResyncDB(db *sql.DB, tenantID string, modelIDs []string, mode, origin string, now time.Time) (int64, error) {
	tenantID = normalizeTenantID(tenantID)
	modelIDs = normalizePricingResyncModelIDs(modelIDs)
	if len(modelIDs) == 0 {
		return 0, nil
	}
	if mode != PricingResyncReprice {
		mode = PricingResyncFill
	}
	notBefore := now.Add(pricingResyncSettleDelay).UTC()
	stamp := now.UTC().Format(time.RFC3339Nano)

	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("usage: begin pricing resync enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		id       int64
		existing string
	)
	err = tx.QueryRow(`
		SELECT id, model_ids FROM pricing_resync_jobs
		WHERE tenant_id = ? AND mode = ? AND status = ?
		ORDER BY id DESC LIMIT 1
	`, tenantID, mode, pricingResyncPending).Scan(&id, &existing)
	switch {
	case err == nil:
		var merged []string
		_ = json.Unmarshal([]byte(existing), &merged)
		merged = normalizePricingResyncModelIDs(append(merged, modelIDs...))
		encoded, _ := json.Marshal(merged)
		if _, err := tx.Exec(`
			UPDATE pricing_resync_jobs
			SET model_ids = ?, not_before = ?, upper_time = ?, updated_at = ?,
			    origin = CASE WHEN origin = ? THEN origin ELSE ? END
			WHERE id = ? AND status = ?
		`, string(encoded), notBefore.Format(time.RFC3339Nano), notBefore.Format(time.RFC3339Nano), stamp,
			PricingResyncOriginManual, origin, id, pricingResyncPending); err != nil {
			return 0, fmt.Errorf("usage: merge pricing resync job: %w", err)
		}
	case err == sql.ErrNoRows:
		encoded, _ := json.Marshal(modelIDs)
		insertSQL := `
			INSERT INTO pricing_resync_jobs
			  (tenant_id, model_ids, mode, origin, status, not_before, upper_time, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
		args := []any{tenantID, string(encoded), mode, origin, pricingResyncPending,
			notBefore.Format(time.RFC3339Nano), notBefore.Format(time.RFC3339Nano), stamp, stamp}
		if metadataForUsageDB(db).driver == "postgres" {
			if err := tx.QueryRow(insertSQL+" RETURNING id", args...).Scan(&id); err != nil {
				return 0, fmt.Errorf("usage: insert pricing resync job: %w", err)
			}
		} else {
			result, err := tx.Exec(insertSQL, args...)
			if err != nil {
				return 0, fmt.Errorf("usage: insert pricing resync job: %w", err)
			}
			if id, err = result.LastInsertId(); err != nil {
				return 0, fmt.Errorf("usage: pricing resync job id: %w", err)
			}
		}
	default:
		return 0, fmt.Errorf("usage: find pending pricing resync job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("usage: commit pricing resync enqueue: %w", err)
	}
	return id, nil
}

const pricingResyncJobColumns = `id, tenant_id, model_ids, mode, origin, status, not_before, upper_time,
	cursor_id, scanned, changed, skipped, cost_delta, last_error, created_at, updated_at, finished_at`

func scanPricingResyncJob(scan func(dest ...any) error) (PricingResyncJob, error) {
	var (
		job                                         PricingResyncJob
		tenantID, modelIDs                          string
		notBefore, upperTime, created, updated, fin storedTime
	)
	if err := scan(&job.ID, &tenantID, &modelIDs, &job.Mode, &job.Origin, &job.Status, &notBefore, &upperTime,
		&job.CursorID, &job.Scanned, &job.Changed, &job.Skipped, &job.CostDelta, &job.LastError, &created, &updated, &fin); err != nil {
		return PricingResyncJob{}, err
	}
	job.TenantID = normalizeTenantID(tenantID)
	_ = json.Unmarshal([]byte(modelIDs), &job.ModelIDs)
	job.NotBefore = formatStoredTimeRFC3339(notBefore)
	job.UpperTime = formatStoredTimeRFC3339(upperTime)
	job.CreatedAt = formatStoredTimeRFC3339(created)
	job.UpdatedAt = formatStoredTimeRFC3339(updated)
	job.FinishedAt = formatStoredTimeRFC3339(fin)
	return job, nil
}

func formatStoredTimeRFC3339(value storedTime) string {
	if !value.Valid {
		return ""
	}
	return value.Time.UTC().Format(time.RFC3339)
}

// ListPricingResyncJobs returns a tenant's latest jobs, newest first.
func ListPricingResyncJobs(tenantID string, limit int) ([]PricingResyncJob, error) {
	db := getReadDB()
	if db == nil {
		return []PricingResyncJob{}, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := db.Query(`SELECT `+pricingResyncJobColumns+` FROM pricing_resync_jobs
		WHERE tenant_id = ? ORDER BY id DESC LIMIT ?`, normalizeTenantID(tenantID), limit)
	if err != nil {
		return nil, fmt.Errorf("usage: list pricing resync jobs: %w", err)
	}
	defer rows.Close()
	out := make([]PricingResyncJob, 0)
	for rows.Next() {
		job, err := scanPricingResyncJob(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("usage: scan pricing resync job: %w", err)
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

// pricingResyncStaleRunning is when a running job counts as abandoned: its
// leader stopped (a deploy, a crash, a leadership change) between batches. The
// next leader resumes it from its cursor. Batches commit their progress with
// their rows, so resuming never applies a delta twice.
const pricingResyncStaleRunning = 2 * time.Minute

// nextDuePricingResyncJob claims the oldest job that may run now: a pending job
// past its settle delay, or a running one abandoned by a stopped leader.
func nextDuePricingResyncJob(db *sql.DB, now time.Time) (PricingResyncJob, bool, error) {
	row := db.QueryRow(`SELECT `+pricingResyncJobColumns+` FROM pricing_resync_jobs
		WHERE (status = ? AND not_before <= ?) OR (status = ? AND updated_at <= ?)
		ORDER BY id ASC LIMIT 1`,
		pricingResyncPending, now.UTC().Format(time.RFC3339Nano),
		pricingResyncRunning, now.Add(-pricingResyncStaleRunning).UTC().Format(time.RFC3339Nano))
	job, err := scanPricingResyncJob(row.Scan)
	if err == sql.ErrNoRows {
		return PricingResyncJob{}, false, nil
	}
	if err != nil {
		return PricingResyncJob{}, false, fmt.Errorf("usage: next pricing resync job: %w", err)
	}
	result, err := db.Exec(`UPDATE pricing_resync_jobs SET status = ?, updated_at = ?
		WHERE id = ? AND status = ?`, pricingResyncRunning, now.UTC().Format(time.RFC3339Nano), job.ID, job.Status)
	if err != nil {
		return PricingResyncJob{}, false, fmt.Errorf("usage: claim pricing resync job: %w", err)
	}
	if n, _ := result.RowsAffected(); n != 1 {
		// Another runner took it first.
		return PricingResyncJob{}, false, nil
	}
	job.Status = pricingResyncRunning
	return job, true, nil
}

func finishPricingResyncJob(db *sql.DB, id int64, status, lastError string, now time.Time) {
	stamp := now.UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`UPDATE pricing_resync_jobs
		SET status = ?, last_error = ?, updated_at = ?, finished_at = ? WHERE id = ?`,
		status, lastError, stamp, stamp, id); err != nil {
		log.Warnf("usage: finish pricing resync job %d: %v", id, err)
	}
}

// pricingResyncRetention bounds the logs (and so the costs) a job can reach:
// request_logs keeps seven days, and older periods survive only as rollup
// totals, which cannot be re-priced request by request.
const pricingResyncRetention = 7 * 24 * time.Hour

// EffectivePricingFingerprint describes the price a tenant bills a model with,
// "" when it has none. Comparing it before and after an edit tells whether the
// edit changed what requests cost, through inheritance and base IDs included.
func EffectivePricingFingerprint(tenantID, modelID string) string {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return ""
	}
	row, ok := resolveModelPricingRowForTenant(normalizeTenantID(tenantID), modelID)
	if !ok || !row.Enabled || !hasEffectiveModelPricing(row) {
		return ""
	}
	return fmt.Sprintf("%s|%g|%g|%g|%g|%g|%g", normalizePricingMode(row.PricingMode),
		row.InputPricePerMillion, row.OutputPricePerMillion, row.CachedPricePerMillion,
		row.CacheReadPricePerMillion, row.CacheWritePricePerMillion, row.PricePerCall)
}
