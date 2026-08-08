package postgres

// Migrations for the shared AI-account read model. Kept out of migrations.go so
// that file stops growing with every release; auth_session_migrations.go set the
// same precedent for identity.

const aiAccountQuotaObservedAtSQL = `
ALTER TABLE ai_account_subject_status
ADD COLUMN IF NOT EXISTS quota_observed_at TIMESTAMPTZ;

-- ai_account_subject_quota_points is only written after a successful probe, so its
-- newest row dates the stored payload correctly even for accounts that have been
-- failing for days (whose upstream_checked_at has moved far past the data it describes).
UPDATE ai_account_subject_status s
SET quota_observed_at = COALESCE(
  (SELECT MAX(p.recorded_at) FROM ai_account_subject_quota_points p
   WHERE p.auth_subject_id = s.auth_subject_id),
  CASE WHEN s.last_probe_state = 'success' THEN s.upstream_checked_at END
)
WHERE s.quota_observed_at IS NULL AND s.quota_json <> '[]';
`
