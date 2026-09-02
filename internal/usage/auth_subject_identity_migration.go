package usage

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// mergedLegacySubjects remembers the legacy ids this process has already dealt
// with. Auth reload fires the binding hook for every credential on every config
// change, and the check below is otherwise a handful of queries per credential
// that can only ever find nothing after the first pass.
var mergedLegacySubjects sync.Map

// legacySubjectMergeMu serialises merges. Two credentials for one account load
// concurrently and would otherwise race to move the same legacy rows.
var legacySubjectMergeMu sync.Mutex

// subjectRewriteTables carry a subject reference whose primary key already
// includes tenant_id (or is a surrogate id), so one account's rows from two
// tenants cannot collide and a plain rewrite is enough.
var subjectRewriteTables = []struct{ table, column string }{
	{"ai_account_tenant_bindings", "auth_subject_id"},
	{"ai_account_status", "auth_subject_id"},
	{"ai_account_subject_quota_points", "auth_subject_id"},
	{"auth_subject_usage_daily", "auth_subject_id"},
	{"auth_subject_quota_cycles", "subject_id"},
	{"auth_file_quota_snapshots", "auth_subject_id"},
	{"auth_file_quota_snapshot_points", "auth_subject_id"},
	{"identity_fingerprints", "auth_subject_id"},
	{"request_logs", "auth_subject_id"},
	{"usage_rollup_buckets", "auth_subject_id"},
}

// subjectAccountKeyTables key fingerprint rows by account_key, which is defined
// to equal the subject id. Their uniqueness includes tenant_id, so the collision
// to resolve is only the one a tenant that held both halves would hit — and the
// key set differs per table, which is what decides whether a row is a duplicate.
var subjectAccountKeyTables = []struct {
	table       string
	uniqueRest  []string
	preferNewer string
}{
	{"identity_fingerprints", []string{"tenant_id", "provider", "profile_key"}, "last_seen_at"},
	{"identity_fingerprint_account_policies", []string{"tenant_id", "provider"}, "updated_at"},
}

