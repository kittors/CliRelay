package usage

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// Serializes absolute rollup rebuild against live request_log writers so blue-green
// catch-up rebuilds cannot race mid-UPSERT.
var usageProjectionMu sync.RWMutex

// Earliest wall time when pending→done finalize is allowed (after blue-green drain).
// Zero means not scheduled / allow only explicit init pending rebuild.
var (
	rollupCatchupEarliestMu sync.Mutex
	rollupCatchupEarliest   time.Time
)

const (
	rollupBucketMinute         = "minute"
	rollupBucketQuotaMinuteUTC = "quota_minute_utc"
	rollupBucketHour           = "hour"
	rollupBucketDay            = "day"
	rollupBucketLifetime       = "lifetime"
	rollupLifetimeStart        = "1970-01-01"
	// minute: 24h, hour: 30d, day: 400d (covers 365d heatmap + buffer)
	rollupMinuteRetention = 24 * time.Hour
	rollupHourRetention   = 30 * 24 * time.Hour
	rollupDayRetention    = 400 * 24 * time.Hour
	usageRollupSchemaSQL  = `
CREATE TABLE IF NOT EXISTS usage_rollup_buckets (
  tenant_id               TEXT NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
  bucket_kind             TEXT NOT NULL,
  bucket_start            TEXT NOT NULL,
  api_key_id              TEXT NOT NULL DEFAULT '',
  end_user_id             TEXT NOT NULL DEFAULT '',
  auth_subject_id         TEXT NOT NULL DEFAULT '',
  model                   TEXT NOT NULL DEFAULT '',
  source                  TEXT NOT NULL DEFAULT '',
  channel_name            TEXT NOT NULL DEFAULT '',
  request_count           INTEGER NOT NULL DEFAULT 0,
  success_count           INTEGER NOT NULL DEFAULT 0,
  failure_count           INTEGER NOT NULL DEFAULT 0,
  streaming_count         INTEGER NOT NULL DEFAULT 0,
  input_tokens            INTEGER NOT NULL DEFAULT 0,
  output_tokens           INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens        INTEGER NOT NULL DEFAULT 0,
  cached_tokens           INTEGER NOT NULL DEFAULT 0,
  effective_input_tokens  INTEGER NOT NULL DEFAULT 0,
  total_tokens            INTEGER NOT NULL DEFAULT 0,
  cost_total              REAL NOT NULL DEFAULT 0,
  latency_sum_ms          INTEGER NOT NULL DEFAULT 0,
  latency_count           INTEGER NOT NULL DEFAULT 0,
  first_token_sum_ms      INTEGER NOT NULL DEFAULT 0,
  first_token_count       INTEGER NOT NULL DEFAULT 0,
  updated_at              DATETIME NOT NULL,
  PRIMARY KEY (
    tenant_id, bucket_kind, bucket_start,
    api_key_id, end_user_id, auth_subject_id,
    model, source, channel_name
  )
);
CREATE INDEX IF NOT EXISTS idx_usage_rollup_tenant_kind_start
  ON usage_rollup_buckets(tenant_id, bucket_kind, bucket_start);
CREATE INDEX IF NOT EXISTS idx_usage_rollup_tenant_key_day
  ON usage_rollup_buckets(tenant_id, bucket_kind, api_key_id, bucket_start);
CREATE INDEX IF NOT EXISTS idx_usage_rollup_tenant_user_day
  ON usage_rollup_buckets(tenant_id, bucket_kind, end_user_id, bucket_start);
CREATE INDEX IF NOT EXISTS idx_usage_rollup_tenant_subject_day
  ON usage_rollup_buckets(tenant_id, bucket_kind, auth_subject_id, bucket_start);
`
	// Bump marker when rebuild semantics change so upgrades re-run once.
	usageRollupBackfillMarker      = "usage_rollup_buckets_v4"
	quotaMinuteCoverageStartMarker = "quota_minute_utc_coverage_start"
	rollupMarkerPending            = "pending"
	rollupMarkerDone               = "done"
)

type rollupEvent struct {
	TenantID      string
	APIKeyID      string
	EndUserID     string
	AuthSubjectID string
	Model         string
	Source        string
	ChannelName   string
	Failed        bool
	Streaming     bool
	LatencyMs     int64
	FirstTokenMs  int64
	Tokens        TokenStats
	Cost          float64
	At            time.Time
}

func ensureUsageRollupTables(db *sql.DB) error {
	if db == nil {
		return nil
	}
	if _, err := db.Exec(usageRollupSchemaSQL); err != nil {
		return fmt.Errorf("usage: create usage_rollup_buckets: %w", err)
	}
	return nil
}

