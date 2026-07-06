# PostgreSQL and Redis Runtime Data Stack

CliRelay now uses PostgreSQL 15+ as the runtime primary database. Redis 7+ is used only for cache, locks, limits, queues, and rebuildable runtime snapshots. Business facts such as API keys, routing, proxy pool, request logs, request log content, model config, pricing, identity fingerprints, and quota records must be recoverable from PostgreSQL.

## Configure

For Docker Compose, the bundled `docker-compose.yml` starts `clirelay-init`, `postgres:15-alpine`, and `redis:7-alpine`. A pre-existing `.env` is optional. On the first `docker compose up -d`, `clirelay-init` creates `.env`, preserves existing values, generates missing secrets, and creates `config.yaml` from `config.example.yaml` if it is missing. The application container then sources `.env` at startup and receives:

- `CLIRELAY_POSTGRES_DSN`
- `CLIRELAY_REDIS_ENABLE`
- `CLIRELAY_REDIS_ADDR`
- `CLIRELAY_REDIS_PASSWORD`
- `CLIRELAY_REDIS_DB`

For non-Compose deployments, set the same values in the environment or edit `postgres.dsn` and `redis.*` in `config.yaml`.

For the native `relay.07230805.xyz` style deployment, prepare the local PostgreSQL/Redis stack with one reproducible script:

```bash
BASE_DIR=/opt/clirelay2 /opt/clirelay2/scripts/prepare-runtime-data-stack.sh
```

The default `up` action creates `/opt/clirelay2/runtime-data-stack/docker-compose.yml`, writes `/opt/clirelay2/runtime-data-stack/.env`, starts `postgres:15-alpine` and `redis:7-alpine`, waits for health checks, and verifies `psql`/`redis-cli`. PostgreSQL and Redis bind only to `127.0.0.1` by default.

This preparation step does not write `/opt/clirelay2/.env`, does not restart CliRelay, does not import SQLite, and does not switch the running service to PostgreSQL/Redis.

Only after local checks pass and the cutover is approved, activate the generated environment for CliRelay:

```bash
BASE_DIR=/opt/clirelay2 /opt/clirelay2/scripts/prepare-runtime-data-stack.sh activate-env
```

After `activate-env`, `scripts/deploy-blue-green.sh` can read `/opt/clirelay2/.env` and run the SQLite import during the approved deploy.

To tear down an unneeded test stack without deleting data, run:

```bash
BASE_DIR=/opt/clirelay2 /opt/clirelay2/scripts/prepare-runtime-data-stack.sh down
```

To reset a disposable test stack and delete its generated PostgreSQL/Redis data directories:

```bash
CLIRELAY_RUNTIME_CONFIRM_RESET=YES_DELETE_CLIRELAY_RUNTIME_DATA \
BASE_DIR=/tmp/clirelay2-test \
./scripts/prepare-runtime-data-stack.sh reset
```

## Docker Compose Auto Migration

For Docker Compose deployments, the default path is:

```bash
docker compose up -d
```

Compose first runs `clirelay-init`, then starts PostgreSQL 15 and Redis 7, waits for their health checks, and finally the CliRelay entrypoint runs `scripts/migrate-sqlite-to-postgres.sh` before starting the API server. The script looks for a legacy SQLite database in these paths unless `CLIRELAY_SQLITE_PATH` is set:

- `/CLIProxyAPI/data/usage.db`
- `/CLIProxyAPI/usage.db`
- `/CLIProxyAPI/logs/usage.db`
- `./data/usage.db`
- `./usage.db`
- `./logs/usage.db`

If no legacy SQLite file exists, the container starts normally with PostgreSQL. If a legacy SQLite file exists, the Docker Compose path provides `CLIRELAY_POSTGRES_DSN` automatically; non-Compose deployments must set it explicitly. The script runs read-only SQLite inventory, PostgreSQL import dry-run, then apply import by default. Set `CLIRELAY_SQLITE_AUTO_IMPORT=false` to stop after dry-run, or `CLIRELAY_SQLITE_AUTO_MIGRATE=false` to skip the startup migration hook.

