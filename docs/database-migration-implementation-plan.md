# Multi-Database Migration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add configurable SQLite, PostgreSQL, and MySQL support for CliRelay's DB-backed runtime data, with a no-loss migration workflow, system-page database status, and a simple management UI progress experience.

**Architecture:** Keep SQLite as the default and introduce a database backend abstraction under `internal/usage` so existing request logs, API keys, model config, routing config, proxy pool, quota snapshots, and runtime settings can run on SQLite, PostgreSQL, or MySQL. Add a migration service that copies data from the currently active backend into a target backend in deterministic batches, verifies counts and sampled content, then only switches `config.yaml` after the migration completes successfully and the user confirms the cutover. Expose management APIs for database info, connection testing, migration preview, migration start, and migration progress polling; the `/manage/system` page consumes those APIs and refreshes progress once per second.

**Tech Stack:** Go 1.26, `database/sql`, `modernc.org/sqlite`, `github.com/jackc/pgx/v5/stdlib`, `github.com/go-sql-driver/mysql`, Gin management APIs, YAML config, existing management panel assets from the `remote-management.panel-github-repository` frontend repository.

---

## Current Code Baseline

This plan is based on latest `origin/dev` as of 2026-05-03, commit `69b3cce1`.

The current runtime DB path is hard-coded by service startup:

- `internal/cmd/run.go` creates `<config-dir>/data/usage.db`.
- `internal/cmd/run.go` calls `usage.InitDB(dbPath, cfg.RequestLogStorage, loc)`.
- `internal/usage/usage_db.go` opens `sql.Open("sqlite", dbPath)`, applies SQLite PRAGMAs, then creates every DB-backed table.

The current DB-backed tables include:

- `request_logs`
- `request_log_content`
- `auth_file_quota_snapshots`
- `auth_file_quota_snapshot_points`
- `api_keys`
- `model_pricing`
- `model_configs`
- `model_owner_presets`
- `model_openrouter_sync_state`
- `routing_config`
- `proxy_pool`
- `runtime_settings`

The repository already has a separate PostgreSQL-backed config/auth store:

- `internal/store/postgresstore.go` stores `config_store` and `auth_store`.
- That store does not currently make `internal/usage` use PostgreSQL.

The current system page API is backed by:

- `internal/api/handlers/management/system_stats.go`
- `GET /v0/management/system-stats`
- `GET /v0/management/system-stats/ws`

It reports SQLite file size through `usage.GetDBPath()` and has no database type, DSN, version, or migration status fields yet.

The backend repository contains built management assets (`manage.html`, `assets/*`) but not the full management UI source. The default frontend repository is configured in `internal/config/config.go` as `https://github.com/kittors/codeProxy`. Full UI work should be implemented in that frontend repository, then built assets should be synced into this backend repository as the project currently does for management panel releases.

## Expected Product Behavior

The first implementation must support these user-facing behaviors:

- `config.yaml` can choose `sqlite`, `postgres`, or `mysql`.
- Existing installations continue using SQLite without config changes.
- Users can migrate from SQLite to PostgreSQL or MySQL from `/manage/system`.
- Users can migrate between PostgreSQL and MySQL after both backends exist.
- Migration is no-loss for DB-backed runtime data.
- Migration progress is visible in a polished modal with a progress bar and a message that updates once per second.
- The active database is only switched after migration verification succeeds.
- If migration fails, the old database stays active and the error is shown in the modal.
- `/manage/system` displays database type, masked DSN, connection status, database version, and table/data size summary.
- Config file changes are made only after successful migration and user confirmation.
- The implementation has unit, handler, and integration tests for schema, copy, verification, progress, failure safety, and config cutover.

## Non-Goals For The First Implementation

- No remote deployment, restart, or production service replacement is part of this plan.
- No SSO, users, projects, or RBAC are part of this specific plan.
- No automatic background migration on startup.
- No destructive cleanup of the source database after migration.
- No live dual-write between two databases.
- No migration of file-backed auth token files unless they are already represented in DB-backed tables.

## Configuration Design

Add a new top-level `database` section:

