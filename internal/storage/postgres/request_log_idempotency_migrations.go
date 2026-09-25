package postgres

// requestLogIdempotencyKeysSQL stores one key per committed request log write,
// so a write retried after its commit acknowledgement was lost (a primary
// failover mid-commit) is recognised instead of being inserted and projected
// into the usage rollups a second time.
//
// It is a separate table on purpose. request_logs is the largest table, and
// migrations run inside a transaction, which rules out CREATE INDEX
// CONCURRENTLY; a unique index added there would block every request log
// write for the whole build. Creating this empty table takes no lock on any
// existing table. Rows are pruned by created_at after a retention window.
const requestLogIdempotencyKeysSQL = `
CREATE TABLE IF NOT EXISTS request_log_idempotency_keys (
  idempotency_key TEXT PRIMARY KEY,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_request_log_idempotency_keys_created_at
  ON request_log_idempotency_keys(created_at);
`
