package postgres

// authSessionHardeningSQL adds windowed lockout state, remember-me provenance,
// an absolute session deadline, and a per-token table that makes refresh
// rotation survivable (grace window + reuse detection) instead of destroying
// the previous token in place.
//
// Per-token rows are used instead of previous_* columns on user_sessions for
// three reasons: (1) two concurrent grace replays would fight over a single
// previous_* slot and the loser would be misclassified as a replay; (2) reuse
// detection becomes a property of the token row itself, so sibling forks are
// legal without cascading revocation; (3) token_hash as PRIMARY KEY keeps
// lookups as single-column equality — an `OR` across two partial indexes on
// user_sessions degrades to a sequential scan on an unauthenticated endpoint.
const authSessionHardeningSQL = `
-- 1) users: windowed lockout state -----------------------------------------
ALTER TABLE users ADD COLUMN IF NOT EXISTS last_failed_login_at TIMESTAMPTZ;
ALTER TABLE users ADD COLUMN IF NOT EXISTS lock_stage INTEGER NOT NULL DEFAULT 0;

-- 2) end_users: windowed lockout state (portal side, same defect) -----------
ALTER TABLE end_users ADD COLUMN IF NOT EXISTS last_failed_login_at TIMESTAMPTZ;

-- 3) user_sessions: remember-me + TTL provenance + absolute family deadline --
ALTER TABLE user_sessions ADD COLUMN IF NOT EXISTS remember_me BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE user_sessions ADD COLUMN IF NOT EXISTS refresh_ttl_seconds INTEGER NOT NULL DEFAULT 0;
ALTER TABLE user_sessions ADD COLUMN IF NOT EXISTS refresh_absolute_expires_at TIMESTAMPTZ;

-- 4) per-token rows: one row per issued access/refresh token ----------------
CREATE TABLE IF NOT EXISTS user_session_tokens (
  token_hash   TEXT PRIMARY KEY,
  session_id   UUID NOT NULL REFERENCES user_sessions(id) ON DELETE CASCADE,
  kind         TEXT NOT NULL CHECK (kind IN ('access', 'refresh')),
  issued_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at   TIMESTAMPTZ NOT NULL,
  consumed_at  TIMESTAMPTZ,
  grace_until  TIMESTAMPTZ,
  reuse_count  INTEGER NOT NULL DEFAULT 0,
  generation   BIGINT NOT NULL DEFAULT 0,
  parent_hash  TEXT
);
CREATE INDEX IF NOT EXISTS idx_user_session_tokens_session
  ON user_session_tokens(session_id, kind, expires_at DESC);
CREATE INDEX IF NOT EXISTS idx_user_session_tokens_expiry
  ON user_session_tokens(expires_at);

-- 5) backfill remember-me / ttl / absolute deadline -------------------------
-- Existing 30-day refresh windows were produced by remember_me at login;
-- defaulting them to false would silently downgrade every live "remember me"
-- session on its first refresh, which is the defect this migration fixes.
UPDATE user_sessions
   SET remember_me = true
 WHERE remember_me = false
   AND refresh_expires_at IS NOT NULL
   AND refresh_expires_at - created_at >= interval '29 days';

UPDATE user_sessions
   SET refresh_ttl_seconds =
       LEAST(2147483647, GREATEST(0, EXTRACT(EPOCH FROM (refresh_expires_at - created_at))::bigint))::int
 WHERE refresh_ttl_seconds = 0
   AND refresh_expires_at IS NOT NULL
   AND refresh_expires_at > created_at;

UPDATE user_sessions
   SET refresh_absolute_expires_at = created_at + interval '60 days'
 WHERE refresh_absolute_expires_at IS NULL;

-- 6) mirror every live token into the new table -----------------------------
-- Without this, the first request after deploy 401s for every signed-in user.
INSERT INTO user_session_tokens (token_hash, session_id, kind, issued_at, expires_at, generation)
SELECT s.token_hash, s.id, 'access', s.created_at, s.expires_at, 0
  FROM user_sessions s
 WHERE s.revoked_at IS NULL
   AND s.token_hash IS NOT NULL AND s.token_hash <> ''
ON CONFLICT (token_hash) DO NOTHING;

INSERT INTO user_session_tokens (token_hash, session_id, kind, issued_at, expires_at, generation)
SELECT s.refresh_token_hash, s.id, 'refresh', s.created_at, s.refresh_expires_at, 0
  FROM user_sessions s
 WHERE s.revoked_at IS NULL
   AND s.refresh_token_hash IS NOT NULL AND s.refresh_token_hash <> ''
   AND s.refresh_expires_at IS NOT NULL
ON CONFLICT (token_hash) DO NOTHING;

-- 7) repair lifetime-accumulated lockout state ------------------------------
-- failed_login_count never decayed, so stored values are meaningless under the
-- new sliding window. Auto-locks (locked_until IS NOT NULL) are released;
-- administrative locks (locked_until IS NULL) are deliberately preserved.
-- Postgres evaluates every SET expression against the pre-UPDATE row, so the
-- CASE below still sees the original locked_until.
UPDATE users
   SET status = CASE WHEN status = 'locked' AND locked_until IS NOT NULL THEN 'active' ELSE status END,
       failed_login_count = 0,
       last_failed_login_at = NULL,
       lock_stage = 0,
       locked_until = NULL,
       updated_at = now()
 WHERE failed_login_count > 0
    OR locked_until IS NOT NULL;

-- 8) portal side: release the permanent lock produced by lockPenalty(20) ----
-- lock_stage = 4 with locked_until IS NULL is only ever produced by the
-- brute-force path, never by an administrator, so releasing it cannot
-- resurrect a deliberately disabled account.
UPDATE end_users
   SET status = CASE WHEN status = 'locked' AND lock_stage > 0 THEN 'active' ELSE status END,
       failed_login_count = 0,
       last_failed_login_at = NULL,
       lock_stage = 0,
       locked_until = NULL,
       updated_at = now()
 WHERE failed_login_count > 0
    OR locked_until IS NOT NULL
    OR lock_stage > 0;
`