For old Docker deployments with a SQLite-only compose file, the online updater upgrades `docker-compose.yml` and `.env` first, adding `clirelay-init`, PostgreSQL, Redis, and generated defaults, then runs the full compose update. If the old deployment mounted files in a way that prevents the updater from writing them, replace `docker-compose.yml` with the latest repository version and run `docker compose up -d` once.

SQLite is never deleted, moved, or written by this migration path. PostgreSQL records a source fingerprint in `sqlite_import_runs` after a successful apply. Repeated container starts skip an already imported SQLite source, and PostgreSQL advisory locking ensures concurrent starts do not import the same source twice.

If migration fails, the container exits instead of starting against an empty PostgreSQL database.

## Non-Docker Migration Script

For non-Docker deployments, build or download the new CliRelay binary, set `CLIRELAY_POSTGRES_DSN`, then run:

```bash
CLIRELAY_BIN=/opt/clirelay2/cli-proxy-api-new \
CLIRELAY_POSTGRES_DSN='postgres://user:pass@127.0.0.1:5432/cliproxy?sslmode=disable' \
./scripts/migrate-sqlite-to-postgres.sh /opt/clirelay2/usage.db
```

The same script performs inventory, dry-run, apply, idempotency marking, and leaves SQLite in place.

## Deploy Gate

Pushes to `dev` build the Linux binary but do not deploy to the server unless repository variable `CLIRELAY_DEV_AUTO_DEPLOY` is set to `true`. Prefer leaving it disabled for this migration and running the deploy workflow manually after PostgreSQL, Redis, SQLite import dry-run/apply, and row-count/checksum checks are complete.

The blue-green deploy script refuses to proceed unless `postgres.dsn` or `CLIRELAY_POSTGRES_DSN` is configured. If Redis is enabled, it also requires `redis.addr` or `CLIRELAY_REDIS_ADDR`.

## Inspect Legacy SQLite

Run the read-only inventory before importing any old `usage.db`:

```bash
./cli-proxy-api -sqlite-dry-run /path/to/usage.db
```

The command opens SQLite read-only and prints table names, columns, row counts, numeric ID ranges, time ranges, checksums, and `dry_run_only: true`. It does not print row contents.

Run the import dry-run against PostgreSQL before applying:

```bash
CLIRELAY_POSTGRES_DSN='postgres://user:pass@127.0.0.1:5432/cliproxy?sslmode=disable' \
./cli-proxy-api -sqlite-import /path/to/usage.db
```

The import command runs PostgreSQL migrations, compares source/target columns, row counts, and checksums, and reports planned inserts without writing by default. Apply only after the report is reviewed:

```bash
CLIRELAY_POSTGRES_DSN='postgres://user:pass@127.0.0.1:5432/cliproxy?sslmode=disable' \
./cli-proxy-api -sqlite-import /path/to/usage.db -sqlite-import-dry-run=false
```

The apply path writes in batches, uses `ON CONFLICT DO NOTHING`, and resets PostgreSQL identity sequences for imported tables with generated IDs.

## Validate PostgreSQL

Run Ent codegen and tests:

```bash
rtk go generate ./internal/storage/postgres/ent
rtk go test ./internal/storage/postgres/... ./internal/cmd -count=1
rtk go test ./internal/usage -count=1
rtk go test ./...
```

With PostgreSQL 15+ and Redis 7+ available:

```bash
CLIRELAY_POSTGRES_TEST_DSN='postgres://cliproxy:cliproxy@127.0.0.1:55432/cliproxy?sslmode=disable' \
CLIRELAY_REDIS_TEST_ADDR='127.0.0.1:56379' \
rtk go test ./internal/usage -run TestPostgresRuntimeDataStackIntegration -count=1 -v

CLIRELAY_POSTGRES_TEST_DSN='postgres://cliproxy:cliproxy@127.0.0.1:55432/cliproxy?sslmode=disable' \
rtk go test ./internal/storage/postgres/sqliteinventory -run TestImportSQLiteDryRunAndApply -count=1 -v
```

## Rollback Boundary

Keep the original SQLite file as a read-only backup until row counts, ID/time ranges, checksums, key CRUD, management queries, request log content, cache fallback, and quota/identity paths have been verified against PostgreSQL. Do not run the migrated service against SQLite as a runtime primary database.
