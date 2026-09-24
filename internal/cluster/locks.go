package cluster

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"time"
)

// Lock names shared by every node. Advisory locks are only meaningful when
// all nodes agree on the key, so names live here instead of at call sites.
const (
	// LockMigrate serialises schema migrations and the one-time data
	// imports that run before a node serves traffic.
	LockMigrate = "migrate"
	// LockMaintenanceRepairs serialises the post-start repair hooks.
	LockMaintenanceRepairs = "maintenance-repairs"
	// LockLeader is the cluster leadership lock.
	LockLeader = "leader"
)

// ErrLockTimeout is returned when a session lock is not acquired in time.
var ErrLockTimeout = errors.New("cluster: timed out waiting for advisory lock")

// LockKey maps a lock name to PostgreSQL's 64-bit advisory lock key space.
// The "clirelay:" prefix keeps the keys clear of the literal keys and
// hashtext() keys that older code already uses.
func LockKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("clirelay:" + name))
	return int64(h.Sum64())
}

// WithSessionLock runs fn while holding the session advisory lock name on a
// dedicated connection from db. It polls for up to wait (wait <= 0 means a
// single attempt) and returns ErrLockTimeout if another session keeps it.
//
// A session lock survives transaction boundaries inside fn, which is what
// schema migrations need; it is released when fn returns, and PostgreSQL
// releases it anyway if this process dies and the connection drops.
//
// When db is not PostgreSQL (SQLite in unit tests) the lock is a no-op.
func WithSessionLock(ctx context.Context, db *sql.DB, name string, wait time.Duration, fn func(context.Context) error) error {
	if db == nil {
		return fn(ctx)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("cluster: acquire connection for lock %s: %w", name, err)
	}
	defer conn.Close()

	key := LockKey(name)
	deadline := time.Now().Add(wait)
	for {
		var locked bool
		err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&locked)
		if err != nil {
			if isUnsupportedAdvisoryLockError(err) {
				return fn(ctx)
			}
			return fmt.Errorf("cluster: take lock %s: %w", name, err)
		}
		if locked {
			break
		}
		if wait <= 0 || time.Now().After(deadline) {
			return fmt.Errorf("%w: %s", ErrLockTimeout, name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(unlockCtx, `SELECT pg_advisory_unlock($1)`, key)
	}()
	return fn(ctx)
}

// TryXactLock takes the transaction-scoped advisory lock name inside tx
// without waiting. It returns false when another transaction holds it. The
// lock is released at commit or rollback. SQLite reports true.
func TryXactLock(ctx context.Context, tx *sql.Tx, name string) (bool, error) {
	if tx == nil {
		return true, nil
	}
	var locked bool
	if err := tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock($1)`, LockKey(name)).Scan(&locked); err != nil {
		if isUnsupportedAdvisoryLockError(err) {
			return true, nil
		}
		return false, fmt.Errorf("cluster: take xact lock %s: %w", name, err)
	}
	return locked, nil
}

// XactLock takes the transaction-scoped advisory lock name inside tx,
// waiting as long as ctx allows. SQLite is a no-op.
func XactLock(ctx context.Context, tx *sql.Tx, name string) error {
	if tx == nil {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, LockKey(name)); err != nil {
		if isUnsupportedAdvisoryLockError(err) {
			return nil
		}
		return fmt.Errorf("cluster: take xact lock %s: %w", name, err)
	}
	return nil
}

func isUnsupportedAdvisoryLockError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such function") ||
		(strings.Contains(msg, "does not exist") && strings.Contains(msg, "pg_")) ||
		(strings.Contains(msg, "undefined function") && strings.Contains(msg, "pg_"))
}
