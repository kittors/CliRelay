package postgres

// usageRollupSubjectIndexSQL lets one AI account's buckets be read across
// tenants without scanning the table. Every other index on usage_rollup_buckets
// leads with tenant_id, but an account is one upstream account whichever tenants
// send it requests, and the spend shown inside its quota windows sums them all
// (usage.QueryAIAccountWindowSpend). Without it that read scanned the whole
// table: 80-190 ms on production on 2026-11-10, several times the rest of the
// status list. The legacy-subject merge and the shared lifetime backfill filter
// on auth_subject_id alone too.
//
// The indexed columns never change once a bucket exists, so the counter updates
// every request makes stay eligible for heap-only updates. Migrations run in a
// transaction, which rules out CONCURRENTLY, so the build blocks writes to the
// table while it runs; the table is bounded by bucket retention (minutes for a
// day, hours for 30 days, days for 400) and held 46k rows on production, a
// build well under a second.
const usageRollupSubjectIndexSQL = `
CREATE INDEX IF NOT EXISTS idx_usage_rollup_subject_kind_start
  ON usage_rollup_buckets(auth_subject_id, bucket_kind, bucket_start);
`
