package postgres

// Tables behind cross-node sessions and asynchronous work. In cluster mode a
// login callback, a task poll or a job status request can reach any node, so
// the state they need lives here instead of in the node that started the
// work. All four are new tables; nothing existing changes shape.

// oauthSessionsSQL holds OAuth login sessions. The PKCE verifier never
// leaves the owner node; the row carries the status and the provider callback
// so any node can accept the callback and answer status polls.
const oauthSessionsSQL = `
CREATE TABLE IF NOT EXISTS oauth_sessions (
  state            TEXT PRIMARY KEY,
  provider         TEXT NOT NULL,
  tenant_id        TEXT NOT NULL DEFAULT '',
  status           TEXT NOT NULL DEFAULT 'pending',
  error            TEXT NOT NULL DEFAULT '',
  owner_node       TEXT NOT NULL DEFAULT '',
  callback_payload JSONB,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at       TIMESTAMPTZ NOT NULL,
  completed_at     TIMESTAMPTZ,
  heartbeat_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_oauth_sessions_expires_at ON oauth_sessions (expires_at);
CREATE INDEX IF NOT EXISTS idx_oauth_sessions_pending_provider
  ON oauth_sessions (provider, tenant_id) WHERE status = 'pending';
`

// asyncTaskRoutesSQL remembers which credential created an upstream task, so
// polling it reaches the same account from any node.
const asyncTaskRoutesSQL = `
CREATE TABLE IF NOT EXISTS async_task_routes (
  task_id    TEXT PRIMARY KEY,
  kind       TEXT NOT NULL,
  provider   TEXT NOT NULL DEFAULT '',
  auth_id    TEXT NOT NULL DEFAULT '',
  tenant_id  TEXT NOT NULL DEFAULT '',
  model      TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_async_task_routes_expires_at ON async_task_routes (expires_at);
`

// managementJobsSQL holds snapshots of console jobs (image, video and model
// tests, AI account status refreshes). The node running a job writes them;
// any node serves polls. version orders concurrent writes, heartbeat_at shows
// whether the running node is still alive. result and error are raw JSON
// bytes: they are only ever read back whole.
const managementJobsSQL = `
CREATE TABLE IF NOT EXISTS management_jobs (
  id           TEXT PRIMARY KEY,
  kind         TEXT NOT NULL,
  tenant_id    TEXT NOT NULL DEFAULT '',
  status       TEXT NOT NULL DEFAULT '',
  phase        TEXT NOT NULL DEFAULT '',
  terminal     BOOLEAN NOT NULL DEFAULT false,
  result       BYTEA,
  error        BYTEA,
  owner_node   TEXT NOT NULL DEFAULT '',
  version      BIGINT NOT NULL DEFAULT 0,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at   TIMESTAMPTZ NOT NULL,
  heartbeat_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_management_jobs_expires_at ON management_jobs (expires_at);
`

// warmupPoliciesSQL persists quota warmup policies, which used to live only
// in memory and vanished on restart. revision changes on every operator edit
// so the scheduler never overwrites an edit with stale run bookkeeping.
const warmupPoliciesSQL = `
CREATE TABLE IF NOT EXISTS warmup_policies (
  tenant_id  TEXT NOT NULL,
  id         TEXT NOT NULL,
  policy     JSONB NOT NULL,
  revision   BIGINT NOT NULL DEFAULT 1,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, id)
);
`
