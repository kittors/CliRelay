# Sourced by the backup scripts in this directory; not run on its own.
#
# Loads the backup config and works out how a pg_basebackup / pg_receivewal
# container reaches PostgreSQL. Two layouts share one set of scripts:
#
# - cluster (BACKUP_HOSTS set): the arbiter host connects over the network
#   with the cluster's TLS client certificate, as the replicator role, and
#   prefers a standby so the primary does no extra work.
# - single node (PG_CONTAINER set): the database runs in one container on the
#   application host. The tools join that container's network namespace and
#   connect to its own loopback, which the stock pg_hba.conf trusts for
#   replication. The database port stays bound to 127.0.0.1 on the host, so
#   taking backups exposes nothing new.
#
# BACKUP_ENV only exists so tests can point at a scratch config; the systemd
# units never set it and these scripts are not reachable through sudo.
set -a
# shellcheck disable=SC1090
. "${BACKUP_ENV:-/etc/clirelay-cluster/backup.env}"
set +a
BACKUP_DIR="${BACKUP_DIR:-/opt/clirelay-cluster/backups}"
PG_IMAGE="${PG_IMAGE:-postgres:15.19-alpine3.24}"
KEEP="${KEEP:-7}"

# backup_conn_setup sets PG_DOCKER_NET, PG_DOCKER_TLS and PG_DOCKER_LIMITS
# (docker run arguments), PG_CONN (a libpq conninfo) and PG_LAYOUT. Only the
# scripts that talk to the database call it; the drill and the offsite pull
# never need a connection.
backup_conn_setup() {
  if [ -n "${PG_CONTAINER:-}" ]; then
    PG_LAYOUT=single
    PG_DOCKER_NET="container:$PG_CONTAINER"
    PG_DOCKER_TLS=()
    # The backup shares the host with live traffic. The unit's Nice= and
    # IOSchedulingClass= reach only the docker CLI, not the container, so
    # cap the container itself at one core.
    PG_DOCKER_LIMITS=(--cpus=1)
    PG_CONN="host=127.0.0.1 port=5432 user=${PG_SUPERUSER:-cliproxy} connect_timeout=10"
    return
  fi
  PG_DOCKER_LIMITS=()
  : "${BACKUP_HOSTS:?set BACKUP_HOSTS (cluster) or PG_CONTAINER (single node) in backup.env}"
  local ports
  ports="$(echo "$BACKUP_HOSTS" | tr ',' '\n' | sed "s/.*/${BACKUP_PORT:-55432}/" | paste -sd, -)"
  PG_LAYOUT=cluster
  PG_DOCKER_NET=host
  PG_DOCKER_TLS=(-v /etc/clirelay-cluster/tls:/tls:ro)
  # prefer-standby: take the copy from a replica when one is up
  PG_CONN="host=$BACKUP_HOSTS port=$ports user=replicator target_session_attrs=prefer-standby connect_timeout=10 sslmode=verify-ca sslrootcert=/tls/ca.crt sslcert=/tls/node.crt sslkey=/tls/node.key"
}

# backup_start_wal prints the first WAL segment a base backup needs, read
# from the backup_label inside it.
backup_start_wal() {
  { tar -xzOf "$1" ./backup_label 2>/dev/null || true; } |
    sed -n 's/^START WAL LOCATION: .*(file \([0-9A-F]\{24\}\)).*/\1/p'
}

# backup_prune keeps the newest KEEP base backups in BACKUP_DIR and removes
# WAL segments older than the oldest one kept: no backup that still exists
# can replay them. It runs only after a new backup verified (on the database
# host) or after a pull (offsite), so a failing run never deletes anything.
backup_prune() {
  { ls -1t "$BACKUP_DIR"/base-*.tar.gz 2>/dev/null || true; } | tail -n +"$((KEEP + 1))" | while read -r old; do
    rm -f -- "$old"
    echo "removed $(basename "$old")"
  done
  local oldest start_wal
  oldest="$({ ls -1t "$BACKUP_DIR"/base-*.tar.gz 2>/dev/null || true; } | tail -n 1)"
  if [ -n "$oldest" ] && [ -d "$BACKUP_DIR/wal" ]; then
    start_wal="$(backup_start_wal "$oldest")"
    if [ -n "$start_wal" ]; then
      docker run --rm -v "$BACKUP_DIR/wal:/wal" "$PG_IMAGE" pg_archivecleanup -x .gz /wal "$start_wal"
      echo "wal archive pruned before $start_wal (oldest backup $(basename "$oldest"))"
    fi
  fi
}
