package postgres

// requestLogContentBodiesStrippedSQL records on each request_log_content row
// whether its bodies have been stripped, so the purge that runs while body
// storage is off reads only the rows that may still hold them
// (usage.purgeStoredRequestBodies).
//
// Rows written with storage off are stripped on the way in and get 1. Rows
// written with storage on, and every row older than this column, read 0; the
// purge checks each of those once and sets 1. Before this the purge re-read the
// whole table on every maintenance pass: on production on 2026-11-10, 76k rows
// and 4 to 22 minutes a pass, every row already stripped.
//
// A constant default makes this a catalog change on PostgreSQL 11 and later, so
// the ALTER holds its lock for a moment. The index is its own migration: its
// build blocks writes to the table, and inside this migration's transaction it
// would hold the ALTER's lock, which blocks reads as well.
const requestLogContentBodiesStrippedSQL = `
ALTER TABLE request_log_content
ADD COLUMN IF NOT EXISTS bodies_stripped INTEGER NOT NULL DEFAULT 0;
`

// requestLogContentUnstrippedIndexSQL is the partial index the purge walks in
// log_id order. A row leaves it once stripped, so while body storage stays off
// it holds nothing and a purge pass is one empty index lookup. Log ids come from
// one sequence, so the index needs no tenant_id; the primary key leads with
// tenant_id, which is why the old walk had no ordered path and re-read every
// detail row through the timestamp index for each batch of 25.
//
// Migrations run in a transaction, which rules out CONCURRENTLY, so the build
// blocks writes to request_log_content while it scans the table. On upgrade
// every existing row reads 0 and goes in. Production held 76k rows in a 99 MB
// heap on 2026-11-10; 76k rows in a 123 MB heap built in 120 to 180 ms on a
// local PostgreSQL 15. Usage writes are queued, so they wait out the build
// rather than fail.
const requestLogContentUnstrippedIndexSQL = `
CREATE INDEX IF NOT EXISTS idx_log_content_bodies_unstripped
  ON request_log_content(log_id)
  WHERE bodies_stripped = 0;
`
