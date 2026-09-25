package usage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Exactly-once request log writes.
//
// A commit can succeed on the server while the client sees the connection die
// before the acknowledgement arrives, which is exactly what a primary failover
// does to in-flight transactions. Retrying (or replaying from the local spool)
// would then insert the row a second time and add its tokens and cost to every
// rollup bucket again. Each record therefore carries a key, and the write
// transaction claims that key first: a key that is already present means an
// earlier attempt committed, so the retry rolls back without touching any
// projection.
//
// The keys live in their own small table rather than in a unique index on
// request_logs. request_logs is large, the migration runner wraps every
// migration in a transaction (so CREATE INDEX CONCURRENTLY is not available),
// and a plain CREATE INDEX there would block writes for as long as the build
// takes on every upgrade. A new table is created instantly and its primary key
// also serialises two concurrent claims of the same key: the second waits for
// the first transaction and then sees the conflict.
const (
	requestLogIdempotencyTable = "request_log_idempotency_keys"

	// requestLogIdempotencyRetention bounds how long a key is kept. A key only
	// has to outlive the retries and spool replays of its own record, which
	// happen within seconds to minutes of the original attempt; three days also
	// covers a node that stays down over a weekend with records in its spool.
	requestLogIdempotencyRetention = 72 * time.Hour

	requestLogIdempotencyPruneBatch = 5000
	// requestLogIdempotencyTimeLayout is fixed width so SQLite's text
	// comparison in the prune query orders timestamps correctly.
	requestLogIdempotencyTimeLayout = "2006-01-02T15:04:05Z"
)

// requestLogIdempotencySQLiteSQL mirrors the PostgreSQL migration for the
// embedded test database.
const requestLogIdempotencySQLiteSQL = `
CREATE TABLE IF NOT EXISTS request_log_idempotency_keys (
  idempotency_key TEXT PRIMARY KEY,
  created_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_request_log_idempotency_keys_created_at
  ON request_log_idempotency_keys(created_at);
`

func ensureRequestLogIdempotencyTable(db *sql.DB) error {
	if db == nil {
		return nil
	}
	if _, err := db.Exec(requestLogIdempotencySQLiteSQL); err != nil {
		return fmt.Errorf("usage: ensure %s: %w", requestLogIdempotencyTable, err)
	}
	return nil
}

// claimRequestLogIdempotencyKeyTx records key inside the write transaction. It
// reports false when the key is already taken, meaning this record was
// committed by an earlier attempt and must not be written again.
func claimRequestLogIdempotencyKeyTx(tx usageWriteTx, key string) (bool, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return true, nil
	}
	res, err := tx.Exec(`INSERT INTO request_log_idempotency_keys (idempotency_key, created_at)
		VALUES (?, ?)
		ON CONFLICT (idempotency_key) DO NOTHING`,
		key, time.Now().UTC().Format(requestLogIdempotencyTimeLayout))
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// pruneRequestLogIdempotencyKeys deletes keys claimed before cutoff, in
// batches so a backlog never turns into one long transaction.
func pruneRequestLogIdempotencyKeys(ctx context.Context, db *sql.DB, cutoff time.Time) (int64, error) {
	if db == nil {
		return 0, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cut := cutoff.UTC().Format(requestLogIdempotencyTimeLayout)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		res, err := db.ExecContext(ctx, `DELETE FROM request_log_idempotency_keys
			WHERE idempotency_key IN (
				SELECT idempotency_key FROM request_log_idempotency_keys
				WHERE created_at < ?
				LIMIT ?
			)`, cut, requestLogIdempotencyPruneBatch)
		if err != nil {
			return total, fmt.Errorf("usage: prune %s: %w", requestLogIdempotencyTable, err)
		}
		n, _ := res.RowsAffected()
		total += n
		if n < requestLogIdempotencyPruneBatch {
			return total, nil
		}
	}
}
