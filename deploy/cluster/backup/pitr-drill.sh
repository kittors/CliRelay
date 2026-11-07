#!/usr/bin/env bash
# Restore drill for the CliRelay cluster backups, run on the arbiter host.
#
# Restores a base backup into a throwaway PostgreSQL that has no network,
# replays the WAL archive to its end (timeline "latest", including the
# segment pg_receivewal is still writing) and reports what it reached. It
# never writes to the archive or talks to the cluster.
#
# The drill fails (exit 1) unless the replay reaches the newest timeline in
# the archive and its last transaction is at most MAX_LAG_SECONDS old. A
# restore that silently stops at the end of the base backup still starts a
# healthy-looking database, on a timeline ID that the live cluster has
# already used; only these two checks tell it apart.
#
# Usage: pitr-drill.sh [base-YYYYmmddTHHMMSSZ.tar.gz]
#   Without an argument it restores the second newest backup, so the replay
#   crosses at least a day of WAL.
# Config: /etc/clirelay-cluster/backup.env (BACKUP_DIR, PG_IMAGE)
#   MAX_LAG_SECONDS=900   PG_SUPERUSER=cliproxy
#   DRILL_MEMORY=768m (optional cap for the throwaway instance, for a small
#   offsite host that also runs other services)
set -euo pipefail
# shellcheck source=deploy/cluster/backup/backup-lib.sh
. "$(dirname "$0")/backup-lib.sh"
MAX_LAG_SECONDS="${MAX_LAG_SECONDS:-900}"
PG_SUPERUSER="${PG_SUPERUSER:-cliproxy}"
# copy_needed_wal copies from the archive SRC to DST what a backup starting at
# segment START can replay: history files, and segments from START on (log
# and segment numbers compared, across timelines). The archive holds KEEP
# days of WAL; copying all of it every week is gigabytes of I/O on a small
# offsite host. An empty START copies everything.
copy_needed_wal() {
  local f b
  for f in "$1"/*; do
    [ -f "$f" ] || continue
    b="$(basename "$f")"
    case "$b" in
    *.history | *.history.gz) cp -p "$f" "$2/" ;;
    *)
      if [ -z "$3" ] || [[ ! "${b:8:16}" < "${3:8:16}" ]]; then
        cp -p "$f" "$2/"
      fi
      ;;
    esac
  done
}

# control_file_params turns pg_controldata output into postgres -c arguments,
# one per line, for the settings recovery insists be at least the primary's.
control_file_params() {
  local key value
  while IFS=: read -r key value; do
    value="$(echo "$value" | tr -d ' ')"
    case "$key" in
    "max_connections setting") printf '%s\n' -c "max_connections=$value" ;;
    "max_worker_processes setting") printf '%s\n' -c "max_worker_processes=$value" ;;
    "max_wal_senders setting") printf '%s\n' -c "max_wal_senders=$value" ;;
    "max_prepared_xacts setting") printf '%s\n' -c "max_prepared_transactions=$value" ;;
    "max_locks_per_xact setting") printf '%s\n' -c "max_locks_per_transaction=$value" ;;
    esac
  done
}

WAL="$BACKUP_DIR/wal"
D="$BACKUP_DIR/.drill"
C=clirelay-pitr-drill

if [ $# -ge 1 ]; then
  bk="$BACKUP_DIR/$1"
else
  bk="$(ls -1t "$BACKUP_DIR"/base-*.tar.gz 2>/dev/null | sed -n 2p || true)"
  [ -n "$bk" ] || bk="$(ls -1t "$BACKUP_DIR"/base-*.tar.gz 2>/dev/null | head -n 1 || true)"
fi
[ -n "$bk" ] && [ -f "$bk" ] || { echo "no base backup found in $BACKUP_DIR" >&2; exit 1; }

cleanup() { docker rm -f "$C" >/dev/null 2>&1 || true; rm -rf "$D"; }
trap cleanup EXIT
cleanup
mkdir -p "$D/data/pgdata" "$D/wal"
tar -xzf "$bk" -C "$D/data/pgdata"
# Read now: recovery renames backup_label to backup_label.old when it ends.
start_tli="$(sed -n 's/^START TIMELINE: //p' "$D/data/pgdata/backup_label")"
# The archive directory is 0700 root and PostgreSQL runs as uid 70, so
# restore_command could not read it in place and every fetch would fail.
start_seg="$(sed -n 's/^START WAL LOCATION: .*(file \([0-9A-F]\{24\}\)).*/\1/p' "$D/data/pgdata/backup_label")"
copy_needed_wal "$WAL" "$D/wal" "$start_seg"

newest_tli="$(ls "$D/wal" | grep -E '^[0-9A-F]{8}\.history\.gz$' | sort | tail -n 1 | cut -c1-8 || true)"
[ -n "$newest_tli" ] || newest_tli="$(printf '%08X' "${start_tli:-1}")"
# pg_receivewal is still writing the newest segment. Its gzip stream has no
# trailer yet, so gunzip stops with an error after the last complete block;
# pad what it wrote to a whole segment, and the zeros read as end of WAL.
partial="$(ls "$D/wal" | grep -E "^${newest_tli}[0-9A-F]{16}\.gz\.partial$" | sort | tail -n 1 || true)"
if [ -n "$partial" ]; then
  seg="$D/data/pgdata/pg_wal/${partial%.gz.partial}"
  gunzip -c "$D/wal/$partial" >"$seg" 2>/dev/null || true
  truncate -s 16M "$seg"
fi
rm -f "$D/data/pgdata/postmaster.pid" "$D/data/pgdata/standby.signal"
touch "$D/data/pgdata/recovery.signal"
chown -R 70:70 "$D"
chmod 700 "$D/data/pgdata"

# Recovery refuses to run with max_connections and friends below what the
# primary used, and the primary may set them on its command line (hk-155
# runs "-c max_connections=200") rather than in the postgresql.conf the
# backup carries. Start from the values recorded in the backup's control file.
hot_standby_params=()
while IFS= read -r arg; do
  hot_standby_params+=("$arg")
done < <(docker run --rm -u 70:70 -v "$D/data:/var/lib/postgresql/data" "$PG_IMAGE" \
  pg_controldata /var/lib/postgresql/data/pgdata | control_file_params)

started=$(date +%s)
# --entrypoint postgres: the image's entrypoint looks for PG_VERSION in PGDATA
# (the parent of pgdata here) and would try to initdb instead.
docker run -d --name "$C" --network none -u 70:70 --entrypoint postgres \
  ${DRILL_MEMORY:+--memory "$DRILL_MEMORY"} \
  -v "$D/data:/var/lib/postgresql/data" -v "$D/wal:/wal:ro" "$PG_IMAGE" \
  -D /var/lib/postgresql/data/pgdata -c listen_addresses= -c port=55999 -c ssl=off \
  -c "restore_command=gunzip -c /wal/%f.gz > %p" -c recovery_target_timeline=latest \
  -c archive_mode=off -c primary_conninfo= -c primary_slot_name= -c synchronous_standby_names= \
  ${hot_standby_params[@]+"${hot_standby_params[@]}"} -c shared_buffers=128MB >/dev/null

q() { docker exec "$C" psql -h /var/run/postgresql -p 55999 -U "$PG_SUPERUSER" -d "$1" -X -At -c "$2" </dev/null 2>/dev/null; }
done_ok=0
for _ in $(seq 1 180); do
  [ "$(docker inspect -f '{{.State.Running}}' "$C" 2>/dev/null)" = "true" ] || break
  if [ "$(q postgres 'select pg_is_in_recovery()')" = "f" ]; then done_ok=1; break; fi
  sleep 5
done
elapsed=$(($(date +%s) - started))
logs="$(docker logs "$C" 2>&1)"

echo "backup:  $(basename "$bk") (timeline ${start_tli:-?})"
echo "replay:  $(printf '%s\n' "$logs" | grep -oE 'restored log file "[0-9A-F]{8}' | cut -d'"' -f2 | sort | uniq -c |
  while read -r n t; do printf 'TL %d x%d  ' "$((16#$t))" "$n"; done || true)"
