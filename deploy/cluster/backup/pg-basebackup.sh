#!/usr/bin/env bash
# Daily physical base backup of the CliRelay cluster, taken on the arbiter
# host so a copy of the data lives on a machine that serves no traffic.
#
# The backup is taken from whichever member answers first in BACKUP_HOSTS
# (a replica is preferred, so the primary does no extra work), verified with
# pg_verifybackup against its manifest, then compressed. Old backups beyond
# KEEP are removed only after the new one verified.
#
# Config: /etc/clirelay-cluster/backup.env (root, 0600)
#   BACKUP_HOSTS="198.51.100.20,43.255.122.4"   BACKUP_PORT=55432
#   PGPASSWORD=<replicator password>              KEEP=7
#   BACKUP_DIR=/opt/clirelay-cluster/backups      PG_IMAGE=postgres:15.19-alpine3.24
set -euo pipefail
set -a
. /etc/clirelay-cluster/backup.env
set +a
BACKUP_DIR="${BACKUP_DIR:-/opt/clirelay-cluster/backups}"
PG_IMAGE="${PG_IMAGE:-postgres:15.19-alpine3.24}"
KEEP="${KEEP:-7}"
MAX_RATE="${MAX_RATE:-30M}"
ports="$(echo "$BACKUP_HOSTS" | tr ',' '\n' | sed "s/.*/${BACKUP_PORT:-55432}/" | paste -sd, -)"
# prefer-standby: take the copy from a replica when one is up
conn="host=$BACKUP_HOSTS port=$ports user=replicator target_session_attrs=prefer-standby connect_timeout=10 sslmode=verify-ca sslrootcert=/tls/ca.crt sslcert=/tls/node.crt sslkey=/tls/node.key"

name="base-$(date -u +%Y%m%dT%H%M%SZ)"
work="$BACKUP_DIR/.tmp-$name"
mkdir -p "$BACKUP_DIR"
chmod 700 "$BACKUP_DIR"
trap 'rm -rf "$work"' EXIT

docker run --rm --network host -e PGPASSWORD \
  -v /etc/clirelay-cluster/tls:/tls:ro -v "$BACKUP_DIR:/backups" \
  "$PG_IMAGE" sh -c "
    set -e
    pg_basebackup -d '$conn' -D /backups/.tmp-$name -Fp -X stream -c fast --max-rate=$MAX_RATE --progress >/dev/null
    pg_verifybackup -q /backups/.tmp-$name
    tar -C /backups/.tmp-$name -czf /backups/$name.tar.gz .
    chmod 600 /backups/$name.tar.gz
  "
size="$(du -h "$BACKUP_DIR/$name.tar.gz" | cut -f1)"
echo "backup $name.tar.gz ($size) verified"

# retention: newest $KEEP verified backups
ls -1t "$BACKUP_DIR"/base-*.tar.gz 2>/dev/null | tail -n +"$((KEEP + 1))" | while read -r old; do
  rm -f -- "$old"
  echo "removed $(basename "$old")"
done

# WAL archive (clirelay-pg-receivewal): keep every segment the oldest kept
# backup needs and everything after it; older segments cannot be replayed
# onto any backup that still exists.
oldest="$(ls -1t "$BACKUP_DIR"/base-*.tar.gz 2>/dev/null | tail -n 1)"
if [ -n "$oldest" ] && [ -d "$BACKUP_DIR/wal" ]; then
  start_wal="$(tar -xzOf "$oldest" ./backup_label 2>/dev/null | sed -n 's/^START WAL LOCATION: .*(file \([0-9A-F]\{24\}\)).*/\1/p')"
  if [ -n "$start_wal" ]; then
    docker run --rm -v "$BACKUP_DIR/wal:/wal" "$PG_IMAGE" pg_archivecleanup -x .gz /wal "$start_wal"
    echo "wal archive pruned before $start_wal (oldest backup $(basename "$oldest"))"
  fi
fi
