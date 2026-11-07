#!/usr/bin/env bash
# Copies the backup set from the database host to this machine, which should
# sit with another provider in another region.
#
# This host pulls; the database host pushes nothing and holds no credential
# for this one. On the database host the key is pinned in authorized_keys to
#   restrict,command="/usr/bin/rrsync -ro /opt/clirelay-cluster/backups" ssh-ed25519 ...
# so it can read the backup directory and nothing else. A compromised
# database host therefore cannot reach, rewrite or delete the copies here.
# rsync runs without --delete for the same reason: this host prunes its own
# copies (newest KEEP base backups and the WAL they need) after each pull.
#
# In-progress files are skipped: pg-basebackup.sh writes .tmp-* and renames a
# backup into place only once it verified. The segment pg_receivewal is still
# writing (*.partial) is copied as it stands; pitr-drill.sh knows how to
# replay a truncated one.
#
# Config: /etc/clirelay-cluster/backup.env on this host
#   PULL_SOURCE=root@203.0.113.10   PULL_PORT=22
#   PULL_KEY=/root/.ssh/clirelay_backup_pull
#   PULL_KNOWN_HOSTS=/etc/clirelay-cluster/pull_known_hosts
#   BACKUP_DIR=/opt/clirelay-cluster/backups   KEEP=14
set -euo pipefail
# shellcheck source=deploy/cluster/backup/backup-lib.sh
. "$(dirname "$0")/backup-lib.sh"
: "${PULL_SOURCE:?set PULL_SOURCE in backup.env}"
PULL_PORT="${PULL_PORT:-22}"
PULL_KEY="${PULL_KEY:-/root/.ssh/clirelay_backup_pull}"
PULL_KNOWN_HOSTS="${PULL_KNOWN_HOSTS:-/etc/clirelay-cluster/pull_known_hosts}"

install -d -m 0700 "$BACKUP_DIR" "$BACKUP_DIR/wal"

# rrsync on the source maps "/" to its backup directory.
rsync -rt --chmod=D700,F600 --partial-dir=.rsync-partial --exclude='.tmp-*' --exclude='.drill' \
  -e "ssh -i $PULL_KEY -p $PULL_PORT -o BatchMode=yes -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=$PULL_KNOWN_HOSTS -o ConnectTimeout=20" \
  "$PULL_SOURCE:/" "$BACKUP_DIR/"
# pg_receivewal renames a segment from .gz.partial to .gz once it is full.
# Without --delete the old .partial copy would stay here forever, and
# pg_archivecleanup does not recognise that name, so drop it once the full
# segment has arrived.
for p in "$BACKUP_DIR"/wal/*.gz.partial; do
  if [ -e "$p" ] && [ -e "${p%.partial}" ]; then rm -f -- "$p"; fi
done
newest="$(ls -1t "$BACKUP_DIR"/base-*.tar.gz 2>/dev/null | head -n 1 || true)"
echo "pulled: newest base backup ${newest:+$(basename "$newest")}, $(find "$BACKUP_DIR/wal" -type f | wc -l | tr -d ' ') WAL files"

backup_prune