```yaml
database:
  type: sqlite
  sqlite:
    path: ""
  postgres:
    dsn: ""
    schema: ""
    max-open-conns: 10
    max-idle-conns: 5
  mysql:
    dsn: ""
    max-open-conns: 10
    max-idle-conns: 5
  migration:
    batch-size: 1000
    verify-samples: 50
```

Behavior:

- Empty or missing `database.type` means `sqlite`.
- Empty SQLite path means `<config-dir>/data/usage.db`, preserving current behavior.
- PostgreSQL and MySQL DSNs are never returned unmasked by management APIs.
- `migration.batch-size` is clamped to `100..10000`; the default is `1000`.
- `migration.verify-samples` is clamped to `0..1000`; the default is `50`.

## Data Safety Rules

- Create all target tables before copying any rows.
- Copy each table in primary-key order where a primary key exists.
- Copy in batches so progress can update frequently.
- Use target-side transactions per table batch.
- Do not modify the source database during copy.
- Do not change `config.yaml` until all copied rows pass verification.
- Keep the source database available after cutover.
- If a target table already contains CliRelay data, migration preview must require an explicit overwrite option; the first UI version should default overwrite to false.
- If overwrite is true, truncate only known CliRelay tables in the target database before copy.

## File Structure

### Backend files to create

- `internal/config/database.go`: database config structs, defaults, normalization, and DSN masking helpers.
- `internal/usage/db_backend.go`: backend type enum, driver opening, `BackendInfo`, and shared database handle metadata.
- `internal/usage/db_dialect.go`: SQL dialect interface for placeholders, schema DDL, UPSERT forms, truncation, version query, and byte-size queries.
- `internal/usage/db_schema.go`: table definitions and schema initialization orchestration for SQLite, PostgreSQL, and MySQL.
- `internal/usage/db_migration.go`: migration job model, migration runner, table copy logic, verification, progress snapshots.
- `internal/usage/db_migration_test.go`: SQLite-backed migration unit tests using temporary source and target databases.
- `internal/api/handlers/management/database.go`: management handlers for database info, connection test, migration preview/start/status.
- `internal/api/handlers/management/database_test.go`: API handler tests for masking, progress, failure handling, and cutover behavior.

### Backend files to modify

- `internal/config/config.go`: include `Database DatabaseConfig`, apply defaults, sanitize values.
- `config.example.yaml`: document database selection and migration settings.
- `internal/cmd/run.go`: resolve configured database backend instead of always building `data/usage.db`.
- `internal/usage/usage_db.go`: replace SQLite-only `InitDB(dbPath, ...)` with backend-aware initialization while preserving a compatibility wrapper for tests.
- `internal/usage/log_content_store.go`: isolate SQLite-only WAL/VACUUM behavior behind dialect checks.
- `internal/usage/apikey_db.go`: replace SQLite-only `INSERT OR IGNORE` / `?` assumptions with dialect helpers.
- `internal/usage/model_config_db.go`: replace SQLite-specific inserts and schema fragments with dialect helpers.
- `internal/usage/openrouter_model_sync.go`: replace SQLite column introspection with dialect-specific introspection.
- `internal/usage/pricing_db.go`: replace SQLite-only upsert patterns.
- `internal/usage/proxy_pool_db.go`: replace SQLite-only upsert patterns.
- `internal/usage/routing_config_db.go`: replace SQLite-only upsert patterns.
- `internal/usage/runtime_settings_db.go`: replace SQLite-only upsert patterns.
- `internal/api/server.go`: register new management database routes.
- `internal/api/handlers/management/system_stats.go`: add database metadata to system stats or call the new database info helper.
- `go.mod` / `go.sum`: add `github.com/go-sql-driver/mysql`.
- `README.md` and `README_CN.md`: document database configuration and migration workflow.

### Frontend files to change in management UI source repository

The backend repository does not include the full source for `/manage/system`. Implement UI changes in the management panel source repository configured by `remote-management.panel-github-repository`:

- Add database status card to the system page.
- Add "Migrate database" action.
- Add modal with database type selection, DSN input, test connection button, migration preview, progress bar, status text, failure state, and success confirmation.
- Poll `GET /v0/management/database/migration/:id` once per second while status is `pending` or `running`.
- Mask DSN values in all display states.

