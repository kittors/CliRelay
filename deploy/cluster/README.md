# deploy/cluster

Building blocks of a multi-instance CliRelay deployment. The full walkthrough,
the failure scenarios and the runbook are in
[docs/multi-instance-deployment.md](../../docs/multi-instance-deployment.md)
([中文](../../docs/multi-instance-deployment_CN.md)).

| Path | What it is |
|---|---|
| `tls/gen-cluster-certs.sh` | Private cluster CA and per-node certificates (mutual TLS for every cross-node link) |
| `etcd/` | One etcd member per machine; the arbiter holds the third vote |
| `postgres-ha/` | PostgreSQL 15 (Alpine) + Patroni image, `render-patroni.sh`, compose file |
| `stunnel/` | Mutual-TLS tunnel used only while migrating a live single-node database |
| `redis/` | Shared cluster Redis (TLS only) on the arbiter |
| `nginx/` | Local-first vhost with spill-over to the peer node |
| `dnswatch/` | systemd unit and example config for `cmd/clirelay-dnswatch` |
| `backup/` | Daily verified base backups and the continuous WAL archive (point-in-time recovery) |
| `bin/` | `cluster-status.sh`, `etcdctl.sh` |
| `lab/lab.sh` | Disposable rehearsal of the database migration and failover drills on one Docker host |
