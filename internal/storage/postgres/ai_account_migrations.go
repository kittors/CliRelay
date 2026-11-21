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

// aiAccountSubscriptionCycleSQL adds the billing-cycle details a live read of
// the provider returns (Codex accounts/check entitlement), and drops the token
// snapshots that are past their end.
//
// Those snapshots never described the plan after its first cycle: Codex keeps
// the chatgpt_subscription_active_* claims from the OAuth login when it
// re-issues tokens on refresh. On 2026-11-20 every signed_claims row in
// production had ended between 07-11 and 10-10 while the plans kept renewing,
// so the panel would have shown each one as long expired. The next live read
// fills the dates back in.
const aiAccountSubscriptionCycleSQL = `
ALTER TABLE ai_account_subject_status ADD COLUMN IF NOT EXISTS subscription_renews_at TIMESTAMPTZ;
ALTER TABLE ai_account_subject_status ADD COLUMN IF NOT EXISTS subscription_cancels_at TIMESTAMPTZ;
ALTER TABLE ai_account_subject_status ADD COLUMN IF NOT EXISTS subscription_period TEXT NOT NULL DEFAULT '';
ALTER TABLE ai_account_subject_status ADD COLUMN IF NOT EXISTS subscription_state TEXT NOT NULL DEFAULT '';
ALTER TABLE ai_account_subject_status ADD COLUMN IF NOT EXISTS subscription_checked_at TIMESTAMPTZ;

UPDATE ai_account_subject_status
SET subscription_started_at = NULL, subscription_expires_at = NULL, subscription_source = ''
WHERE subscription_source IN ('signed_claims', 'migration')
  AND (subscription_expires_at IS NULL OR subscription_expires_at <= now());
`