// MergeLegacySplitAuthSubject folds rows an account accumulated under an older
// subject id onto the identity it has now.
//
// Two predecessors exist:
//
//  1. Tenant-scoped email / auth_id seeds. Every seed except auth_index used to
//     carry tenant_id, so one upstream account became a separate subject in each
//     tenant. Usage sat on one half while the quota bar described the whole
//     account — a tenant that had not called it yet showed zeros next to 87%.
//  2. Codex account_id seeds. Distinguishing Team members required hashing
//     chatgpt_user_id too, which minted a new subject for personal Pro accounts
//     that already had a unique account_id. Bindings moved; history did not.
//     The card then counted hours of traffic against a WHAM bar for the real
//     week. Docker and native upgrades hit this the same way — it is an identity
//     change, not a SQLite leftover.
//
// Called from the binding hook, so it runs with the authoritative auth in hand
// rather than trying to reconstruct seeds from hashes on disk. Native restarts
// and Docker image upgrades both load every credential once, which is enough
// to repair a split that already happened.
func MergeLegacySplitAuthSubject(auth *coreauth.Auth, identity *AuthSubjectIdentity) error {
	if auth == nil || identity == nil || strings.TrimSpace(identity.ID) == "" {
		return nil
	}
	if getDB() == nil {
		return nil
	}
	var firstErr error
	for _, legacyID := range predecessorAuthSubjectIDs(auth, identity) {
		if err := mergeLegacyAuthSubjectIfPresent(legacyID, identity); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func predecessorAuthSubjectIDs(auth *coreauth.Auth, identity *AuthSubjectIdentity) []string {
	if identity == nil {
		return nil
	}
	seen := map[string]struct{}{strings.TrimSpace(identity.ID): {}}
	out := make([]string, 0, 2)
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, exists := seen[id]; exists {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	add(LegacyTenantScopedAuthSubjectID(auth))
	add(LegacyAccountIDAuthSubjectID(auth))
	return out
}

func mergeLegacyAuthSubjectIfPresent(legacyID string, identity *AuthSubjectIdentity) error {
	if _, seen := mergedLegacySubjects.Load(legacyID); seen {
		return nil
	}
	db := getDB()
	if db == nil {
		return nil
	}

	legacySubjectMergeMu.Lock()
	defer legacySubjectMergeMu.Unlock()
	if _, seen := mergedLegacySubjects.Load(legacyID); seen {
		return nil
	}

	present, err := legacyAuthSubjectHasRows(db, legacyID)
	if err != nil {
		return err
	}
	if !present {
		mergedLegacySubjects.Store(legacyID, struct{}{})
		return nil
	}
	skip, err := skipCrowdedAccountIDMerge(db, legacyID, identity)
	if err != nil {
		return err
	}
	if skip {
		// Several credentials still share the old account_id subject — the Team
		// collision the user-id seed was added to stop. Folding that pool onto
		// this user would also rewrite the others' bindings onto this subject.
		mergedLegacySubjects.Store(legacyID, struct{}{})
		return nil
	}

	if err := mergeLegacyAuthSubjectDB(db, legacyID, identity); err != nil {
		return err
	}
	mergedLegacySubjects.Store(legacyID, struct{}{})
	return nil
}

// skipCrowdedAccountIDMerge reports whether the old account_id subject still has
// more than one active binding. Personal Pro is 0 (already rebound after the
// seed change) or 1 (first upgrade, merge runs before the binding upsert).
func skipCrowdedAccountIDMerge(db *sql.DB, legacyID string, identity *AuthSubjectIdentity) (bool, error) {
	if identity == nil || identity.SeedKind != "account_user_id" {
		return false, nil
	}
	var n int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM ai_account_tenant_bindings
		WHERE auth_subject_id = ? AND binding_state = 'active'
	`, legacyID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("usage: count account_id subject bindings: %w", err)
	}
	return n > 1, nil
}

// legacyAuthSubjectHasRows is the cheap guard on the reload path: an account
// that was never split, or was migrated on an earlier boot, answers false here
// and never opens a transaction.
func legacyAuthSubjectHasRows(db *sql.DB, legacyID string) (bool, error) {
	for _, probe := range []string{
		`SELECT 1 FROM ai_account_subjects WHERE auth_subject_id = ? LIMIT 1`,
		`SELECT 1 FROM ai_account_subject_usage_buckets WHERE auth_subject_id = ? LIMIT 1`,
		`SELECT 1 FROM ai_account_subject_status WHERE auth_subject_id = ? LIMIT 1`,
	} {
		var found int
		err := db.QueryRow(probe, legacyID).Scan(&found)
		if err == nil {
			return true, nil
		}
		if err != sql.ErrNoRows {
			return false, fmt.Errorf("usage: probe legacy auth subject: %w", err)
		}
	}
	return false, nil
}

func mergeLegacyAuthSubjectDB(db *sql.DB, legacyID string, identity *AuthSubjectIdentity) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("usage: begin legacy auth subject merge: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The shared subject row must exist first: four tables carry a foreign key to
	// it, so nothing may point at an id that is not there yet.
	if err := ensureAuthSubjectRowTx(tx, legacyID, identity); err != nil {
		return err
	}
	if err := mergeLegacySubjectUsageBucketsTx(tx, legacyID, identity.ID); err != nil {
		return err
	}
	if err := mergeLegacySubjectStatusTx(tx, legacyID, identity.ID); err != nil {
		return err
	}
	if err := mergeLegacySubjectQuotaCyclesTx(tx, legacyID, identity.ID); err != nil {
		return err
	}
	for _, target := range subjectRewriteTables {
		if _, err := tx.Exec(
			`UPDATE `+target.table+` SET `+target.column+` = ? WHERE `+target.column+` = ?`,
			identity.ID, legacyID,
		); err != nil {
			return fmt.Errorf("usage: rewrite %s.%s: %w", target.table, target.column, err)
		}
	}
	for _, target := range subjectAccountKeyTables {
		if err := mergeSubjectAccountKeyRowsTx(tx, target.table, target.uniqueRest, target.preferNewer, legacyID, identity.ID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM ai_account_subjects WHERE auth_subject_id = ?`, legacyID); err != nil {
		return fmt.Errorf("usage: drop legacy auth subject: %w", err)
	}

	// Two tenants anchored their halves of this account independently, so the
	// merged cycle buckets can be keyed to different starts. Fold them now rather
	// than leaving the account reading zero until the period rolls.
	if err := realignAuthSubjectCycleBucketsTx(tx, identity.ID); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("usage: commit legacy auth subject merge: %w", err)
	}
	// The projection keys buckets off the cycle cache, which may still hold the
	// pre-merge anchor for either half.
	resetAIAccountSubjectCycleCache()
	return nil
}

// ensureAuthSubjectRowTx creates the shared subject row, seeding its history
// markers from the legacy row so a merged account does not look newly observed.
func ensureAuthSubjectRowTx(tx *sql.Tx, legacyID string, identity *AuthSubjectIdentity) error {
	// Use bool, not integer 0/1: Postgres share_eligible is BOOLEAN and pgx
	// cannot encode a Go int into OID 16. That failure aborted every merge on
	// a Postgres host — including Docker upgrades — while SQLite had accepted
	// the integer. UpsertAIAccountSubject already writes a bool; this path
	// has to match or the repair never lands.
	if _, err := tx.Exec(`
		INSERT INTO ai_account_subjects (
			auth_subject_id, provider, subject_scope, seed_kind, seed_hash, share_eligible,
			usage_projected_since, usage_history_complete, created_at, updated_at
		)
		SELECT ?, ?, ?, ?, ?, ?, usage_projected_since, usage_history_complete, created_at, updated_at
		FROM ai_account_subjects WHERE auth_subject_id = ?
		ON CONFLICT(auth_subject_id) DO UPDATE SET
			usage_projected_since = CASE
				WHEN excluded.usage_projected_since IS NOT NULL
				 AND (ai_account_subjects.usage_projected_since IS NULL
				      OR excluded.usage_projected_since < ai_account_subjects.usage_projected_since)
				THEN excluded.usage_projected_since
				ELSE ai_account_subjects.usage_projected_since END,
			created_at = CASE WHEN excluded.created_at < ai_account_subjects.created_at
				THEN excluded.created_at ELSE ai_account_subjects.created_at END,
			updated_at = CASE WHEN excluded.updated_at > ai_account_subjects.updated_at
				THEN excluded.updated_at ELSE ai_account_subjects.updated_at END
	`, identity.ID, identity.Provider, identity.SubjectScope, identity.SeedKind, identity.SeedHash,
		identity.ShareEligible, legacyID); err != nil {
		return fmt.Errorf("usage: ensure shared auth subject: %w", err)
	}
	return nil
}

// mergeLegacySubjectUsageBucketsTx adds the legacy totals into the shared rows.
// Counters sum, the first event keeps the earliest sighting and updated_at the
// latest, so the merged account reads as one continuous history.
func mergeLegacySubjectUsageBucketsTx(tx *sql.Tx, legacyID, subjectID string) error {
	if _, err := tx.Exec(`
		INSERT INTO ai_account_subject_usage_buckets (
			auth_subject_id, bucket_kind, bucket_start, request_count, success_count,
			failure_count, cost_total, total_tokens, first_event_at, updated_at
		)
		SELECT ?, bucket_kind, bucket_start, request_count, success_count,
			failure_count, cost_total, total_tokens, first_event_at, updated_at
		FROM ai_account_subject_usage_buckets WHERE auth_subject_id = ?
		ON CONFLICT(auth_subject_id, bucket_kind, bucket_start) DO UPDATE SET
			request_count = ai_account_subject_usage_buckets.request_count + excluded.request_count,
			success_count = ai_account_subject_usage_buckets.success_count + excluded.success_count,
			failure_count = ai_account_subject_usage_buckets.failure_count + excluded.failure_count,
			cost_total = ai_account_subject_usage_buckets.cost_total + excluded.cost_total,
			total_tokens = ai_account_subject_usage_buckets.total_tokens + excluded.total_tokens,
			first_event_at = CASE WHEN excluded.first_event_at < ai_account_subject_usage_buckets.first_event_at
				THEN excluded.first_event_at ELSE ai_account_subject_usage_buckets.first_event_at END,
			updated_at = CASE WHEN excluded.updated_at > ai_account_subject_usage_buckets.updated_at
				THEN excluded.updated_at ELSE ai_account_subject_usage_buckets.updated_at END
	`, subjectID, legacyID); err != nil {
		return fmt.Errorf("usage: merge legacy subject usage buckets: %w", err)
	}
	if _, err := tx.Exec(
		`DELETE FROM ai_account_subject_usage_buckets WHERE auth_subject_id = ?`, legacyID,
	); err != nil {
		return fmt.Errorf("usage: drop legacy subject usage buckets: %w", err)
	}
	return nil
}

// mergeSubjectAccountKeyRowsTx re-keys fingerprint rows onto the shared subject.
//
// A tenant only collides with itself here — it held both halves of the account
// and so has a row under each key. The newer row wins, since these describe the
// client identity last presented for the account, and the loser is dropped
// rather than being rewritten into a duplicate key.
func mergeSubjectAccountKeyRowsTx(tx *sql.Tx, table string, uniqueRest []string, preferNewer, legacyID, subjectID string) error {
	match := make([]string, 0, len(uniqueRest))
	for _, column := range uniqueRest {
		match = append(match, "other."+column+" = "+table+"."+column)
	}
	matched := strings.Join(match, " AND ")

	// Drop whichever side is older wherever both exist for the same tenant row.
	for _, drop := range []struct{ victim, rival string }{
		{subjectID, legacyID},
		{legacyID, subjectID},
	} {
		comparison := ">"
		if drop.victim == legacyID {
			// The shared row survives ties: re-keying the legacy row on top of an
			// equally fresh shared row would only trade one for an identical other.
			comparison = ">="
		}
		if _, err := tx.Exec(
			`DELETE FROM `+table+` WHERE account_key = ? AND EXISTS (
				SELECT 1 FROM `+table+` other
				WHERE other.account_key = ? AND `+matched+`
				  AND other.`+preferNewer+` `+comparison+` `+table+`.`+preferNewer+`
			)`, drop.victim, drop.rival,
		); err != nil {
			return fmt.Errorf("usage: drop duplicate %s row: %w", table, err)
		}
	}
	if _, err := tx.Exec(
		`UPDATE `+table+` SET account_key = ? WHERE account_key = ?`, subjectID, legacyID,
	); err != nil {
		return fmt.Errorf("usage: rewrite %s.account_key: %w", table, err)
	}
	return nil
}

// mergeLegacySubjectStatusTx keeps whichever half was probed last. Status is a
// snapshot of the upstream account, not something to add up.
func mergeLegacySubjectStatusTx(tx *sql.Tx, legacyID, subjectID string) error {
	if _, err := tx.Exec(`
		DELETE FROM ai_account_subject_status
		WHERE auth_subject_id = ? AND EXISTS (
			SELECT 1 FROM ai_account_subject_status legacy
			WHERE legacy.auth_subject_id = ?
			  AND legacy.updated_at > ai_account_subject_status.updated_at
		)`, subjectID, legacyID); err != nil {
		return fmt.Errorf("usage: drop stale shared subject status: %w", err)
	}
	if _, err := tx.Exec(`
		DELETE FROM ai_account_subject_status
		WHERE auth_subject_id = ? AND EXISTS (
			SELECT 1 FROM ai_account_subject_status shared WHERE shared.auth_subject_id = ?
		)`, legacyID, subjectID); err != nil {
		return fmt.Errorf("usage: drop stale legacy subject status: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE ai_account_subject_status SET auth_subject_id = ? WHERE auth_subject_id = ?`,
		subjectID, legacyID,
	); err != nil {
		return fmt.Errorf("usage: rewrite legacy subject status: %w", err)
	}
	return nil
}

// mergeLegacySubjectQuotaCyclesTx keeps the most recently verified anchor per
// quota key — both halves describe the same upstream window.
func mergeLegacySubjectQuotaCyclesTx(tx *sql.Tx, legacyID, subjectID string) error {
	if _, err := tx.Exec(`
		DELETE FROM ai_account_subject_quota_cycles
		WHERE auth_subject_id = ? AND EXISTS (
			SELECT 1 FROM ai_account_subject_quota_cycles legacy
			WHERE legacy.auth_subject_id = ?
			  AND legacy.quota_key = ai_account_subject_quota_cycles.quota_key
			  AND legacy.last_verified_at > ai_account_subject_quota_cycles.last_verified_at
		)`, subjectID, legacyID); err != nil {
		return fmt.Errorf("usage: drop stale shared quota cycles: %w", err)
	}
	if _, err := tx.Exec(`
		DELETE FROM ai_account_subject_quota_cycles
		WHERE auth_subject_id = ? AND EXISTS (
			SELECT 1 FROM ai_account_subject_quota_cycles shared
			WHERE shared.auth_subject_id = ?
			  AND shared.quota_key = ai_account_subject_quota_cycles.quota_key
		)`, legacyID, subjectID); err != nil {
		return fmt.Errorf("usage: drop stale legacy quota cycles: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE ai_account_subject_quota_cycles SET auth_subject_id = ? WHERE auth_subject_id = ?`,
		subjectID, legacyID,
	); err != nil {
		return fmt.Errorf("usage: rewrite legacy quota cycles: %w", err)
	}
	return nil
}