After the frontend is built, sync the generated management assets into this backend repository according to the current panel release process.

## API Design

Register these management routes under `/v0/management`:

- `GET /database`
- `POST /database/test`
- `POST /database/migrations/preview`
- `POST /database/migrations`
- `GET /database/migrations/:id`
- `POST /database/migrations/:id/confirm-cutover`

### `GET /database`

Response:

```json
{
  "type": "sqlite",
  "dsn": "data/usage.db",
  "masked_dsn": "data/usage.db",
  "connected": true,
  "version": "3.50.4",
  "schema_ready": true,
  "stats": {
    "tables": 12,
    "rows": {
      "request_logs": 1200,
      "request_log_content": 900,
      "api_keys": 8
    },
    "size_bytes": 10485760
  }
}
```

### `POST /database/test`

Request:

```json
{
  "type": "postgres",
  "dsn": "postgres://user:pass@127.0.0.1:5432/clirelay?sslmode=disable",
  "schema": "public"
}
```

Response:

```json
{
  "ok": true,
  "type": "postgres",
  "version": "PostgreSQL 17.2",
  "masked_dsn": "postgres://user:***@127.0.0.1:5432/clirelay?sslmode=disable"
}
```

### `POST /database/migrations/preview`

Request:

```json
{
  "target": {
    "type": "mysql",
    "dsn": "clirelay:secret@tcp(127.0.0.1:3306)/clirelay?parseTime=true&charset=utf8mb4"
  },
  "overwrite": false
}
```

Response:

```json
{
  "source_type": "sqlite",
  "target_type": "mysql",
  "target_empty": true,
  "tables": [
    {"name": "request_logs", "rows": 1200},
    {"name": "request_log_content", "rows": 900},
    {"name": "api_keys", "rows": 8}
  ],
  "total_rows": 2108,
  "warnings": []
}
```

### `POST /database/migrations`

Request:

```json
{
  "target": {
    "type": "postgres",
    "dsn": "postgres://user:pass@127.0.0.1:5432/clirelay?sslmode=disable",
    "schema": "public"
  },
  "overwrite": false
}
```

Response:

```json
{
  "id": "mig_20260503_123456_abcdef",
  "status": "pending"
}
```

### `GET /database/migrations/:id`

Response while running:

```json
{
  "id": "mig_20260503_123456_abcdef",
  "status": "running",
  "source_type": "sqlite",
  "target_type": "postgres",
  "phase": "copy_request_logs",
  "table": "request_logs",
  "current_rows": 3000,
  "total_rows": 10000,
  "percent": 30,
  "message": "Migrating request_logs",
  "started_at": "2026-05-03T12:34:56Z",
  "updated_at": "2026-05-03T12:35:10Z"
}
```

Response after copy and verification:

```json
{
  "id": "mig_20260503_123456_abcdef",
  "status": "ready_for_cutover",
  "source_type": "sqlite",
  "target_type": "postgres",
  "phase": "verified",
  "current_rows": 10000,
  "total_rows": 10000,
  "percent": 100,
  "message": "Migration verified. Confirm cutover to update config.yaml.",
  "verification": {
    "row_counts_match": true,
    "sampled_rows_match": true
  }
}
```

### `POST /database/migrations/:id/confirm-cutover`

Response:

```json
{
  "ok": true,
  "type": "postgres",
  "message": "Database configuration updated. Restart the service to use the new database connection."
}
```

The first version can require a restart after cutover. Hot-swapping the live `*sql.DB` is intentionally excluded from the first implementation because current code stores a package-level DB handle and many caches are initialized during startup.

## Implementation Tasks

### Task 1: Add Database Config Types

**Files:**

- Create: `internal/config/database.go`
- Modify: `internal/config/config.go`
- Modify: `config.example.yaml`
- Test: `internal/config/database_test.go`

- [ ] **Step 1: Write failing config tests**

Add tests that assert missing config defaults to SQLite, SQLite path can be empty, DSNs are masked, invalid types normalize to SQLite, and migration settings are clamped.

Run:

```bash
go test ./internal/config -run 'TestDatabaseConfig' -count=1
```

Expected before implementation: compile failure because `DatabaseConfig` is not defined.

- [ ] **Step 2: Implement config structs and normalization**

