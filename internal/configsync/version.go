package configsync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	log "github.com/sirupsen/logrus"
)

// ErrVersionConflict marks a write that was based on a version another writer
// has already replaced. Management handlers answer it with 409 so the client
// reloads before trying again.
var ErrVersionConflict = errors.New("configuration was modified by another node or administrator; refresh and retry")

// ConflictError describes a failed compare-and-swap.
type ConflictError struct {
	Domain   string
	TenantID string
	Key      string
	Expected int64
	Current  int64
}

func (e *ConflictError) Error() string {
	target := e.Domain
	if e.Key != "" {
		target += "/" + e.Key
	}
	return fmt.Sprintf("%s: %s expected version %d, current version %d", ErrVersionConflict.Error(), target, e.Expected, e.Current)
}

// Is makes errors.Is(err, ErrVersionConflict) match.
func (e *ConflictError) Is(target error) bool { return target == ErrVersionConflict }

// AnyVersion disables the version check of a compare-and-swap: the write
// applies to whatever version is current. Clients that predate versioning
// send no version and get this, which is the old last-writer-wins behaviour.
const AnyVersion int64 = -1

// createConfigVersionsTableSQL mirrors the PostgreSQL migration for SQLite
// test databases. updated_at is TEXT like runtime_settings.updated_at.
const createConfigVersionsTableSQL = `
CREATE TABLE IF NOT EXISTS config_versions (
  tenant_id  TEXT NOT NULL,
  domain     TEXT NOT NULL,
  version    BIGINT NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (tenant_id, domain)
);
`

// InitTables creates config_versions when a migration did not (SQLite test
// databases). On PostgreSQL the versioned migration owns the table and this
// statement is a no-op.
func InitTables(db *sql.DB) {
	if db == nil {
		return
	}
	if _, err := db.Exec(createConfigVersionsTableSQL); err != nil {
		log.Errorf("configsync: create config_versions table: %v", err)
	}
}

// BumpTx increments the version of the (domain, tenant) collection inside tx
// and returns the new version. It is meant to be the first statement of a
// collection write: the row lock it takes serialises concurrent writers of the
// same collection until tx ends, so two full replacements can no longer
// interleave their deletes and inserts.
//
// expected < 0 (AnyVersion) skips the check. Otherwise expected must equal the
// current version, 0 meaning "never written"; a mismatch returns a
// *ConflictError and tx must be rolled back.
func BumpTx(ctx context.Context, tx *sql.Tx, domain, tenantID string, expected int64) (int64, error) {
	if tx == nil {
		return 0, errors.New("configsync: nil transaction")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	tenantID = NormalizeTenantID(tenantID)
	domain = strings.TrimSpace(domain)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var (
		version int64
		err     error
	)
	switch {
	case expected < 0:
		err = tx.QueryRowContext(ctx, `INSERT INTO config_versions (tenant_id, domain, version, updated_at) VALUES (?, ?, 1, ?)
			ON CONFLICT (tenant_id, domain) DO UPDATE SET version = config_versions.version + 1, updated_at = excluded.updated_at
			RETURNING version`, tenantID, domain, now).Scan(&version)
	case expected == 0:
		err = tx.QueryRowContext(ctx, `INSERT INTO config_versions (tenant_id, domain, version, updated_at) VALUES (?, ?, 1, ?)
			ON CONFLICT (tenant_id, domain) DO NOTHING
			RETURNING version`, tenantID, domain, now).Scan(&version)
	default:
		err = tx.QueryRowContext(ctx, `UPDATE config_versions SET version = version + 1, updated_at = ?
			WHERE tenant_id = ? AND domain = ? AND version = ?
			RETURNING version`, now, tenantID, domain, expected).Scan(&version)
	}
	if errors.Is(err, sql.ErrNoRows) {
		current, currentErr := collectionVersion(ctx, tx, domain, tenantID)
		if currentErr != nil {
			return 0, currentErr
		}
		return 0, &ConflictError{Domain: domain, TenantID: tenantID, Expected: expected, Current: current}
	}
	if err != nil {
		return 0, fmt.Errorf("configsync: bump %s version: %w", domain, err)
	}
	return version, nil
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func collectionVersion(ctx context.Context, q rowQuerier, domain, tenantID string) (int64, error) {
	var version int64
	err := q.QueryRowContext(ctx, `SELECT version FROM config_versions WHERE tenant_id = ? AND domain = ?`, NormalizeTenantID(tenantID), domain).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("configsync: read %s version: %w", domain, err)
	}
	return version, nil
}

// CollectionVersion returns the current version of a (domain, tenant)
// collection, 0 when it was never written or cannot be read.
func CollectionVersion(ctx context.Context, db *sql.DB, domain, tenantID string) int64 {
	if db == nil {
		return 0
	}
	if ctx == nil {
		ctx = context.Background()
	}
	version, err := collectionVersion(ctx, db, strings.TrimSpace(domain), tenantID)
	if err != nil {
		log.WithError(err).Debug("configsync: collection version unavailable")
		return 0
	}
	return version
}

// WriteCollection runs fn inside a transaction that first bumps the (domain,
// tenant) collection version (checked against expected unless it is
// AnyVersion) and then announces the write with the new version, together
// with any extra events. It returns the new version.
func WriteCollection(ctx context.Context, db *sql.DB, domain, tenantID string, expected int64, fn func(*sql.Tx) error, extra ...cluster.ConfigEvent) (int64, error) {
	if db == nil {
		return 0, errors.New("configsync: database not initialised")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	version, err := BumpTx(ctx, tx, domain, tenantID, expected)
	if err != nil {
		return 0, err
	}
	if fn != nil {
		if err = fn(tx); err != nil {
			return 0, err
		}
	}
	events := append([]cluster.ConfigEvent{KeyEvent(domain, tenantID, "", version)}, extra...)
	if err = CommitTx(ctx, tx, events...); err != nil {
		return 0, err
	}
	return version, nil
}

// BumpAndCommit ends a row-level write to a whole-replace collection: it bumps
// the collection version, so a replacement computed from an older list is
// detected, and commits tx together with the announcement. A database without
// config_versions (a SQLite test schema) still commits the write.
func BumpAndCommit(ctx context.Context, tx *sql.Tx, domain, tenantID string, extra ...cluster.ConfigEvent) error {
	if ctx == nil {
		ctx = context.Background()
	}
	version, err := BumpTx(ctx, tx, domain, tenantID, AnyVersion)
	if err != nil && !isMissingTable(err) {
		return err
	}
	return CommitTx(ctx, tx, append([]cluster.ConfigEvent{KeyEvent(domain, tenantID, "", version)}, extra...)...)
}
