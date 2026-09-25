package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/configsync"
	log "github.com/sirupsen/logrus"
)

// GetWithVersion returns the stored routing config and its row version, or
// (nil, 0) when the tenant has none.
func (s Store) GetWithVersion() (*config.RoutingConfig, int64) {
	if s.db == nil {
		return nil, 0
	}
	var (
		payload string
		version int64
	)
	if err := s.db.QueryRow(`SELECT payload, version FROM routing_config WHERE tenant_id = ? AND id = 1`, s.tenantID).Scan(&payload, &version); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Warnf("sqlite/routing: load routing_config: %v", err)
		}
		return nil, 0
	}
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return nil, version
	}
	var cfg config.RoutingConfig
	if err := json.Unmarshal([]byte(payload), &cfg); err != nil {
		log.Warnf("sqlite/routing: decode routing_config: %v", err)
		return nil, version
	}
	normalized := normalize(cfg)
	return &normalized, version
}

// CompareAndSwap stores cfg if the stored version still equals expected (0:
// no row yet, configsync.AnyVersion: unchecked) and announces the change in
// the same transaction. It returns the new version, or a
// *configsync.ConflictError.
func (s Store) CompareAndSwap(ctx context.Context, cfg config.RoutingConfig, expected int64) (int64, error) {
	if s.db == nil {
		return 0, fmt.Errorf("sqlite/routing: database not initialised")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	payload, err := json.Marshal(normalize(cfg))
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var version int64
	switch {
	case expected < 0:
		err = tx.QueryRowContext(ctx, `INSERT INTO routing_config (tenant_id, id, payload, updated_at, version) VALUES (?, 1, ?, ?, 1)
			ON CONFLICT (tenant_id, id) DO UPDATE SET payload = excluded.payload, updated_at = excluded.updated_at,
				version = routing_config.version + 1
			RETURNING version`, s.tenantID, string(payload), now).Scan(&version)
	case expected == 0:
		err = tx.QueryRowContext(ctx, `INSERT INTO routing_config (tenant_id, id, payload, updated_at, version) VALUES (?, 1, ?, ?, 1)
			ON CONFLICT (tenant_id, id) DO NOTHING
			RETURNING version`, s.tenantID, string(payload), now).Scan(&version)
	default:
		err = tx.QueryRowContext(ctx, `UPDATE routing_config SET payload = ?, updated_at = ?, version = version + 1
			WHERE tenant_id = ? AND id = 1 AND version = ?
			RETURNING version`, string(payload), now, s.tenantID, expected).Scan(&version)
	}
	if errors.Is(err, sql.ErrNoRows) {
		var current int64
		if scanErr := tx.QueryRowContext(ctx, `SELECT version FROM routing_config WHERE tenant_id = ? AND id = 1`, s.tenantID).Scan(&current); scanErr != nil && !errors.Is(scanErr, sql.ErrNoRows) {
			return 0, scanErr
		}
		return 0, &configsync.ConflictError{Domain: configsync.DomainRouting, TenantID: s.tenantID, Expected: expected, Current: current}
	}
	if err != nil {
		return 0, fmt.Errorf("sqlite/routing: write routing_config: %w", err)
	}
	if err = configsync.CommitTx(ctx, tx, configsync.KeyEvent(configsync.DomainRouting, s.tenantID, "", version)); err != nil {
		return 0, err
	}
	return version, nil
}

// Update applies mutate to the stored routing config (or fallback when the
// tenant has none) and stores the result against the version it was read at,
// re-reading and retrying when another writer got in between. It is for
// derived writes such as renaming a channel everywhere it is referenced.
// It reports whether mutate changed anything.
func (s Store) Update(ctx context.Context, fallback config.RoutingConfig, mutate func(*config.RoutingConfig) bool) (config.RoutingConfig, bool, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		stored, version := s.GetWithVersion()
		current := fallback
		if stored != nil {
			current = *stored
		}
		if !mutate(&current) {
			return current, false, nil
		}
		if _, err := s.CompareAndSwap(ctx, current, version); err != nil {
			lastErr = err
			if errors.Is(err, configsync.ErrVersionConflict) {
				continue
			}
			return current, false, err
		}
		return normalize(current), true, nil
	}
	return fallback, false, lastErr
}

// ensureVersionColumn adds routing_config.version to SQLite databases created
// before it existed; PostgreSQL gets it from a versioned migration.
func ensureVersionColumn(db *sql.DB) {
	rows, err := db.Query("PRAGMA table_info(routing_config)")
	if err != nil {
		return
	}
	hasVersion, sawColumn := false, false
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var def sql.NullString
		if rows.Scan(&cid, &name, &typ, &notNull, &def, &pk) != nil {
			_ = rows.Close()
			return
		}
		sawColumn = true
		hasVersion = hasVersion || name == "version"
	}
	_ = rows.Close()
	if !sawColumn || hasVersion {
		return
	}
	if _, err := db.Exec("ALTER TABLE routing_config ADD COLUMN version INTEGER NOT NULL DEFAULT 1"); err != nil {
		log.Warnf("sqlite/routing: add routing_config.version: %v", err)
	}
}
