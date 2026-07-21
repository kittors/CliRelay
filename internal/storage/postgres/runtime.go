package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres/compatdriver"
)

const (
	driverName           = "pgxq"
	runtimeDBPingTimeout = 30 * time.Second
)

type Migration struct {
	Version string
	SQL     string
}

func OpenRuntimeDB(ctx context.Context, cfg config.PostgresConfig) (*sql.DB, error) {
	return openRuntimeDB(ctx, cfg, 0)
}

// OpenRuntimeDBWithMigrationTimeout opens the runtime database with separate
// budgets for the connection ping and migration phases.
func OpenRuntimeDBWithMigrationTimeout(ctx context.Context, cfg config.PostgresConfig, migrationTimeout time.Duration) (*sql.DB, error) {
	if migrationTimeout <= 0 {
		return nil, errors.New("postgres: migration timeout must be greater than zero")
	}
	return openRuntimeDB(ctx, cfg, migrationTimeout)
}

func openRuntimeDB(ctx context.Context, cfg config.PostgresConfig, migrationTimeout time.Duration) (*sql.DB, error) {
	dsn := strings.TrimSpace(cfg.DSN)
	if dsn == "" {
		return nil, errors.New("postgres dsn is required")
	}
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: open: %w", err)
	}
	if cfg.MaxOpenConns > 0 {
		db.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	if cfg.MaxIdleConns > 0 {
		db.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	db.SetConnMaxLifetime(30 * time.Minute)

	pingCtx := ctx
	cancelPing := func() {}
	if migrationTimeout > 0 {
		pingCtx, cancelPing = context.WithTimeout(ctx, runtimeDBPingTimeout)
	}
	if err := db.PingContext(pingCtx); err != nil {
		cancelPing()
		_ = db.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	cancelPing()

	migrationCtx := ctx
	cancelMigrations := func() {}
	if migrationTimeout > 0 {
		migrationCtx, cancelMigrations = context.WithTimeout(ctx, migrationTimeout)
	}
	defer cancelMigrations()
	if err := ApplyMigrations(migrationCtx, db, RuntimeMigrations()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func ApplyMigrations(ctx context.Context, db *sql.DB, migrations []Migration) error {
	if db == nil {
		return errors.New("postgres: db is nil")
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			checksum TEXT NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			duration_ms BIGINT NOT NULL DEFAULT 0,
			dirty BOOLEAN NOT NULL DEFAULT false
		)
	`); err != nil {
		return fmt.Errorf("postgres: create schema_migrations: %w", err)
	}
	for _, migration := range migrations {
		if err := applyMigration(ctx, db, migration); err != nil {
			return err
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, migration Migration) error {
	version := strings.TrimSpace(migration.Version)
	if version == "" {
		return errors.New("postgres: migration version is required")
	}
	checksum := migrationChecksum(migration.SQL)
	var existingChecksum string
	var dirty bool
	err := db.QueryRowContext(ctx, `SELECT checksum, dirty FROM schema_migrations WHERE version = ?`, version).Scan(&existingChecksum, &dirty)
	switch {
	case err == nil && dirty:
		return fmt.Errorf("postgres: migration %s is dirty; compare the database schema with this migration's SQL and confirm whether the SQL committed before changing schema_migrations; if it did not commit, apply the SQL before marking the row clean; do not only clear dirty, and do not automatically replay SQL while the commit state is uncertain", version)
	case err == nil && existingChecksum != checksum:
		return fmt.Errorf("postgres: migration %s checksum mismatch", version)
	case err == nil:
		return nil
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("postgres: read migration %s: %w", version, err)
	}

	started := time.Now()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO schema_migrations (version, checksum, applied_at, dirty)
		VALUES (?, ?, now(), true)
	`, version, checksum); err != nil {
		return fmt.Errorf("postgres: mark migration %s dirty: %w", version, err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgres: begin migration %s: %w", version, err)
	}
	if _, err := tx.ExecContext(ctx, migration.SQL); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("postgres: apply migration %s: %w", version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("postgres: commit migration %s: %w", version, err)
	}
	duration := time.Since(started).Milliseconds()
	if _, err := db.ExecContext(ctx, `
		UPDATE schema_migrations
		   SET dirty = false, duration_ms = ?, applied_at = now()
		 WHERE version = ?
	`, duration, version); err != nil {
		return fmt.Errorf("postgres: mark migration %s clean: %w", version, err)
	}
	return nil
}

func migrationChecksum(sqlText string) string {
	sum := sha256.Sum256([]byte(sqlText))
	return fmt.Sprintf("%x", sum[:])
}
