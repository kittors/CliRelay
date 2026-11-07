#!/usr/bin/env bash
# Daily physical base backup of the CliRelay database.
#
# Cluster: taken on the arbiter host, so a copy of the data lives on a
# machine that serves no traffic, from whichever member answers first in
# BACKUP_HOSTS (a replica is preferred, so the primary does no extra work).
# Single node: taken on the application host through the database
# container's loopback; offsite-pull.sh then copies it to another machine.
#
# The backup is verified with pg_verifybackup against its manifest, then
# compressed. Old backups beyond KEEP are removed only after the new one
# verified.
#
# Config: /etc/clirelay-cluster/backup.env (root, 0600)
#   cluster:     BACKUP_HOSTS="198.51.100.20,43.255.122.4"   BACKUP_PORT=55432
#                PGPASSWORD=<replicator password>
#   single node: PG_CONTAINER=clirelay-pg   PG_SUPERUSER=cliproxy
#   both:        BACKUP_DIR=/opt/clirelay-cluster/backups   KEEP=7
#                PG_IMAGE=postgres:15.19-alpine3.24   MAX_RATE=30M
#                COMPRESS=server-zstd (cluster default) or none
#                CHECKPOINT=fast (cluster default) or spread (single-node default)
#
# With server-side compression the member compresses what it streams and
# pg_basebackup decompresses it before writing the plain directory, so
# pg_verifybackup still checks every file. The arbiter's link is often
# metered; this cuts the daily transfer to about a quarter for a few seconds
# of CPU on the member. A single node streams over its own loopback, so it
# skips that. It also defaults to a spread checkpoint: the source is the
# primary that is serving traffic, and a fast checkpoint would flush every
# dirty buffer at once.
set -euo pipefail
# shellcheck source=deploy/cluster/backup/backup-lib.sh
. "$(dirname "$0")/backup-lib.sh"
backup_conn_setup
MAX_RATE="${MAX_RATE:-30M}"
if [ "$PG_LAYOUT" = single ]; then
  COMPRESS="${COMPRESS:-none}"
  CHECKPOINT="${CHECKPOINT:-spread}"
else
  COMPRESS="${COMPRESS:-server-zstd}"
  CHECKPOINT="${CHECKPOINT:-fast}"
fi

name="base-$(date -u +%Y%m%dT%H%M%SZ)"
work="$BACKUP_DIR/.tmp-$name"
mkdir -p "$BACKUP_DIR"
chmod 700 "$BACKUP_DIR"
trap 'rm -rf "$work" "$work.tar.gz"' EXIT

docker run --rm --network "$PG_DOCKER_NET" -e PGPASSWORD \
  ${PG_DOCKER_TLS[@]+"${PG_DOCKER_TLS[@]}"} ${PG_DOCKER_LIMITS[@]+"${PG_DOCKER_LIMITS[@]}"} \
  -v "$BACKUP_DIR:/backups" \
  "$PG_IMAGE" sh -c "
    set -e
    pg_basebackup -d '$PG_CONN' -D /backups/.tmp-$name -Fp -X stream -c $CHECKPOINT --max-rate=$MAX_RATE --compress=$COMPRESS --progress >/dev/null
    nice -n 19 pg_verifybackup -q /backups/.tmp-$name
    nice -n 19 tar -C /backups/.tmp-$name -czf /backups/.tmp-$name.tar.gz .
    chmod 600 /backups/.tmp-$name.tar.gz
    mv /backups/.tmp-$name.tar.gz /backups/$name.tar.gz
  "
size="$(du -h "$BACKUP_DIR/$name.tar.gz" | cut -f1)"
echo "backup $name.tar.gz ($size) verified"

backup_prune