func bootstrapUsageRollup(db *sql.DB, loc *time.Location) error {
	_ = loc
	// Schema only during InitDB/InitPostgres. Historical rebuild runs later via
	// RunUsageRollupBackfillAtInit after YAML key import + end-user backfill.
	return ensureUsageRollupTables(db)
}

// RunUsageRollupBackfillAtInit rebuilds usage_rollup_buckets from surviving request_logs.
// Must run after api_keys are imported and end_user_id ownership is populated.
// Leaves marker as "pending" so a post-drain catch-up can include old-slot rows
// before detail cleanup is allowed ("done").
func RunUsageRollupBackfillAtInit() error {
	return runUsageRollupBackfillAtInitDB(getDB(), getUsageLocation(), rollupMarkerPending)
}

// ScheduleUsageRollupBlueGreenCatchup waits for old-slot drain, then absolute-rebuilds
// once more and marks done. Only runs while marker is not yet done — never wipes
// lifetime rollup after detail retention has already pruned request_logs.
// Retries until success so a single failed catch-up cannot leave cleanup gated forever.
func ScheduleUsageRollupBlueGreenCatchup() {
	// Maintenance must not finalize before this instant (drain + cutover lag).
	rollupCatchupEarliestMu.Lock()
	rollupCatchupEarliest = time.Now().Add(90 * time.Second)
	earliest := rollupCatchupEarliest
	rollupCatchupEarliestMu.Unlock()

	go func() {
		if d := time.Until(earliest); d > 0 {
			time.Sleep(d)
		}
		attempt := 0
		for {
			db := getDB()
			if usageRollupBackfillCompleted(db) {
				return
			}
			attempt++
			if err := runUsageRollupBackfillAtInitDB(db, getUsageLocation(), rollupMarkerDone); err != nil {
				log.Errorf("usage: blue-green rollup catch-up attempt %d failed: %v", attempt, err)
				// Keep retrying; detail cleanup stays blocked while pending.
				time.Sleep(60 * time.Second)
				continue
			}
			log.Infof("usage: blue-green rollup catch-up marked done on attempt %d", attempt)
			return
		}
	}()
}

func usageRollupBackfillCompleted(db *sql.DB) bool {
	if db == nil {
		return false
	}
	return projectionMarkerValue(db, usageRollupBackfillMarker) == rollupMarkerDone
}

// maybeFinalizeUsageRollupCatchup is called from maintenance as a backup finalizer.
// It never runs before rollupCatchupEarliest (drain window) so it cannot race old slot.
func maybeFinalizeUsageRollupCatchup(db *sql.DB) {
	if db == nil || usageRollupBackfillCompleted(db) {
		return
	}
	if projectionMarkerValue(db, usageRollupBackfillMarker) != rollupMarkerPending {
		return
	}
	rollupCatchupEarliestMu.Lock()
	earliest := rollupCatchupEarliest
	rollupCatchupEarliestMu.Unlock()
	if earliest.IsZero() || time.Now().Before(earliest) {
		return
	}
	if err := runUsageRollupBackfillAtInitDB(db, getUsageLocation(), rollupMarkerDone); err != nil {
		log.Warnf("usage: maintenance rollup catch-up: %v", err)
		return
	}
	log.Info("usage: maintenance rollup catch-up marked done")
}

