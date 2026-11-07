package postgres

// accessPolicySQL adds the access_policy column to the three objects that carry
// quota: portal accounts (end_users), API keys and permission profiles.
//
// It is a JSON document holding what the fixed limit columns cannot express:
// request and token limits per window, per-model quota rules (with an optional
// fallback model once a rule runs out) and model routes. The fixed columns stay
// the single source for the cells they already hold; see internal/quota/policy.go.
//
// The default '{}' is the empty policy, so existing rows need no backfill and a
// node of the previous build, which never reads the column, is unaffected.
const accessPolicySQL = `
ALTER TABLE end_users ADD COLUMN IF NOT EXISTS access_policy TEXT NOT NULL DEFAULT '{}';
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS access_policy TEXT NOT NULL DEFAULT '{}';
ALTER TABLE api_key_permission_profiles ADD COLUMN IF NOT EXISTS access_policy TEXT NOT NULL DEFAULT '{}';
`
