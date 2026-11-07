#!/usr/bin/env bash
# Foreground pg_receivewal for clirelay-pg-receivewal.service: the continuous
# WAL archive behind point-in-time recovery.
#
# Replication protects against a lost machine, not against a bad DELETE: a
# mistake is replicated in milliseconds. Base backups plus every WAL segment
# since the oldest one let an operator restore to any moment in between.
#
# Cluster: pg_receivewal follows the primary through WAL_ARCHIVE_DSN and the
# permanent slot clirelay_walarchive that Patroni keeps on every member, so a
# failover neither loses segments nor needs reconfiguration.
#
# Single node: nothing manages slots, so this creates clirelay_walarchive the
# first time. A slot makes the server keep WAL while the archiver is down;
# without a cap, an archiver that stays stopped fills the database disk and
# takes production down with it. The script therefore refuses to create the
# slot unless max_slot_wal_keep_size is set (ALTER SYSTEM, then
# pg_reload_conf(); no restart needed). Past the cap the server drops the
# retained WAL, the slot is invalidated and this service fails loudly instead.
set -euo pipefail
# The unit also loads backup.env through systemd's EnvironmentFile=, as it
# always has. systemd keeps an unquoted value with spaces whole, while
# sourcing the file below would cut WAL_ARCHIVE_DSN at the first space, so
# the value systemd passed wins.
dsn_from_unit="${WAL_ARCHIVE_DSN:-}"
# shellcheck source=deploy/cluster/backup/backup-lib.sh
. "$(dirname "$0")/backup-lib.sh"
backup_conn_setup
SLOT=clirelay_walarchive
if [ "$PG_LAYOUT" = cluster ]; then
  PG_CONN="${dsn_from_unit:-${WAL_ARCHIVE_DSN:?set WAL_ARCHIVE_DSN in backup.env}}"
fi

install -d -m 0700 "$BACKUP_DIR/wal"
docker rm -f clirelay-walarchive >/dev/null 2>&1 || true

if [ "$PG_LAYOUT" = single ]; then
  cap="$(docker run --rm --network "$PG_DOCKER_NET" "$PG_IMAGE" \
    psql "$PG_CONN dbname=postgres" -X -At -c 'show max_slot_wal_keep_size')"
  if [ -z "$cap" ] || [ "$cap" = "-1" ]; then
    echo "max_slot_wal_keep_size is unlimited; set it before archiving, e.g." >&2
    echo "  ALTER SYSTEM SET max_slot_wal_keep_size = '4GB'; SELECT pg_reload_conf();" >&2
    exit 1
  fi
  docker run --rm --network "$PG_DOCKER_NET" "$PG_IMAGE" \
    pg_receivewal --create-slot --if-not-exists --slot="$SLOT" --dbname="$PG_CONN"
fi

exec docker run --rm --name clirelay-walarchive --network "$PG_DOCKER_NET" -e PGPASSWORD \
  ${PG_DOCKER_TLS[@]+"${PG_DOCKER_TLS[@]}"} -v "$BACKUP_DIR/wal:/wal" \
  "$PG_IMAGE" pg_receivewal -D /wal --slot="$SLOT" --compress=gzip --dbname="$PG_CONN"
