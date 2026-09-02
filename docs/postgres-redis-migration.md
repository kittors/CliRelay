# PostgreSQL / Redis Runtime

## Runtime boundary

CliRelay runs exclusively on PostgreSQL 15+, Redis 7+, and Ent ORM.

- PostgreSQL is the only persistent runtime data source.
- Redis stores cache, locks, rate limits, queues, and rebuildable state only.
- The application entrypoint does not scan for legacy database files and does not run a data importer at startup, health checks, blue-green deploys, or OTA updates.
- Stack upgrades remove a stale `clirelay-migrate` service and leftover `CLIRELAY_SQLITE_*` application environment entries from old compose files. They do not delete files on disk.

For Docker Compose deployments:

```bash
docker compose up -d
```

The stack starts `clirelay-init`, PostgreSQL, Redis, the application, and `clirelay-updater`. OTA state is stored by the updater in `.clirelay-updater-status.json` and streamed to the management API through SSE.

## Old Compose and updater transition

For a deployment that still uses an old compose file:

1. Back up `docker-compose.yml`, `.env`, and PostgreSQL data.
2. Replace `docker-compose.yml` with the current repository version.
3. Start the runtime dependencies and updater:

   ```bash
   docker compose up -d postgres redis clirelay-updater
   ```

4. Start or recreate the application service.

An updater sidecar from a release that predates the SSE protocol must be recreated once:

```bash
docker compose up -d --force-recreate clirelay-updater
```

After that transition, the management panel receives real updater snapshots and can recover after API-container restarts or temporary SSE disconnects.

## Validation

Run the repository tests:

```bash
rtk go test ./cmd/updater -count=1
rtk go test ./internal/management/updateflow ./internal/api/handlers/management -count=1
rtk go test ./internal/storage/postgres/... ./internal/usage -count=1
rtk go test ./...
```

With integration services available:

```bash
CLIRELAY_POSTGRES_TEST_DSN='postgres://cliproxy:cliproxy@127.0.0.1:55432/cliproxy?sslmode=disable' \
CLIRELAY_REDIS_TEST_ADDR='127.0.0.1:56379' \
rtk go test ./internal/usage -run TestPostgresRuntimeDataStackIntegration -count=1 -v
```