Add `DatabaseConfig`, `SQLiteDatabaseConfig`, `PostgresDatabaseConfig`, `MySQLDatabaseConfig`, and `DatabaseMigrationConfig`. Implement methods:

```go
func (cfg *DatabaseConfig) Normalize()
func (cfg DatabaseConfig) TypeOrDefault() string
func MaskDatabaseDSN(dbType, dsn string) string
```

Use `sqlite`, `postgres`, and `mysql` as the only supported values.

- [ ] **Step 3: Wire config loading defaults**

Add `Database DatabaseConfig` to `config.Config`, set default database type to SQLite before YAML unmarshal, and call `cfg.Database.Normalize()` after unmarshal.

- [ ] **Step 4: Document config sample**

Update `config.example.yaml` with the database block shown in this plan.

- [ ] **Step 5: Verify**

Run:

```bash
go test ./internal/config -run 'TestDatabaseConfig' -count=1
git diff --check
```

Expected: tests pass, no whitespace errors.

- [ ] **Step 6: Commit**

```bash
git add internal/config/database.go internal/config/config.go internal/config/database_test.go config.example.yaml
git commit -m "feat: add database configuration model"
```

### Task 2: Introduce Usage DB Backend And Dialects

**Files:**

- Create: `internal/usage/db_backend.go`
- Create: `internal/usage/db_dialect.go`
- Create: `internal/usage/db_schema.go`
- Modify: `internal/usage/usage_db.go`
- Modify: `go.mod`
- Modify: `go.sum`
- Test: `internal/usage/db_backend_test.go`

- [ ] **Step 1: Write failing backend tests**

Cover opening SQLite with the existing temp-file behavior, rejecting missing PostgreSQL/MySQL DSNs, returning masked DSNs, and exposing a version query for SQLite.

Run:

```bash
go test ./internal/usage -run 'TestOpenUsageDatabase|TestDatabaseInfo' -count=1
```

Expected before implementation: compile failure because backend APIs are missing.

- [ ] **Step 2: Add MySQL driver dependency**

Run:

```bash
go get github.com/go-sql-driver/mysql@latest
```

Expected: `go.mod` and `go.sum` include the MySQL driver.

- [ ] **Step 3: Create backend open API**

Implement:

```go
type DatabaseType string

const (
    DatabaseTypeSQLite   DatabaseType = "sqlite"
    DatabaseTypePostgres DatabaseType = "postgres"
    DatabaseTypeMySQL    DatabaseType = "mysql"
)

type OpenDatabaseOptions struct {
    Config     config.DatabaseConfig
    ConfigDir  string
    Storage    config.RequestLogStorageConfig
    Location   *time.Location
}

type BackendInfo struct {
    Type        string
    DSN         string
    MaskedDSN   string
    Version     string
    Connected   bool
    SchemaReady bool
}
```

Keep `InitDB(dbPath, storageCfg, loc)` as a compatibility wrapper that constructs SQLite options and calls the new initializer.

- [ ] **Step 4: Add dialect interface**

Implement dialect methods for placeholders, schema creation, version query, table truncation, and byte size query. SQLite uses `?`, PostgreSQL uses `$1`, `$2`, and MySQL uses `?`.

- [ ] **Step 5: Move schema creation to db_schema.go**

Keep the table list unchanged. Convert SQLite-specific column types to dialect-specific equivalents:

- SQLite: `INTEGER PRIMARY KEY AUTOINCREMENT`, `TEXT`, `BLOB`, `DATETIME`
- PostgreSQL: `BIGSERIAL PRIMARY KEY`, `TEXT`, `BYTEA`, `TIMESTAMPTZ`
- MySQL: `BIGINT AUTO_INCREMENT PRIMARY KEY`, `TEXT`, `LONGBLOB`, `DATETIME(6)`

- [ ] **Step 6: Verify**

Run:

```bash
go test ./internal/usage -run 'TestOpenUsageDatabase|TestDatabaseInfo|TestInitDB' -count=1
git diff --check
```