// runUsageRollupBackfillAtInitDB absolute-rebuilds rollup from surviving request_logs.
// No-op when marker is already "done" (lifetime history may only live in rollup).
// finalMarker is "pending" (init) or "done" (post-drain catch-up).
func runUsageRollupBackfillAtInitDB(db *sql.DB, loc *time.Location, finalMarker string) error {
	if db == nil {
		return nil
	}
	ensureUsageProjectionMarkerTable(db)
	if finalMarker != rollupMarkerPending && finalMarker != rollupMarkerDone {
		finalMarker = rollupMarkerPending
	}
	// Day keys use process usageLoc (set by InitDB/InitPostgres); keep param for call-site clarity.
	_ = loc

	// Exclusive rebuild: re-check marker under lock so concurrent finalizers cannot
	// double-DELETE after the first writer already marked done and cleanup ran.
	usageProjectionMu.Lock()
	defer usageProjectionMu.Unlock()

	current := projectionMarkerValue(db, usageRollupBackfillMarker)
	if current == rollupMarkerDone {
		return nil
	}
	// pending→pending rebuild is allowed (init retry); pending→done is catch-up.

	// Resolve end_user_id from api_keys after identity tables exist.
	// Map by api_key_id first, then raw secret for legacy empty-id rows.
	keyIDToEndUser := map[string]string{}
	secretToEndUser := map[string]string{}
	secretToKeyID := map[string]string{}
	// end_user_id is UUID on Postgres; cast to text so empty COALESCE works on both drivers.
	keyRows, err := db.Query(`SELECT id, key, COALESCE(CAST(end_user_id AS TEXT), '') FROM api_keys`)
	if err != nil {
		// api_keys missing is fatal for correct ownership; do not write marker.
		return fmt.Errorf("usage: rollup backfill query api_keys: %w", err)
	}
	for keyRows.Next() {
		var id, key, endUser string
		if err := keyRows.Scan(&id, &key, &endUser); err != nil {
			_ = keyRows.Close()
			return fmt.Errorf("usage: rollup backfill scan api_keys: %w", err)
		}
		id = strings.TrimSpace(id)
		key = strings.TrimSpace(key)
		endUser = strings.TrimSpace(endUser)
		if id != "" {
			keyIDToEndUser[id] = endUser
		}
		if key != "" {
			secretToEndUser[key] = endUser
			if id != "" {
				secretToKeyID[key] = id
			}
		}
	}
	if err := keyRows.Err(); err != nil {
		_ = keyRows.Close()
		return fmt.Errorf("usage: rollup backfill api_keys rows: %w", err)
	}
	_ = keyRows.Close()

	type backfillRow struct {
		ev rollupEvent
	}
	events := make([]backfillRow, 0)
	rows, err := db.Query(`
		SELECT tenant_id, COALESCE(api_key_id,''), COALESCE(auth_subject_id,''),
		       COALESCE(model,''), COALESCE(source,''), COALESCE(channel_name,''),
		       failed, streaming, latency_ms, first_token_ms,
		       input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens,
		       cost, timestamp, COALESCE(api_key,'')
		FROM request_logs
		ORDER BY timestamp ASC, id ASC
	`)
	if err != nil {
		// Do not mark done on read failure — next startup must retry.
		return fmt.Errorf("usage: rollup backfill query request_logs: %w", err)
	}
	for rows.Next() {
		var (
			tenantID, apiKeyID, authSubjectID, model, source, channel, ts, apiKey string
			failed, streaming                                                     int
			latencyMs, firstTokenMs                                               int64
			inTok, outTok, reasonTok, cachedTok, totalTok                         int64
			cost                                                                  float64
		)
		if err = rows.Scan(
			&tenantID, &apiKeyID, &authSubjectID, &model, &source, &channel,
			&failed, &streaming, &latencyMs, &firstTokenMs,
			&inTok, &outTok, &reasonTok, &cachedTok, &totalTok,
			&cost, &ts, &apiKey,
		); err != nil {
			_ = rows.Close()
			return fmt.Errorf("usage: rollup backfill scan: %w", err)
		}
		at, ok := parseStoredTimeString(ts)
		if !ok {
			continue
		}
		apiKeyID = strings.TrimSpace(apiKeyID)
		apiKey = strings.TrimSpace(apiKey)
		endUserID := ""
		if apiKeyID != "" {
			endUserID = keyIDToEndUser[apiKeyID]
		}
		if endUserID == "" && apiKey != "" {
			endUserID = secretToEndUser[apiKey]
		}
		if apiKeyID == "" && apiKey != "" {
			apiKeyID = secretToKeyID[apiKey]
		}
		events = append(events, backfillRow{ev: rollupEvent{
			TenantID:      tenantID,
			APIKeyID:      apiKeyID,
			EndUserID:     endUserID,
			AuthSubjectID: authSubjectID,
			Model:         model,
			Source:        source,
			ChannelName:   channel,
			Failed:        failed != 0,
			Streaming:     streaming != 0,
			LatencyMs:     latencyMs,
			FirstTokenMs:  firstTokenMs,
			Tokens: TokenStats{
				InputTokens: inTok, OutputTokens: outTok, ReasoningTokens: reasonTok,
				CachedTokens: cachedTok, TotalTokens: totalTok,
			},
			Cost: cost,
			At:   at,
		}})
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	// Single transaction: wipe incomplete projection + rebuild + marker.
	// Prevents double-count if process dies between commit and marker write.
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("usage: rollup backfill begin: %w", err)
	}
	if _, err = tx.Exec(`DELETE FROM usage_rollup_buckets`); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("usage: rollup backfill clear: %w", err)
	}
	for _, item := range events {
		if err = projectUsageRollupTx(tx, item.ev); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	nowTime := time.Now().UTC()
	now := nowTime.Format(time.RFC3339Nano)
	if _, err = tx.Exec(`
		INSERT INTO usage_projection_markers (marker_key, marker_value, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(marker_key) DO NOTHING
	`, quotaMinuteCoverageStartMarker, nowTime.Truncate(time.Minute).Format(time.RFC3339), now); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("usage: quota minute coverage marker: %w", err)
	}
	if _, err = tx.Exec(`
		INSERT INTO usage_projection_markers (marker_key, marker_value, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(marker_key) DO UPDATE SET
			marker_value = excluded.marker_value,
			updated_at = excluded.updated_at
	`, usageRollupBackfillMarker, finalMarker, now); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("usage: rollup backfill marker: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("usage: rollup backfill commit: %w", err)
	}
	return nil
}

