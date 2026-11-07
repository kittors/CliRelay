# deploy/single-node

Host-side pieces of a single-node CliRelay deployment: one application host
running the native blue-green slots (`clirelay2-8318` / `clirelay2-8319`,
switched by `scripts/deploy-blue-green.sh`) and PostgreSQL in the
`clirelay-pg` container. hk-155 runs this layout since 2026-10-18.

These files are root-managed, like the deploy scripts: GitHub Actions never
uploads them. After a change is merged, root copies them to the host.

| File | Installed as | What it does |
|---|---|---|
| `clirelay-watchdog.sh` | `/usr/local/bin/clirelay-watchdog.sh` | SIGKILLs the active slot after two failed `/readyz` probes in a row, so `Restart=always` replaces a hung process. Watches only the slot `/opt/clirelay2/.active-port` names, skips a slot within 180 s of starting, and never starts a unit |
| `clirelay-watchdog.service` | `/etc/systemd/system/clirelay-watchdog.service` | Runs the watchdog; logs go to `journalctl -u clirelay-watchdog` |

Install or update the watchdog:

```bash
install -m 0755 clirelay-watchdog.sh /usr/local/bin/clirelay-watchdog.sh
install -m 0644 clirelay-watchdog.service /etc/systemd/system/clirelay-watchdog.service
systemctl daemon-reload && systemctl enable --now clirelay-watchdog && systemctl restart clirelay-watchdog
```

Why it is shaped this way: the first watchdog on hk-155 had port 8319
hard-coded. On 2026-10-27 the deploy moved traffic to 8318 and the drain
stopped 8319; the watchdog read the closed listener as a hang, SIGKILLed the
draining process (cutting its shutdown grace) and started it again, leaving
the retired build running next to the new one. `scripts/gha-node-verify.sh`
now looks at the old slot again 90 s after it stopped and fails the rollout
if anything brought it back.

## Backups

The database backups use the scripts in `../cluster/backup` with
`PG_CONTAINER=clirelay-pg` in `/etc/clirelay-cluster/backup.env`; the design
and the measurements are in
[docs/multi-instance-deployment.md](../../docs/multi-instance-deployment.md),
section 4.8 "Single node".

On the database host:

```bash
# /etc/clirelay-cluster/backup.env (root, 0600)
PG_CONTAINER=clirelay-pg
PG_SUPERUSER=cliproxy
BACKUP_DIR=/opt/clirelay-cluster/backups
KEEP=7
MAX_RATE=20M
```

```bash
docker exec clirelay-pg psql -U cliproxy -d postgres -c "ALTER SYSTEM SET max_slot_wal_keep_size = '4GB'" -c "SELECT pg_reload_conf()"
install -d /opt/clirelay-cluster/backup
install -m 0644 ../cluster/backup/backup-lib.sh /opt/clirelay-cluster/backup/
install -m 0755 ../cluster/backup/{pg-basebackup,pg-receivewal,pitr-drill}.sh /opt/clirelay-cluster/backup/
install -m 0644 ../cluster/backup/clirelay-pg-{receivewal.service,basebackup.service,basebackup.timer} /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now clirelay-pg-receivewal clirelay-pg-basebackup.timer
```

On the offsite host (another provider, another region), the pull key is
generated there and authorized on the database host as
`from="<offsite IP>",restrict,command="/usr/bin/rrsync -ro /opt/clirelay-cluster/backups" ssh-ed25519 ...`:

```bash
# /etc/clirelay-cluster/backup.env (root, 0600)
PULL_SOURCE=root@<database host>
PULL_PORT=<ssh port>
PULL_KEY=/root/.ssh/clirelay_backup_pull
PULL_KNOWN_HOSTS=/etc/clirelay-cluster/pull_known_hosts
BACKUP_DIR=/opt/clirelay-cluster/backups
KEEP=14
MAX_LAG_SECONDS=1800
DRILL_MEMORY=768m
```

```bash
install -d /opt/clirelay-cluster/backup /etc/systemd/system/clirelay-pitr-drill.service.d
install -m 0644 ../cluster/backup/backup-lib.sh /opt/clirelay-cluster/backup/
install -m 0755 ../cluster/backup/{offsite-pull,pitr-drill}.sh /opt/clirelay-cluster/backup/
install -m 0644 ../cluster/backup/clirelay-offsite-pull.{service,timer} ../cluster/backup/clirelay-pitr-drill.{service,timer} /etc/systemd/system/
install -m 0644 ../cluster/backup/clirelay-pitr-drill.offsite.conf /etc/systemd/system/clirelay-pitr-drill.service.d/offsite.conf
systemctl daemon-reload
systemctl enable --now clirelay-offsite-pull.timer clirelay-pitr-drill.timer
systemctl start clirelay-pitr-drill   # first drill: pull, restore, replay
```