Expected: SQLite tests pass and existing `InitDB` tests still compile.

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum internal/usage/db_backend.go internal/usage/db_dialect.go internal/usage/db_schema.go internal/usage/usage_db.go internal/usage/db_backend_test.go
git commit -m "feat: introduce usage database backends"
```

### Task 3: Remove SQLite-Only SQL From Usage Stores

**Files:**

- Modify: `internal/usage/apikey_db.go`
- Modify: `internal/usage/model_config_db.go`
- Modify: `internal/usage/openrouter_model_sync.go`
- Modify: `internal/usage/pricing_db.go`
- Modify: `internal/usage/proxy_pool_db.go`
- Modify: `internal/usage/routing_config_db.go`
- Modify: `internal/usage/runtime_settings_db.go`
- Modify: `internal/usage/log_content_store.go`
- Test: existing tests in `internal/usage`

- [ ] **Step 1: Write failing dialect tests for store operations**

Add tests that run API key upsert/list/delete, routing config save/load, proxy pool save/load, model config save/load, and runtime settings save/load through a dialect-aware SQLite backend. These tests should assert that store functions call the dialect helpers rather than raw SQLite-only SQL.

Run:

```bash
go test ./internal/usage -run 'Test.*Dialect|TestAPIKey|TestRouting|TestProxyPool|TestRuntimeSettings' -count=1
```

Expected before implementation: failures where raw SQLite UPSERT or introspection paths are still assumed.

- [ ] **Step 2: Replace insert-ignore and upsert SQL**

Use dialect helpers for these patterns:

- SQLite: `INSERT OR IGNORE`, `ON CONFLICT(...) DO UPDATE`
- PostgreSQL: `ON CONFLICT(...) DO NOTHING`, `ON CONFLICT(...) DO UPDATE`
- MySQL: `INSERT IGNORE`, `ON DUPLICATE KEY UPDATE`

- [ ] **Step 3: Isolate SQLite maintenance**

Keep WAL checkpoint, PRAGMA, `VACUUM`, `freelist_count`, and SQLite page-size queries only in the SQLite dialect path. PostgreSQL and MySQL should skip SQLite maintenance without logging warnings.

- [ ] **Step 4: Replace column-exists introspection**

Implement dialect-specific column checks:

- SQLite: `PRAGMA table_info(table_name)`
- PostgreSQL: `information_schema.columns`
- MySQL: `information_schema.columns`

- [ ] **Step 5: Verify**

Run:

```bash
go test ./internal/usage -count=1
git diff --check
```

Expected: all `internal/usage` tests pass.

- [ ] **Step 6: Commit**

```bash
git add internal/usage
git commit -m "feat: make usage stores database dialect aware"
```

### Task 4: Wire Configured Database At Startup

**Files:**

- Modify: `internal/cmd/run.go`
- Modify: `internal/api/handlers/management/system_stats.go`
- Test: `internal/cmd/run_test.go` or focused helper tests

- [ ] **Step 1: Extract DB startup helper**

Create a helper that resolves database options from `cfg.Database`, `configPath`, request log storage, and timezone. Keep current SQLite default path exactly as `<config-dir>/data/usage.db`.

- [ ] **Step 2: Write failing startup helper tests**

Assert:

- Missing database config resolves to SQLite and the existing `data/usage.db` path.
- Explicit SQLite path is resolved relative to config directory when not absolute.
- PostgreSQL/MySQL use DSN and do not create `data/usage.db` as the active path.

Run:

```bash
go test ./internal/cmd -run 'TestResolveUsageDatabaseOptions' -count=1
```

Expected before implementation: compile failure because helper does not exist.

- [ ] **Step 3: Use backend-aware initialization**

Replace direct `dbPath := filepath.Join(dataDir, "usage.db")` and `usage.InitDB(dbPath, ...)` with the new helper and backend-aware initializer. Preserve legacy SQLite file migration only when the selected database is SQLite.

- [ ] **Step 4: Add database info to system stats**

Extend the system stats response with `database` metadata or add a helper consumed by the new `/database` handler. Preserve existing `db_size_bytes` for older panels.

- [ ] **Step 5: Verify**

Run:

```bash
go test ./internal/cmd ./internal/api/handlers/management -run 'TestResolveUsageDatabaseOptions|TestGetSystemStats' -count=1
git diff --check
```

Expected: focused tests pass.

- [ ] **Step 6: Commit**

```bash
git add internal/cmd/run.go internal/api/handlers/management/system_stats.go internal/cmd/run_test.go
git commit -m "feat: use configured usage database at startup"
```

### Task 5: Build Migration Service

**Files:**

- Create: `internal/usage/db_migration.go`
- Test: `internal/usage/db_migration_test.go`

- [ ] **Step 1: Write failing migration tests**

Use two temporary SQLite databases to prove the migration engine copies all known tables, preserves BLOB content in `request_log_content`, preserves JSON text arrays in `api_keys`, reports progress, and refuses to overwrite non-empty targets unless overwrite is true.

Run:

```bash
go test ./internal/usage -run 'TestDatabaseMigration' -count=1
```

Expected before implementation: compile failure because migration service types do not exist.

- [ ] **Step 2: Define migration model**

Implement:

```go
type MigrationStatus string