func rollupBucketStarts(at time.Time, loc *time.Location) map[string]string {
	if loc == nil {
		loc = time.Local
	}
	local := at.In(loc)
	return map[string]string{
		rollupBucketMinute:         local.Format("2006-01-02T15:04"),
		rollupBucketQuotaMinuteUTC: at.UTC().Format("2006-01-02T15:04"),
		rollupBucketHour:           local.Format("2006-01-02T15"),
		rollupBucketDay:            local.Format("2006-01-02"),
		rollupBucketLifetime:       rollupLifetimeStart,
	}
}

func projectUsageRollupTx(tx *sql.Tx, ev rollupEvent) error {
	if tx == nil {
		return nil
	}
	ev.TenantID = normalizeTenantID(ev.TenantID)
	ev.APIKeyID = strings.TrimSpace(ev.APIKeyID)
	ev.EndUserID = strings.TrimSpace(ev.EndUserID)
	ev.AuthSubjectID = strings.TrimSpace(ev.AuthSubjectID)
	ev.Model = strings.TrimSpace(ev.Model)
	ev.Source = strings.TrimSpace(ev.Source)
	ev.ChannelName = strings.TrimSpace(ev.ChannelName)
	if ev.At.IsZero() {
		ev.At = time.Now()
	}

	successInc, failureInc := int64(1), int64(0)
	if ev.Failed {
		successInc, failureInc = 0, 1
	}
	streamingInc := int64(0)
	if ev.Streaming {
		streamingInc = 1
	}
	effectiveInput := effectiveInputTokenTotal(ev.Tokens.InputTokens, ev.Tokens.CachedTokens)
	latencyCount := int64(0)
	if ev.LatencyMs > 0 {
		latencyCount = 1
	}
	firstTokenCount := int64(0)
	if ev.FirstTokenMs > 0 {
		firstTokenCount = 1
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	// Read usageLoc without locking: InitDB holds usageDBMu while backfilling.
	loc := usageLoc
	if loc == nil {
		loc = time.Local
	}
	starts := rollupBucketStarts(ev.At, loc)

	const upsertSQL = `
		INSERT INTO usage_rollup_buckets (
			tenant_id, bucket_kind, bucket_start,
			api_key_id, end_user_id, auth_subject_id,
			model, source, channel_name,
			request_count, success_count, failure_count, streaming_count,
			input_tokens, output_tokens, reasoning_tokens, cached_tokens,
			effective_input_tokens, total_tokens, cost_total,
			latency_sum_ms, latency_count, first_token_sum_ms, first_token_count,
			updated_at
		) VALUES (
			?, ?, ?,
			?, ?, ?,
			?, ?, ?,
			1, ?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?,
			?, ?, ?, ?,
			?
		)
		ON CONFLICT(
			tenant_id, bucket_kind, bucket_start,
			api_key_id, end_user_id, auth_subject_id,
			model, source, channel_name
		) DO UPDATE SET
			request_count = usage_rollup_buckets.request_count + 1,
			success_count = usage_rollup_buckets.success_count + excluded.success_count,
			failure_count = usage_rollup_buckets.failure_count + excluded.failure_count,
			streaming_count = usage_rollup_buckets.streaming_count + excluded.streaming_count,
			input_tokens = usage_rollup_buckets.input_tokens + excluded.input_tokens,
			output_tokens = usage_rollup_buckets.output_tokens + excluded.output_tokens,
			reasoning_tokens = usage_rollup_buckets.reasoning_tokens + excluded.reasoning_tokens,
			cached_tokens = usage_rollup_buckets.cached_tokens + excluded.cached_tokens,
			effective_input_tokens = usage_rollup_buckets.effective_input_tokens + excluded.effective_input_tokens,
			total_tokens = usage_rollup_buckets.total_tokens + excluded.total_tokens,
			cost_total = usage_rollup_buckets.cost_total + excluded.cost_total,
			latency_sum_ms = usage_rollup_buckets.latency_sum_ms + excluded.latency_sum_ms,
			latency_count = usage_rollup_buckets.latency_count + excluded.latency_count,
			first_token_sum_ms = usage_rollup_buckets.first_token_sum_ms + excluded.first_token_sum_ms,
			first_token_count = usage_rollup_buckets.first_token_count + excluded.first_token_count,
			updated_at = excluded.updated_at
	`

	coverageStart := ev.At.UTC().Truncate(time.Minute).Format(time.RFC3339)
	if _, err := tx.Exec(`
		INSERT INTO usage_projection_markers (marker_key, marker_value, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(marker_key) DO NOTHING
	`, quotaMinuteCoverageStartMarker, coverageStart, now); err != nil {
		return fmt.Errorf("usage: record quota minute coverage start: %w", err)
	}

	// Fixed order avoids Postgres deadlocks when concurrent writers UPSERT the same key.
	for _, kind := range []string{rollupBucketMinute, rollupBucketQuotaMinuteUTC, rollupBucketHour, rollupBucketDay, rollupBucketLifetime} {
		start := starts[kind]
		if _, err := tx.Exec(upsertSQL,
			ev.TenantID, kind, start,
			ev.APIKeyID, ev.EndUserID, ev.AuthSubjectID,
			ev.Model, ev.Source, ev.ChannelName,
			successInc, failureInc, streamingInc,
			ev.Tokens.InputTokens, ev.Tokens.OutputTokens, ev.Tokens.ReasoningTokens, ev.Tokens.CachedTokens,
			effectiveInput, ev.Tokens.TotalTokens, ev.Cost,
			ev.LatencyMs, latencyCount, ev.FirstTokenMs, firstTokenCount,
			now,
		); err != nil {
			return fmt.Errorf("usage: project rollup %s: %w", kind, err)
		}
	}
	return nil
}

func resolveEndUserIDForKey(apiKey string) string {
	row := GetAPIKey(strings.TrimSpace(apiKey))
	if row == nil {
		return ""
	}
	return strings.TrimSpace(row.EndUserID)
}

// commitLogWithProjections writes tenant rollup and shared subject buckets, then commits.
// Caller must already hold usageProjectionMu.RLock (see insertLogIdentity).
func commitLogWithProjections(tx *sql.Tx, ev rollupEvent) error {
	if err := projectUsageRollupTx(tx, ev); err != nil {
		_ = tx.Rollback()
		return err
	}
	if ev.AuthSubjectID != "" {
		if err := projectAIAccountSubjectUsageTx(tx, ev.AuthSubjectID, ev.Failed, ev.Cost, ev.Tokens.TotalTokens, ev.At); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("project shared auth subject usage: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

// cleanupExpiredUsageRollupBuckets prunes minute/hour/day buckets past retention.
// lifetime buckets are never deleted.
func cleanupExpiredUsageRollupBuckets(db *sql.DB) (int64, error) {
	if db == nil {
		return 0, nil
	}
	loc := usageLoc
	if loc == nil {
		loc = time.Local
	}
	now := time.Now().In(loc)
	cuts := []struct {
		kind string
		from string
	}{
		{rollupBucketMinute, now.Add(-rollupMinuteRetention).Format("2006-01-02T15:04")},
		{rollupBucketQuotaMinuteUTC, time.Now().UTC().Add(-rollupMinuteRetention).Format("2006-01-02T15:04")},
		{rollupBucketHour, now.Add(-rollupHourRetention).Format("2006-01-02T15")},
		{rollupBucketDay, now.Add(-rollupDayRetention).Format("2006-01-02")},
	}
	var deleted int64
	for _, c := range cuts {
		res, err := db.Exec(`
			DELETE FROM usage_rollup_buckets
			WHERE bucket_kind = ? AND bucket_start < ?
		`, c.kind, c.from)
		if err != nil {
			return deleted, fmt.Errorf("usage: prune rollup %s: %w", c.kind, err)
		}
		n, _ := res.RowsAffected()
		deleted += n
	}
	return deleted, nil
}

func FiveHourQuotaProjectionReadyAt(now time.Time) bool {
	db := getReadDB()
	if db == nil {
		return false
	}
	raw := strings.TrimSpace(projectionMarkerValue(db, quotaMinuteCoverageStartMarker))
	if raw == "" {
		return false
	}
	started, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return false
	}
	return !started.After(now.UTC().Truncate(time.Minute).Add(-5 * time.Hour))
}

func FiveHourQuotaProjectionReady() bool {
	return FiveHourQuotaProjectionReadyAt(time.Now())
}
