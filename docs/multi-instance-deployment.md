# Multi-instance deployment

CliRelay runs in one of two ways:

| Mode | For | Configuration |
|---|---|---|
| **Single instance** (default) | Personal use, small teams, NAS boxes: one machine is enough | `cluster.enabled: false` (or no `cluster` block) |
| **Multi-instance (cluster mode)** | Spreading traffic, automatic takeover when a machine dies, relieving an overloaded node | `cluster.enabled: true` on every node; all nodes share one PostgreSQL primary |

Single-instance behaviour is exactly what it was before clustering existed. This document covers multi-instance deployment. 中文版：[multi-instance-deployment_CN.md](multi-instance-deployment_CN.md)

## 1. What you get

- **Traffic spread across nodes.** Each hostname has one A record per node, so clients spread out, and so does outbound traffic.
- **Automatic takeover.**
  - When a node's CliRelay process is down, being deployed, or saturated, its nginx hands the same request to the peer node within the request.
  - When a whole machine dies, `clirelay-dnswatch` on the arbiter removes it from DNS after three failed probes (about 30 s) and adds it back once it recovers.
  - When a node can no longer reach its upstream proxies but its peer can, dnswatch removes it from DNS in about a minute (see 4.7).
  - When the database primary's machine dies, Patroni promotes the synchronous replica in about 20–40 s, and applications reconnect to it on their own.
- **No data loss.**
  - Replication is synchronous: a commit lands on both database nodes.
  - A primary cut off from the cluster cannot report a commit as successful and then lose it. Its commits wait for the replica and fail.
  - While the database switches over, applications spool usage records to local disk and replay them exactly once.
- **Single-node deployments keep working.** Every cluster feature sits behind `cluster.enabled`, which is off by default.

## 2. Architecture

```
                       clients
                          │  DNS: relay/code each resolve to every healthy node (DNS only, not proxied)
            ┌─────────────┴─────────────┐
            ▼                           ▼
     node A (n43)                  node B (n156)
     nginx :443                    nginx :443
      ├ local CliRelay first        ├ local CliRelay first
      └ down/full → peer :8445 ◀───▶ └ down/full → peer :8445      (mutual TLS)
     CliRelay (cluster mode)       CliRelay (cluster mode)
     PostgreSQL (Patroni) ◀── synchronous replication ──▶ PostgreSQL (Patroni)
     etcd member                   etcd member
            │                           │
            └────────────┬──────────────┘
                         ▼
               arbiter (hk-relay): serves no user traffic
                ├ etcd member (the third vote that prevents two primaries)
                ├ clirelay-dnswatch: probes every node every 10 s, edits DNS
                ├ shared cluster Redis (global rate limits, affinity, minted IDs)
                └ daily base backups + continuous WAL archive (restore to any moment in 7 days)
```

Every connection between machines uses **mutual TLS** with certificates from a private cluster CA:

- etcd;
- PostgreSQL replication and remote clients;
- the Patroni REST API;
- the shared Redis;
- the nginx spill-over port.

No VPN and no firewall rules are needed. That matters because some VPS providers drop all inbound UDP, which rules out WireGuard-style overlays.

### 2.1 Why a third machine

With only two machines, a broken link between them looks exactly like a dead peer. Promoting the replica on that evidence can leave two primaries accepting writes, and the data diverges.

The arbiter is the third vote. Only the side that holds an etcd majority (2 of 3) may run the primary, and an isolated primary demotes itself before its lease expires. The arbiter carries no user traffic. If it goes down, service continues; only automatic switchover pauses until it is back.

## 3. Failure scenarios (measured)

Numbers come from a rehearsal with the production settings: Patroni 4.1.5, PostgreSQL 15.19, `ttl` 30 s / `loop_wait` 10 s, synchronous replication.