const (
    MigrationPending        MigrationStatus = "pending"
    MigrationRunning        MigrationStatus = "running"
    MigrationFailed         MigrationStatus = "failed"
    MigrationReadyForCutover MigrationStatus = "ready_for_cutover"
    MigrationCutoverComplete MigrationStatus = "cutover_complete"
)

type MigrationSnapshot struct {
    ID          string
    Status      MigrationStatus
    SourceType  string
    TargetType  string
    Phase       string
    Table       string
    CurrentRows int64
    TotalRows   int64
    Percent     int
    Message     string
    Error       string
    StartedAt   time.Time
    UpdatedAt   time.Time
}
```

- [ ] **Step 3: Implement preview**

Preview counts rows in every known table, checks whether target tables already contain rows, and returns warnings without modifying either database.

- [ ] **Step 4: Implement copy**

For each table:

- Select rows in stable primary-key order.
- Insert into target in batches.
- Update progress after every batch.
- Use target transactions per batch.

- [ ] **Step 5: Implement verification**

Verify:

- Every table row count matches.
- `request_log_content` byte lengths match.
- A deterministic sample of rows matches by primary key for configured sample count.

- [ ] **Step 6: Verify**

Run:

```bash
go test ./internal/usage -run 'TestDatabaseMigration' -count=1
go test ./internal/usage -count=1
git diff --check
```

Expected: migration tests and existing usage tests pass.

- [ ] **Step 7: Commit**

```bash
git add internal/usage/db_migration.go internal/usage/db_migration_test.go
git commit -m "feat: add database migration service"
```

### Task 6: Add Management Database APIs

**Files:**

- Create: `internal/api/handlers/management/database.go`
- Create: `internal/api/handlers/management/database_test.go`
- Modify: `internal/api/server.go`

- [ ] **Step 1: Write failing handler tests**

Cover:

- `GET /database` returns type, masked DSN, version, connection state, and row counts.
- `POST /database/test` masks DSN and does not persist config.
- `POST /database/migrations/preview` reports total rows and target state.
- `POST /database/migrations` returns a migration ID.
- `GET /database/migrations/:id` returns progress.
- Failed migration keeps active config unchanged.
- Confirm cutover writes `config.yaml` only for verified migration jobs.

Run:

```bash
go test ./internal/api/handlers/management -run 'TestDatabase' -count=1
```

Expected before implementation: compile failure because database handlers do not exist.

- [ ] **Step 2: Implement handlers**

Add handler methods:

```go
func (h *Handler) GetDatabaseInfo(c *gin.Context)
func (h *Handler) PostDatabaseTest(c *gin.Context)
func (h *Handler) PostDatabaseMigrationPreview(c *gin.Context)
func (h *Handler) PostDatabaseMigration(c *gin.Context)
func (h *Handler) GetDatabaseMigration(c *gin.Context)
func (h *Handler) PostDatabaseMigrationCutover(c *gin.Context)
```

- [ ] **Step 3: Register routes**

In `internal/api/server.go`, register the six routes under `/v0/management`.

- [ ] **Step 4: Verify**

Run:

```bash
go test ./internal/api/handlers/management -run 'TestDatabase' -count=1
go test ./internal/api -run 'Test' -count=1
git diff --check
```

Expected: focused management tests pass.

- [ ] **Step 5: Commit**

```bash
git add internal/api/server.go internal/api/handlers/management/database.go internal/api/handlers/management/database_test.go
git commit -m "feat: expose database migration management api"
```

### Task 7: Add PostgreSQL And MySQL Integration Tests

**Files:**

- Create: `internal/usage/db_integration_test.go`
- Modify: `.github/workflows/ci.yml` if this repository has a CI workflow suitable for DB services

- [ ] **Step 1: Write skip-by-default integration tests**

Use environment variables:

```bash
CLIRELAY_TEST_POSTGRES_DSN
CLIRELAY_TEST_MYSQL_DSN
```

Tests skip when the relevant DSN is empty. When DSNs are present, run schema creation, insert sample rows into all known tables, query via existing usage APIs, and migrate SQLite to the target database.

- [ ] **Step 2: Add optional CI services**

If the repository's CI has enough runtime budget, add PostgreSQL and MySQL services and set the two DSN environment variables only for the integration test job.

- [ ] **Step 3: Verify locally**

Run without DSNs:

```bash
go test ./internal/usage -run 'TestDatabaseIntegration' -count=1
```

Expected: tests skip cleanly.

Run with DSNs:

```bash
CLIRELAY_TEST_POSTGRES_DSN='postgres://postgres:postgres@127.0.0.1:5432/clirelay_test?sslmode=disable' \
CLIRELAY_TEST_MYSQL_DSN='root:root@tcp(127.0.0.1:3306)/clirelay_test?parseTime=true&charset=utf8mb4' \
go test ./internal/usage -run 'TestDatabaseIntegration' -count=1
```

Expected: integration tests pass against both databases.

- [ ] **Step 4: Commit**

```bash
git add internal/usage/db_integration_test.go .github/workflows
git commit -m "test: cover postgres and mysql usage databases"
```

### Task 8: Implement Management UI Flow

**Files:**

- Modify in frontend repository: system page components for `/manage/system`
- Modify in frontend repository: API client module for management database APIs
- Modify in frontend repository: shared modal/progress components if the project already has them
- Modify in backend repository after build: `manage.html` and `assets/*`

- [ ] **Step 1: Locate frontend source**

Use `remote-management.panel-github-repository` default repository `https://github.com/kittors/codeProxy` unless the active project setup uses a different management panel source.

- [ ] **Step 2: Add API client tests**

Add tests or typed client assertions for:

- `getDatabaseInfo`
- `testDatabaseConnection`
- `previewDatabaseMigration`
- `startDatabaseMigration`
- `getDatabaseMigration`
- `confirmDatabaseMigrationCutover`

- [ ] **Step 3: Add system page database card**

Display:

- Current database type
- Version
- Connection status
- Masked DSN
- Size and row summary
- "Migrate database" action

- [ ] **Step 4: Add migration modal**

Modal states:

- Target database selection
- DSN input
- Test connection
- Preview
- Running progress
- Ready for cutover
- Failed
- Complete

The running state polls every second and displays a stable progress bar, table name, percent, row count, and message.

- [ ] **Step 5: Sync built assets**

Build the frontend panel according to its repository instructions. Copy generated `manage.html` and `assets/*` into this backend repository only after the backend API tests pass.

- [ ] **Step 6: Verify UI**

Use a local dev server and browser automation to verify:

- System page renders without overlap on desktop and mobile widths.
- Database info card displays loaded data.
- Migration modal advances through test, preview, running, success, and failure states using mocked or local backend responses.
- Long DSNs are masked and do not overflow the modal.

- [ ] **Step 7: Commit**

```bash
git add manage.html assets
git commit -m "feat: add database migration management UI"
```

### Task 9: Documentation And Release Notes

**Files:**

- Modify: `README.md`
- Modify: `README_CN.md`
- Create or modify: `docs/database-migration.md`
- Modify: `CHANGELOG.md`

- [ ] **Step 1: Document database configuration**

Explain SQLite default behavior, PostgreSQL DSN, MySQL DSN, and the restart requirement after cutover confirmation.

- [ ] **Step 2: Document migration workflow**

Include:

- Backup recommendation
- Test connection
- Preview
- Run migration
- Confirm cutover
- Restart service
- Validate `/manage/system`

- [ ] **Step 3: Document rollback**

Rollback path:

- Stop service.
- Restore previous `config.yaml` database section.
- Start service.
- SQLite source remains untouched by migration.

- [ ] **Step 4: Verify docs**

Run:

```bash
git diff --check
```

Expected: no whitespace errors.

- [ ] **Step 5: Commit**

```bash
git add README.md README_CN.md CHANGELOG.md docs/database-migration.md
git commit -m "docs: document database migration workflow"
```

### Task 10: Full Verification

**Files:**

- All changed backend and frontend files

- [ ] **Step 1: Run Go unit tests**

```bash
go test ./internal/config ./internal/usage ./internal/api ./internal/api/handlers/management ./internal/cmd -count=1
```

Expected: all focused backend tests pass.

- [ ] **Step 2: Run full Go tests**

```bash
go test ./... -count=1
```

Expected: all tests pass.

- [ ] **Step 3: Run integration tests with PostgreSQL and MySQL**

```bash
CLIRELAY_TEST_POSTGRES_DSN='postgres://postgres:postgres@127.0.0.1:5432/clirelay_test?sslmode=disable' \
CLIRELAY_TEST_MYSQL_DSN='root:root@tcp(127.0.0.1:3306)/clirelay_test?parseTime=true&charset=utf8mb4' \
go test ./internal/usage -run 'TestDatabaseIntegration' -count=1
```

Expected: PostgreSQL and MySQL integration tests pass.

- [ ] **Step 4: Run frontend tests**

Run the frontend repository's test command, then run its build command. Use the exact package manager already used by that repository.

Expected: tests and build pass, generated panel assets are available.

- [ ] **Step 5: Run browser verification**

Start CliRelay locally with SQLite. Open `/manage/system`, test the migration modal states, and verify responsive layout.

Expected: database card and modal render correctly, progress updates once per second, and errors are visible without layout breakage.

- [ ] **Step 6: Final diff hygiene**

```bash
git status --short
git diff --check
```

Expected: only intended files are changed; no whitespace errors.

## Acceptance Criteria

- Existing users with no `database` config continue using SQLite at the existing `data/usage.db` location.
- PostgreSQL and MySQL can be selected in `config.yaml`.
- The service initializes schema for SQLite, PostgreSQL, and MySQL.
- All existing DB-backed usage APIs work on all three backends.
- `/v0/management/database` returns database type, masked DSN, version, connection status, and row counts.
- `/manage/system` displays database info consistently with the current UI style.
- Migration preview reports table row counts and refuses accidental overwrite.
- Migration progress is queryable every second through management API.
- The UI modal shows progress bar, current table, percent, status message, failure details, and cutover confirmation.
- Data is copied without row-count loss across all known DB-backed tables.
- `request_log_content` BLOB content remains readable after migration.
- Source DB remains untouched after successful or failed migration.
- `config.yaml` is updated only after verified migration and explicit cutover confirmation.
- Tests cover config parsing, dialect SQL, migration success, migration failure, API handlers, and UI states.

## Risk Register

- SQL dialect drift: reduce by centralizing placeholders, DDL, UPSERT, and introspection in dialect files.
- Large request logs: reduce by batch copy, progress snapshots, and per-batch target transactions.
- Existing target data: reduce by preview warnings and overwrite=false default.
- Live DB cutover complexity: reduce first version risk by requiring restart after successful config update.
- Management UI source split: reduce by implementing backend APIs first, then updating the panel source repository and syncing built assets.
- Sensitive DSN leakage: reduce by never returning raw DSNs from management APIs and adding tests for masking.
- SQLite maintenance assumptions: reduce by isolating PRAGMA, WAL checkpoint, freelist, and VACUUM to SQLite dialect only.

## Implementation Order Recommendation

Implement backend foundation before UI:

1. Database config.
2. Backend and dialect abstraction.
3. Usage store SQL cleanup.
4. Startup wiring.
5. Migration service.
6. Management APIs.
7. PostgreSQL/MySQL integration tests.
8. Management UI.
9. Documentation.
10. Full verification and merge.

This order keeps each step independently testable and avoids building a UI over unstable backend contracts.