if [ "$done_ok" -ne 1 ]; then
  echo "FAIL: recovery did not finish within ${elapsed}s"
  printf '%s\n' "$logs" | tail -n 20 | sed 's/^/  /'
  exit 1
fi

new_tli="$(printf '%s\n' "$logs" | sed -n 's/.*selected new timeline ID: \([0-9]*\).*/\1/p' | tail -n 1)"
last_xact="$(printf '%s\n' "$logs" | sed -n 's/.*last completed transaction was at log time \(.*\)$/\1/p' | tail -n 1)"
lag=$(($(date +%s) - $(date -d "$last_xact" +%s 2>/dev/null || echo 0)))
echo "reached: timeline $((16#$newest_tli)) -> new timeline ${new_tli:-?} in ${elapsed}s; last transaction ${last_xact:-unknown} (${lag}s ago)"
if [ -n "$(q cliproxy "select to_regclass('public.request_logs')")" ]; then
  echo "rows:    request_logs $(q cliproxy "select count(*) || ' rows, max id ' || coalesce(max(id), 0) from request_logs")"
fi

if [ "${new_tli:-0}" -le "$((16#$newest_tli))" ]; then
  echo "FAIL: replay stopped before the newest archived timeline ($((16#$newest_tli))); check restore_command and the archive"
  exit 1
fi
if [ -z "$last_xact" ] || [ "$lag" -gt "$MAX_LAG_SECONDS" ]; then
  echo "FAIL: the last replayed transaction is ${lag}s old (limit ${MAX_LAG_SECONDS}s); the archive is behind"
  exit 1
fi
echo "OK"