| Scenario | What happens | User impact |
|---|---|---|
| CliRelay on a node crashes or is being deployed | nginx forwards to the peer node | None |
| A whole node goes down | dnswatch removes its records after about 30 s; clients move within the 60 s DNS TTL. A database failover follows if the primary was there | Requests in flight on that node fail; everything else is served |
| Planned primary switchover (`patronictl switchover`) | About 2 s without writes | Writes are spooled and replayed; none visible |
| Primary's machine dies | The synchronous replica is promoted after about 21 s | Writes are spooled and replayed |
| Primary partitioned away (still running) | It demotes itself before its lease expires, then the other node is promoted. Two primaries never coexist | Unconfirmed commits on the isolated side fail instead of reporting success |
| Failed node comes back | `pg_rewind` removes its divergent tail and it rejoins as the synchronous replica | No manual step |
| Synchronous replica stalls or hangs (host I/O stall, frozen VM) | Commits on the primary wait for it: as long as a short stall lasts, at most 15–19 s for a replica that never answers, after which the primary continues asynchronously | API requests do not wait on commits (usage is queued and spooled); management writes wait |
| Arbiter down | Service continues; etcd still has 2 of 3 votes; DNS stops being updated automatically | None |
| Shared Redis down | Nodes count locally and enforce `limit / active nodes` | Limits become approximate for a while |

## 4. Deploying a cluster

The example is the production layout: application nodes **n43** (43.255.122.4) and **n156** (156.225.27.154), and the arbiter **relay** (103.231.58.53). Everything referenced below lives in the repository under `deploy/cluster/`.

### 4.1 Certificates (private cluster CA)

```bash
deploy/cluster/tls/gen-cluster-certs.sh <secure-dir> \
  n43=43.255.122.4 n156=156.225.27.154 relay=103.231.58.53
```

- **Validity and usage.** The CA is valid for 10 years and node certificates for 5 years. Node certificates carry both `serverAuth` and `clientAuth`, and their SANs are the node name, `127.0.0.1` and the public IP.
- **Keep `ca.key` off the servers.** A machine gets only `ca.crt`, `node.crt` and `node.key`, in `/etc/clirelay-cluster/tls/`, owned by root, with the key at `0600`.
- **Per-service copies.** PostgreSQL (uid 70 in its container) and the shared Redis (uid 999) each get their own copy with matching ownership: `/etc/clirelay-cluster/tls-pg/` and `/etc/clirelay-cluster/tls-redis/`.
- **Adding a node.** Run the script again: it keeps the CA and existing certificates and issues only the new node's.

### 4.2 etcd (one member per machine)

```bash
# on every machine
install -d -m 0700 /opt/clirelay-cluster/etcd-data
cp deploy/cluster/etcd/docker-compose.yml /opt/clirelay-cluster/etcd/
cp deploy/cluster/etcd/etcd.env.example /opt/clirelay-cluster/etcd/.env   # set NODE_NAME / NODE_IP
docker compose -f /opt/clirelay-cluster/etcd/docker-compose.yml up -d
deploy/cluster/bin/etcdctl.sh endpoint health --cluster
```

- **Networking and auth.** Members use host networking. Both the client port (2379) and the peer port (2380) require mutual TLS.
- **Timeouts.** `heartbeat-interval` is 250 ms and `election-timeout` 2500 ms, to tolerate jitter between VPS hosts.
- **Footprint.** A member uses about 15–50 MB of memory.

### 4.3 PostgreSQL with Patroni

The image is `deploy/cluster/postgres-ha/Dockerfile`: `postgres:15.19-alpine3.24` plus Patroni 4.1.5.

> **Stay on the same image family as the single-node PostgreSQL (Alpine/musl).** Physical replication copies index pages byte for byte, and text indexes are ordered by the C library's collation. Mixing glibc and musl silently returns wrong results for indexed text lookups after a failover. Keep the minor version equal or newer, too.

Write `/etc/clirelay-cluster/node.env` (root, `0600`) on each node; its header documents every field. Then render the config and start:

```bash
render-patroni.sh /etc/clirelay-cluster/node.env > /etc/clirelay-cluster/patroni.yml   # owner 70:70, 0600
docker compose -f /opt/clirelay-cluster/postgres-ha/docker-compose.yml up -d
docker exec clirelay-patroni patronictl -c /etc/patroni/patroni.yml list
```

Key settings and why:

| Setting | Value | Why |
|---|---|---|
| `synchronous_mode` | true (once a replica exists) | Commits land on both nodes, so a failover loses nothing |
| `synchronous_mode_strict` | false | A dead replica drops the primary back to asynchronous replication instead of blocking it, once it is noticed (next row) |
| `wal_sender_timeout` | 15s | A replica that hangs without closing its connection holds commits until it is noticed. Freezing the standby in the rehearsal held them 32 s with the default 60 s (Patroni's member key expired first) and 15–19 s with 15 s. Shorter would cut replication during the few-second I/O stalls some hosts show, which hold commits for their own length either way |
| `failsafe_mode` | **false** | In the rehearsal, with failsafe on, an isolated primary first tried to reach the other members and demoted *after* the peer was promoted, so both were primary for a few seconds. With it off, the isolated side demotes before its lease expires |
| `use_pg_rewind` + `wal_log_hints` | on | A failed node rewinds its divergent tail and rejoins by itself |
| `remove_data_directory_on_*` | false | A data directory is never deleted automatically; a human decides after a diverged timeline |
| `max_slot_wal_keep_size` | 8GB | Caps the WAL a primary keeps for a replica that stays away, so it cannot fill the disk |
| pg_hba | Password on the same host; TLS + cluster certificate + password across hosts; everything else rejected | Port 55432 faces the internet; certificates are the access control |
| Ports | 55432 on `127.0.0.1` and the public IP | Same port as the single-node stack, so the local CliRelay DSN stays valid |

#### Migrating a live single-node database without downtime

This is how production moved. The old primary was never restarted.

1. **On the old primary (configuration reload only):**
   - enable SSL, with the certificate in the data directory's `tls/`;
   - create the `replicator` role and the replication slot `clirelay_standby`;
   - set `max_slot_wal_keep_size`;
   - allow the tunnelled replication connection in `pg_hba`, then `SELECT pg_reload_conf()`.
2. **Tunnel.** The old primary listens on `127.0.0.1` only, so a stunnel pair with mutual TLS carries replication to the new host (`deploy/cluster/stunnel/`). The server end accepts only the target node's certificate (`checkHost`).
3. **Start Patroni on the new host as a standby cluster.** Set `STANDBY_HOST=127.0.0.1` and `STANDBY_PORT=25431` in `node.env`. It takes a base backup through the tunnel and keeps streaming.
4. **Planned switchover** (about 2 s without writes):
   - set `default_transaction_read_only=on` on the old primary, reload, and terminate client sessions;
   - wait until the standby has replayed the old primary's final WAL position;
   - promote with `patronictl edit-config -s standby_cluster=null`.
   Applications use a multi-host DSN with `target_session_attrs=read-write` and find the new primary by themselves.
5. **Clean up:**
   - `ALTER SYSTEM RESET ALL` plus a reload on the new primary, to drop the `postgresql.auto.conf` copied from the old one;
   - stop the old PostgreSQL container and keep its data directory as a backup;
   - stop the tunnel;
   - start Patroni on the old host as a regular member, which clones from the new primary and joins;
   - turn on `synchronous_mode`.

`deploy/cluster/lab/lab.sh` rehearses the whole sequence on any Docker host, with a throwaway CA and no published ports.

### 4.4 Shared cluster Redis (arbiter)

`deploy/cluster/redis/docker-compose.yml`:

- TLS port 6380 only, with client certificates required, plus a password;
- 256 MB, `volatile-lru`, no persistence.

It holds only disposable data. When it is unreachable, nodes fall back to local limits.

### 4.5 CliRelay node settings

Keep `config.yaml` identical on all nodes, apart from node-local values such as `port` and `auth-dir`. Put the cluster settings in `/opt/clirelay2/.env`, which the slot units load:

```bash
CLIRELAY_CLUSTER_ENABLED=true
CLIRELAY_CLUSTER_NODE_ID=n43                     # unique per node
# multi-host DSN: local host first; target_session_attrs=read-write finds the primary
CLIRELAY_POSTGRES_DSN=postgres://cliproxy:<password>@127.0.0.1:55432,156.225.27.154:55432/cliproxy?target_session_attrs=read-write&sslmode=verify-ca&sslrootcert=/etc/clirelay-cluster/tls/ca.crt&sslcert=/etc/clirelay-cluster/tls/node.crt&sslkey=/etc/clirelay-cluster/tls/node.key&connect_timeout=5
# shared cluster Redis
CLIRELAY_CLUSTER_REDIS_ADDR=103.231.58.53:6380
CLIRELAY_CLUSTER_REDIS_PASSWORD=<password>
CLIRELAY_CLUSTER_REDIS_TLS_CA_FILE=/etc/clirelay-cluster/tls/ca.crt
CLIRELAY_CLUSTER_REDIS_TLS_CERT_FILE=/etc/clirelay-cluster/tls/node.crt
CLIRELAY_CLUSTER_REDIS_TLS_KEY_FILE=/etc/clirelay-cluster/tls/node.key
CLIRELAY_CLUSTER_REDIS_TLS_SERVER_NAME=relay
```

The top-level `redis` block stays a node-local instance and is independent of `cluster.redis`.

### 4.6 nginx: local first, spill over to the peer

Full example: `deploy/cluster/nginx/relay-cluster-node.conf.example`. The gist:

```nginx
upstream clirelay_app {
    zone clirelay_app 64k;
    server 127.0.0.1:8319 max_conns=400;   # the local active slot; the deploy script rewrites the port
    server 127.0.0.1:8446 backup;          # spill-over hop to the peer node
}
# in the user-facing server (443 → SNI router → 8444):
#   proxy_pass http://clirelay_app;
#   proxy_next_upstream error timeout;  proxy_next_upstream_tries 2;
server {                                    # spill-over hop: mutual TLS to the peer's 8445
    listen 127.0.0.1:8446;
    location / {
        proxy_pass https://<peer-ip>:8445;
        proxy_ssl_certificate     /etc/clirelay-cluster/tls/node.crt;
        proxy_ssl_certificate_key /etc/clirelay-cluster/tls/node.key;
        proxy_ssl_trusted_certificate /etc/clirelay-cluster/tls/ca.crt;
        proxy_ssl_verify on;  proxy_ssl_name <peer-node-name>;
    }
}
server {                                    # accepts the peer's spill-over: local slot only, no loops
    listen <own-public-ip>:8445 ssl;
    ssl_verify_client on;  ssl_client_certificate /etc/clirelay-cluster/tls/ca.crt;
    location / { proxy_pass http://127.0.0.1:8319; }
}
```

- When the local slot refuses the connection or already holds `max_conns` connections, nginx uses the backup, which is the peer.
- `proxy_next_upstream` retries only connection-level failures, where the request has not been sent yet, so it is safe for POST.
- On cutover the deploy script rewrites both slot references in the file and verifies each one.
- `location = /manage/version.txt` serves the panel's version file, so the panel deploy can verify each node.

### 4.7 DNS health checks (arbiter)

The watcher is `cmd/clirelay-dnswatch`, plus `deploy/cluster/dnswatch/`. The unit file's header lists the install steps, and `dnswatch.example.yaml` is the configuration template.

- **Probing.** Every 10 s the watcher requests `https://<node-ip>/readyz` with the hostname as SNI and a verified certificate. Three failures remove a node and three successes add it back, with at least 60 s between two changes of the same node.
- **Egress check (`probe.egress_path`).** Nodes do not share one route to the upstream proxies: they sit with different providers and transit, so one node can lose the proxy provider while its peer still reaches it. On 2026-09-25 n156 lost its route to the proxy provider's address ranges while `/readyz` stayed healthy, and for about 43 minutes every Codex request that landed on n156 failed until a human pulled it from DNS. DNS health therefore includes egress:
  - **On each node**, CliRelay opens a plain TCP connection every 15 s (3 s timeout) to each distinct proxy endpoint its upstream traffic may use: every enabled proxy-pool entry of every tenant, plus the global `proxy-url`. It sends nothing through it, performs no proxy handshake and uses no credentials. Two consecutive failed connects make an endpoint unreachable. `GET /readyz/egress` answers 204 while no endpoint is unreachable (also with no proxy configured, and before the first check after startup), otherwise 503 `{"status":"degraded","unreachable":N,"total":M}`. The body names no host; the node log names `host:port`. Like `/readyz`, the path bypasses the IP access list.
  - **On the arbiter**, `egress_path: /readyz/egress` adds that request to every round; anything but 2xx fails, including a timeout or a 404 from a release without the endpoint. A ready node failing it three rounds in a row is **degraded** until it passes three in a row. DNS then lists the healthy nodes (ready, egress passing) if there are any; otherwise the degraded ones, so a proxy outage that hits every node changes nothing; otherwise nothing changes (see the safety rules).
  - A node whose egress breaks leaves DNS after about a minute. `min_change_interval`, the hold file and dry-run apply as before. Degraded nodes show in the log, in the summary line (`healthy=… degraded=… unhealthy=…`), in `GET /status` (`state`, `egress`) and in the alerts `node_egress_degraded` / `node_egress_recovered`.
  - Upgrade every node before setting `egress_path`. An enabled pool entry that is dead from everywhere degrades every node and so disables the preference: disable such entries. An empty `egress_path` keeps the readiness-only behaviour.
- **Safety rules.**
  - If no node passes readiness, DNS is left untouched and an alert is raised. **It never deletes every record.**
  - Records are added before stale ones are removed.
  - Only A records pointing at the listed node IPs are touched.
- **Operating it.**
  - `touch /etc/clirelay-dnswatch/hold` before maintenance keeps it probing without editing DNS; remove the file afterwards.
  - Start with `dry_run: true`, read the logs, then turn dry-run off.
- **Token.** The Cloudflare token needs only DNS edit on the zone. It lives in a root-only file handed over through systemd `LoadCredential`.

### 4.8 Backups and point-in-time recovery

Replication protects against a lost machine, not against a bad `DELETE`: the mistake reaches the replica within milliseconds. The arbiter therefore keeps base backups plus a continuous WAL archive, which can restore any moment in the last 7 days. The files are in `deploy/cluster/backup/`.

- **Daily base backup** (`clirelay-pg-basebackup.timer`, 04:20):
  - taken from a replica when one is up (`target_session_attrs=prefer-standby`);
  - verified with `pg_verifybackup` before it is compressed, keeping the newest 7;
  - afterwards, WAL older than the oldest kept backup is pruned.
- **Continuous WAL archive** (`clirelay-pg-receivewal.service`): `pg_receivewal` follows the primary through the multi-host DSN on the permanent slot `clirelay_walarchive`. Patroni keeps that slot on every member, so a failover neither loses segments nor needs reconfiguration.
- **Restoring to a point in time:**
  - unpack the newest base backup older than the target into a new directory;
  - set `restore_command = 'gunzip -c /wal/%f.gz > %p'` and `recovery_target_time`, and create an empty `recovery.signal`;
  - start a standalone PostgreSQL from the same image and let it replay to the target. Check the data, then export what you need or use it as the seed of a new cluster;
  - the segment still being written is `*.partial`: to recover up to the latest moment, decompress it, drop the `.partial` suffix and put it into `pg_wal/`.

### 4.9 Rolling deploys

The `Deploy CliRelay` GitHub Actions workflow deploys one node at a time:

- each node is verified through `--resolve` for the whole drain window before the next one starts;
- the first failure stops the rollout, and later nodes are not touched;
- single-node repositories need no change.

Variables and secrets for several nodes are listed in the [node bootstrap guide](multi-instance-node-bootstrap_CN.md).

Releases must keep the old and new versions able to run side by side. Database migrations only expand (new tables, new columns); dropping anything waits for a later release.

## 5. Operations runbook

| Task | How |
|---|---|
| One-screen status | `deploy/cluster/bin/cluster-status.sh` |
| Application cluster members | `GET /v0/management/cluster`; the `X-CliRelay-Node` response header names the serving node |
| Database topology | `docker exec clirelay-patroni patronictl -c /etc/patroni/patroni.yml list` |
| Planned primary switchover | `patronictl ... switchover --leader <current> --candidate <target> --force` |
| Maintain a node | On the arbiter `touch /etc/clirelay-dnswatch/hold`; mark the node's local upstream `down` in nginx and reload; revert afterwards |
| Recover a failed node | Start its Patroni container; it rewinds and rejoins. `patronictl list` shows `Sync Standby` when done |
| Take a backup now | On the arbiter `systemctl start clirelay-pg-basebackup` |
| Check the WAL archive | On the arbiter `systemctl status clirelay-pg-receivewal`; `ls /opt/clirelay-cluster/backups/wal` |
| Restore from a backup | See 4.8 |
| Rotate certificates | Re-issue from the same CA, distribute, then restart etcd, Patroni, nginx and CliRelay one node at a time |

## 6. How the application stays correct across nodes

In cluster mode all nodes share one database. Every piece of state that used to live in a single process now has a cluster-consistent home. On a single node these mechanisms are either inactive or fall back to the original implementation.

### 6.1 Coordinator (`internal/cluster`)

- **Event bus.** Events travel over PostgreSQL `LISTEN/NOTIFY` on a dedicated connection that never comes from the pool.
  - Payloads carry IDs and versions, never data.
  - `PublishTx` delivers only when the writing transaction commits, so a rollback never notifies anyone.
  - Notifications are not durable. Every subscriber therefore receives a `Resync` whenever the listener (re)connects or a node joins, and reconciles by version.
- **Leader election.** Leadership is a session advisory lock, re-checked every 2 s against a writable primary.
  - Session-level TCP keepalives release it within about 30 s when the leader's machine vanishes, instead of Linux's roughly 2-hour default.
  - Leader-only work: account status probes, the OpenRouter price sync, shared-table log maintenance and retention, the usage rollup catch-up, session and audit cleanup, IP-rule cleanup, expiry of OAuth sessions and async jobs, and warmup scheduling.
- **Membership.** `cluster_nodes` receives a heartbeat every 5 s. `GET /v0/management/cluster` lists the nodes, the leader and the active count, and every response carries `X-CliRelay-Node`.
- **Migration lock.** Migrations, YAML imports and backfills run under a cluster-wide advisory lock, so two nodes upgrading together never race.

### 6.2 Credentials (OAuth accounts)

- **Storage.** The `auth_credentials` table in PostgreSQL is the source of truth. The local `auth-dir` is a mirror, so downloads, OAuth callbacks and model registration keep working unchanged.
- **Two kinds of writes:**
  - Credential writes (tokens and the like) are compare-and-swap on a version. A write based on an old version is refused, and the node converges to the stored value. **An old token never overwrites a new one.**
  - Runtime write-backs, which happen on every request (quota state, Claude OAuth health), are coalesced per account and written about once a second to a separate `runtime` column. They never touch tokens and never bump the version.
- **Refresh.** Only one node at a time holds an account's refresh lease. It first adopts a newer stored credential, then refreshes only if it still has to. Nodes without the lease skip the round; the refresher publishes the result.
- **First enablement.**
  - An empty table imports the local files with their existing `auth_index`, so request logs, quota snapshots and account bindings stay linked.
  - A joining node does not import its possibly stale files. It moves them to `.pre-cluster-backup-*` (nothing is deleted) and rebuilds the mirror from the database.
- **Uploads.** In cluster mode, upload credentials through the management panel. Files copied into `auth-dir` by hand are not imported.

### 6.3 Management configuration

- **Versioned writes.** Saves write only the keys that changed, with a compare-and-swap on the row version. Whole-collection replacements are serialised by a collection version. A save based on stale data gets `409 config_version_conflict`.
- **Broadcast and reload.** Every write is broadcast inside its transaction. Other nodes reload only the affected domain, within about 300 ms to 1 s.
  - Revoking or deleting an API key rebuilds only the auth map, never the executors, so Codex upstream WebSocket sessions survive.
  - Routing, proxy pool, pricing, model configs, IP rules and tenants each have their own reload path.
- **Toggles that were YAML-only now live in the database**, shared by all nodes: 14 of them, including `debug`, `request-retry` and `proxy-url`. Node-local settings stay in each node's YAML: port, TLS, `auth-dir`, `postgres`, `redis`, `cluster`, `trusted-proxies`, `auto-update`.

### 6.4 OAuth logins and asynchronous work

- **OAuth logins.**
  - Sessions live in `oauth_sessions`. A callback that reaches any node is stored on the session and wakes the initiating node, which exchanges the code with the PKCE verifier it keeps in memory.
  - If the initiating node dies mid-login, the session is reported failed within 60 s, with a prompt to log in again.
- **Asynchronous work.**
  - Video task → account routes live in `async_task_routes`, so status queries go back to the submitting account.
  - Progress snapshots of management image, video and model tests and account-status refreshes live in `management_jobs`, readable from any node.
- **Warmup policies** are persisted and run on the leader only.

### 6.5 Global limits, cooldown sharing and session affinity

These need the shared cluster Redis (`cluster.redis`):

- **Rate limits.** API-key and end-user RPM, TPM and concurrency, and upstream-account concurrency, are cluster-wide caps backed by atomic Redis scripts. Concurrency slots are leases, so a crashed process frees them when they expire.
- **Without Redis.** When the shared Redis is unavailable, nodes count locally and enforce `limit / active nodes`, so a Redis outage never multiplies a limit.
- **Cooldowns.** A node that cools down an account or model after a 429, an exhausted quota or an upstream failure announces it. Peers only ever extend their own cooldown, so the other node stops hitting that account too.
- **Session affinity.** A session is bound to the same account on every node, so the upstream prompt cache keeps hitting.
- **Synthetic session IDs** are minted once per cluster, with unchanged formats.
- **Security counters.** Login-throttle and IP auto-ban counters are shared.

### 6.6 Serving through database switchovers

- **Usage writes.** Connection-class errors are retried with backoff for about 20 s, then spooled to local disk and replayed in order once the database is back. Each record carries an idempotency key, so it lands exactly once, even when a commit's reply is lost or a switchover discards a locally committed transaction.
- **Quota admission and tenant checks.** While the database is down, the latest readings decide, if they are at most 120 s and 10 minutes old respectively.
- **Driver.** A connection that reports a read-only server (`25006`, an old primary after a switchover) or a broken session is never reused. New connections find the primary through the multi-host DSN, and a statement known not to have run is retried once on a fresh connection.
- **Measured.** The production switchover interrupted writes for 2.6 s, and no user request failed.

## 7. Limitations

- AI Studio WebSocket relay accounts (wsrelay) are usable only on the node they are connected to.
- Panel pages such as system monitoring and runtime logs show the node that served the page.
- DNS spreads clients, not requests: one very busy client can stay on one node. nginx spill-over covers that.
- While the arbiter is down there is no automatic failover, so monitor it separately (for example with Sonar).
