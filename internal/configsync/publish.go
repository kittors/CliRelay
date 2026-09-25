package configsync

import (
	"context"
	"database/sql"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
)

// PublishFunc queues one configuration event inside tx.
type PublishFunc func(ctx context.Context, tx *sql.Tx, ev cluster.ConfigEvent) error

var publishOverride atomic.Pointer[PublishFunc]

// SetPublisherForTest routes every event through fn instead of the process
// coordinator, so a test can observe exactly what a write path announces and
// in which transaction. It returns a function that restores the default.
func SetPublisherForTest(fn PublishFunc) func() {
	previous := publishOverride.Swap(&fn)
	return func() { publishOverride.Store(previous) }
}

// Active reports whether writes are announced at all: in cluster mode, or
// while a test publisher is installed. Single-node deployments skip the extra
// transaction a publish needs, so their write paths stay exactly as they were.
func Active() bool {
	return publishOverride.Load() != nil || cluster.Default().Enabled()
}

// PublishTx queues ev inside tx. The event reaches other nodes only if tx
// commits, so a rolled-back write is never announced. A nil tx publishes
// immediately.
func PublishTx(ctx context.Context, tx *sql.Tx, ev cluster.ConfigEvent) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ev.TenantID = NormalizeTenantID(ev.TenantID)
	if override := publishOverride.Load(); override != nil {
		return (*override)(ctx, tx, ev)
	}
	return cluster.Default().PublishTx(ctx, tx, cluster.TopicConfig, ev)
}

// CommitTx queues events in tx and then commits it. Queueing first is the
// point: the announcement becomes part of the transaction, so a failed commit
// announces nothing.
func CommitTx(ctx context.Context, tx *sql.Tx, events ...cluster.ConfigEvent) error {
	if Active() {
		for _, ev := range events {
			if err := PublishTx(ctx, tx, ev); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// WithTx runs fn in a transaction on db and commits it together with events.
// fn must only use tx: SQLite test databases have a single connection, so a
// query on db inside fn would deadlock.
func WithTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error, events ...cluster.ConfigEvent) error {
	if ctx == nil {
		ctx = context.Background()
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = fn(tx); err != nil {
		return err
	}
	return CommitTx(ctx, tx, events...)
}

// Exec runs one statement and announces events with it. When nothing is
// announced it is a plain ExecContext; otherwise the statement and the events
// share a transaction.
func Exec(ctx context.Context, db *sql.DB, events []cluster.ConfigEvent, query string, args ...any) (sql.Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !Active() || len(events) == 0 {
		return db.ExecContext(ctx, query, args...)
	}
	var result sql.Result
	err := WithTx(ctx, db, func(tx *sql.Tx) error {
		var execErr error
		result, execErr = tx.ExecContext(ctx, query, args...)
		return execErr
	}, events...)
	return result, err
}
