package enduser

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// isUnsupportedAdvisoryLockError is true only for engines that do not implement
// pg_advisory_xact_lock (e.g. SQLite). Permission/execution failures on Postgres
// must fail closed and must not match this helper.
func isUnsupportedAdvisoryLockError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	// Explicit "function does not exist" / "no such function" patterns only.
	return strings.Contains(msg, "no such function") ||
		strings.Contains(msg, "function pg_advisory_xact_lock") && strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "undefined function") && strings.Contains(msg, "pg_advisory")
}

// migrationDefaultPassword is the known initial password for one-shot key→user backfill.
// Admins may change it later from the management UI.
const migrationDefaultPassword = "password123"

// BackfillFromAPIKeys is a one-shot migration: runs only when end_user_backfill_state is empty.
// After success, marks done so deleted users / unbound keys are never re-created.
// Username = pinyin/slug of key name; password = password123 (no forced change).
func (s *Service) BackfillFromAPIKeys(ctx context.Context) (int, error) {
	if s == nil || s.db == nil {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// Serialize concurrent boots. Fail closed on Postgres; only ignore engines
	// that literally do not implement advisory locks (SQLite/compat).
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(74201501)`); err != nil {
		if !isUnsupportedAdvisoryLockError(err) {
			return 0, fmt.Errorf("enduser backfill lock: %w", err)
		}
	}

	var done int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM end_user_backfill_state WHERE id = 1`).Scan(&done); err != nil {
		// Table missing is a hard error after migrations; do not mark complete blindly.
		return 0, err
	}
	if done > 0 {
		return 0, nil
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT key, id, name, tenant_id FROM api_keys
		WHERE end_user_id IS NULL
		ORDER BY tenant_id, created_at ASC
	`)
	if err != nil {
		return 0, err
	}
	type row struct {
		key, id, name, tenantID string
	}
	items := make([]row, 0)
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.key, &r.id, &r.name, &r.tenantID); err != nil {
			_ = rows.Close()
			return 0, err
		}
		items = append(items, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	_ = rows.Close()

	passwordHash, err := HashPassword(migrationDefaultPassword)
	if err != nil {
		return 0, err
	}
	created := 0
	for _, item := range items {
		base := UsernameFromDisplay(item.name)
		uname, err := s.uniqueUsername(ctx, tx, item.tenantID, base)
		if err != nil {
			return created, err
		}
		display := strings.TrimSpace(item.name)
		if display == "" {
			display = uname
		}
		userID := uuid.NewString()
		if _, err = tx.ExecContext(ctx, `
			INSERT INTO end_users (id, tenant_id, username, username_normalized, display_name, password_hash, must_change_password)
			VALUES (?, ?, ?, ?, ?, ?, false)
		`, userID, item.tenantID, uname, NormalizeUsername(uname), display, passwordHash); err != nil {
			return created, err
		}
		if _, err = tx.ExecContext(ctx, `
			UPDATE api_keys SET end_user_id = ?, is_default = true WHERE key = ? AND tenant_id = ? AND end_user_id IS NULL
		`, userID, item.key, item.tenantID); err != nil {
			return created, err
		}
		created++
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO end_user_backfill_state (id, done_at) VALUES (1, now()) ON CONFLICT (id) DO NOTHING`); err != nil {
		return created, err
	}
	if err = tx.Commit(); err != nil {
		return created, err
	}
	return created, nil
}
