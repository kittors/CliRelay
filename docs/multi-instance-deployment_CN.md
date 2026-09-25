# 多实例部署

CliRelay 支持两种部署方式：

| 方式 | 适用场景 | 配置 |
|---|---|---|
| **单实例**（默认） | 个人、小团队、NAS，一台机器就够 | `cluster.enabled: false`（不写即为关闭） |
| **多实例（集群模式）** | 需要流量分摊、一台宕机另一台自动顶上、单机压力过大时分流 | 每个节点 `cluster.enabled: true`，所有节点共用一个 PostgreSQL 主库 |

单实例的行为与引入集群功能之前完全一致。本文只讲多实例。

## 1. 能做到什么

- **流量分摊**：两个域名各有多条 A 记录，客户端自然分散到各节点，出站流量也随之分摊。
- **一台挂了另一台顶上**：
  - 应用进程挂了、正在部署、或者并发打满时，nginx 在同一次请求里就转给对端节点，秒级切换。
  - 整台机器宕机时，仲裁机上的 `clirelay-dnswatch` 连续 3 次（约 30 秒）探测失败，就把它从 DNS 摘掉，恢复后自动加回。
  - 某节点连不上上游代理、而对端还连得上时，dnswatch 约 1 分钟内把它从 DNS 摘掉（见 4.7）。
  - 数据库主库所在机器宕机时，Patroni 在约 20–40 秒内把同步备库提升为主库，应用自动连到新主库。
- **数据不丢**：
  - 数据库使用同步复制，每次提交同时落到两台。
  - 被网络隔离的旧主库不会出现"提交成功但数据丢失"：它的提交会等不到备库确认，客户端收到的是报错而不是成功。
  - 应用在数据库切换期间把用量写入落到本地磁盘，恢复后补写，并做幂等去重。
- **仍然支持单实例**：所有集群功能都由 `cluster.enabled` 控制，默认关闭。

## 2. 架构

```
                     客户端
                       │  DNS：relay/code 各有两条 A 记录（灰云直连，不经 Cloudflare 代理）
          ┌────────────┴────────────┐
          ▼                         ▼
   节点 A（n43）                 节点 B（n156）
   nginx :443                    nginx :443
    ├ 本机 CliRelay（优先）        ├ 本机 CliRelay（优先）
    └ 满载/故障 → 对端 :8445 ◀────▶ └ 满载/故障 → 对端 :8445     （双向 TLS）
   CliRelay（集群模式）          CliRelay（集群模式）
   PostgreSQL（Patroni 成员）◀── 同步复制 ──▶ PostgreSQL（Patroni 成员）
   etcd 成员                     etcd 成员
          │                         │
          └──────────┬──────────────┘
                     ▼
              仲裁机（hk-relay）：不接业务流量
               ├ etcd 成员（第三票，防止出现两个主库）
               ├ clirelay-dnswatch：每 10 秒探测各节点，自动增删 DNS 记录
               ├ 集群共享 Redis（全局限流计数、会话粘性、合成 ID）
               └ 每日全量备份 + 连续 WAL 归档（可恢复到 7 天内任意时刻）
```

所有跨机器的连接（etcd、PostgreSQL 复制与远程连接、Patroni REST、共享 Redis、nginx 溢出端口）都使用集群私有 CA 签发的证书做**双向 TLS**。所以不需要 VPN，也不需要改防火墙规则。有些 VPS 服务商会丢弃全部入站 UDP，WireGuard 这类方案在这种机器上用不了。

### 2.1 为什么需要第三台机器

只有两台时，两台之间的网络断了而两台都还活着，任何一方都无法判断是对方挂了还是链路断了。自动提升备库就可能出现两个主库，两边各写各的，数据分叉。

仲裁机提供第三票：哪一边能拿到多数票（etcd 的 2/3），哪一边的数据库才能当主库；被隔离的一边会在租约到期前自己降级。仲裁机本身不接业务流量，它宕机时业务照常，只是这段时间不会自动切换。

## 3. 故障场景与实测表现

下列数字来自与生产相同配置的演练：Patroni 4.1.5，PostgreSQL 15.19，ttl 30s / loop_wait 10s，同步复制。

| 场景 | 表现 | 用户影响 |
|---|---|---|
| 某节点 CliRelay 进程挂了或正在发版 | nginx 把请求转给对端节点 | 无感 |
| 某节点整机宕机 | dnswatch 约 30 秒后摘掉它的 DNS 记录，客户端在 DNS TTL（60 秒）内陆续切走；如果主库在这台，同时触发数据库切换 | 该节点上的在途请求失败；其余请求正常 |
| 主库计划内切换（`patronictl switchover`） | 写入中断约 2 秒 | 应用写入先缓冲、再补写，用户无感 |
| 主库所在机器宕机 | 约 21 秒后同步备库升为主库 | 期间的写入先缓冲、再补写 |
| 主库被网络隔离（进程还活着） | 先在租约到期前自行降级，另一台随后升主，不会同时存在两个主库 | 隔离侧未确认的提交返回错误，不会误报成功 |
| 故障节点恢复 | 自动用 pg_rewind 回滚分叉部分，以同步备库身份重新加入 | 无需人工 |
| 同步备库卡顿或卡死（宿主机 I/O 卡顿、虚拟机冻结） | 主库的提交会等它：短暂卡顿卡多久就等多久；备库一直不响应时最多等 15–19 秒，之后主库退回异步复制继续写 | API 请求不等提交（用量先入队、落本地 spool），管理后台的写操作会等 |
| 仲裁机宕机 | 业务照常；etcd 仍有 2/3 票；DNS 不再自动切换 | 无 |
| 共享 Redis 不可用 | 各节点退回本机计数，上限按"总上限 ÷ 在线节点数"执行 | 限额在短时间内变得较粗略 |

## 4. 部署多实例

下面以生产环境为例：两台应用节点 n43（43.255.122.4）、n156（156.225.27.154），仲裁机 relay（103.231.58.53）。各组件的部署文件都在仓库的 `deploy/cluster/` 下。

### 4.1 证书（集群私有 CA）

```bash
deploy/cluster/tls/gen-cluster-certs.sh <安全目录> \
  n43=43.255.122.4 n156=156.225.27.154 relay=103.231.58.53
```

- **有效期**：CA 10 年，节点证书 5 年。节点证书同时带 serverAuth 和 clientAuth 用途，SAN 包含节点名、127.0.0.1 和公网 IP。
- **`ca.key` 不要放到任何服务器上**。每台机器只放 `ca.crt`、`node.crt`、`node.key`，统一放在 `/etc/clirelay-cluster/tls/`，属主 root，`node.key` 权限 0600。
- **各组件自己的副本**：PostgreSQL（容器内 uid 70）和共享 Redis（uid 999）各需要一份属主匹配的副本，分别放在 `/etc/clirelay-cluster/tls-pg/`、`/etc/clirelay-cluster/tls-redis/`。
- **以后加节点**：再次运行脚本即可。已有的 CA 和证书会保留，只签发新节点的证书。

### 4.2 etcd（三台各一个成员）

```bash
# 每台机器
install -d -m 0700 /opt/clirelay-cluster/etcd-data
cp deploy/cluster/etcd/docker-compose.yml /opt/clirelay-cluster/etcd/
cp deploy/cluster/etcd/etcd.env.example /opt/clirelay-cluster/etcd/.env   # 改 NODE_NAME / NODE_IP
docker compose -f /opt/clirelay-cluster/etcd/docker-compose.yml up -d
deploy/cluster/bin/etcdctl.sh endpoint health --cluster
```

- **网络与认证**：使用主机网络；2379（客户端）和 2380（成员间）都要求双向 TLS。
- **超时参数**：`heartbeat-interval 250ms`、`election-timeout 2500ms`，用来容忍 VPS 之间的网络抖动。
- **资源**：每个成员内存约 15–50MB。

### 4.3 PostgreSQL + Patroni

镜像由 `deploy/cluster/postgres-ha/Dockerfile` 构建，基于 `postgres:15.19-alpine3.24`，加装 Patroni 4.1.5。

> **必须与原来的单实例 PostgreSQL 使用同一系列镜像**（alpine / musl）。物理复制是逐字节复制索引页，文本索引的顺序由 C 库的排序规则决定。glibc 与 musl 混用时，切换后索引查询会悄悄返回错误结果。小版本也要一致或更新。

每个节点写一份 `/etc/clirelay-cluster/node.env`（root，0600），参见 `render-patroni.sh` 开头的说明，然后生成配置并启动：

```bash
render-patroni.sh /etc/clirelay-cluster/node.env > /etc/clirelay-cluster/patroni.yml   # 属主 70:70，0600
docker compose -f /opt/clirelay-cluster/postgres-ha/docker-compose.yml up -d
docker exec clirelay-patroni patronictl -c /etc/patroni/patroni.yml list
```

关键设定及原因：

| 设定 | 值 | 原因 |
|---|---|---|
| `synchronous_mode` | true（有备库后再打开） | 提交同时落到两台，切换不丢数据 |
| `synchronous_mode_strict` | false | 备库挂了、并且被发现后，自动退回异步复制，主库不会一直卡住（发现时间见下一行） |
| `wal_sender_timeout` | 15s | 备库卡死但连接没断时，主库的提交会一直等到发现它为止。演练中冻结备库：默认 60 秒时提交卡了 32 秒（先等到 Patroni 成员键过期），设为 15 秒后卡 15–19 秒。再短的话，某些主机常见的几秒 I/O 卡顿就会把复制连接切断；而这类卡顿无论如何都会让提交等上同样长的时间 |
| `failsafe_mode` | **false** | 演练中打开它时，被隔离的主库会先去联系各成员再降级，结果晚于对方升主，两边短暂同时为主。关掉后，隔离侧在租约到期前降级 |
| `use_pg_rewind` + `wal_log_hints` | on | 故障节点恢复后自动回滚分叉部分并重新加入 |
| `remove_data_directory_on_*` | false | 绝不自动删除数据目录，出现分叉时由人来决定 |
| `max_slot_wal_keep_size` | 8GB | 备库长时间离线时，限制主库为它保留的 WAL，防止撑爆磁盘 |
| pg_hba | 本机密码认证；跨机器必须 TLS 加集群证书加密码；其他来源一律拒绝 | 5432 暴露在公网，靠证书做访问控制 |
| 监听端口 | 127.0.0.1 与公网 IP 的 55432 | 与单实例的端口一致，本机 CliRelay 的 DSN 不用改 |

#### 从单实例无停机迁移到 Patroni

生产就是这样迁过来的，全程没有重启原来的主库：

1. **在旧主库上执行（只需重载配置）**：
   - 开启 SSL，证书放在数据目录的 `tls/` 下；
   - 建 `replicator` 复制账号和复制槽 `clirelay_standby`；
   - 设置 `max_slot_wal_keep_size`；
   - 在 pg_hba 里放行经隧道进来的复制连接，然后 `SELECT pg_reload_conf()`。
2. **建隧道**：旧主库只监听 127.0.0.1，所以用 stunnel 双向 TLS 隧道把复制连接带到新机器，配置见 `deploy/cluster/stunnel/`。服务端只接受目标节点的证书（`checkHost`）。
3. **在新机器上以"备用集群"启动 Patroni**：node.env 里设置 `STANDBY_HOST=127.0.0.1`、`STANDBY_PORT=25431`，它会经隧道全量拷贝，然后持续流式复制。
4. **计划内切换**，写入中断约 2 秒：
   - 旧主库设 `default_transaction_read_only=on`，重载配置，断开客户端连接；
   - 等备库回放到旧主库的最终 WAL 位置；
   - 执行 `patronictl edit-config -s standby_cluster=null` 提升新主库；
   - 应用的 DSN 是多主机加 `target_session_attrs=read-write`，会自动连到新主库。
5. **收尾**：
   - 在新主库上执行 `ALTER SYSTEM RESET ALL` 并重载配置，清掉从旧库拷贝过来的 `postgresql.auto.conf`；
   - 停掉旧 PostgreSQL 容器，数据目录保留作备份；
   - 关闭隧道；
   - 在旧机器上以普通成员身份启动 Patroni，它会从新主库全量拷贝并加入集群；
   - 打开 `synchronous_mode`。

整套流程可以用 `deploy/cluster/lab/lab.sh` 在任意一台 Docker 主机上完整演练（使用一次性 CA，不对外暴露任何端口）。

### 4.4 集群共享 Redis（仲裁机）

`deploy/cluster/redis/docker-compose.yml`：

- 只开 TLS 端口 6380，要求客户端证书，另设密码；
- 最多使用 256MB，`volatile-lru`，不做持久化。

这里只放可以丢的临时数据，它不可用时各节点自动退回本机限流。

### 4.5 CliRelay 节点配置

`config.yaml` 在各节点保持一致（`port`、`auth-dir` 等本机设置除外）。集群相关设置建议放在 slot 单元读取的 `/opt/clirelay2/.env`：

```bash
CLIRELAY_CLUSTER_ENABLED=true
CLIRELAY_CLUSTER_NODE_ID=n43                     # 各节点唯一
# 多主机 DSN：本机优先，target_session_attrs=read-write 自动找到当前主库
CLIRELAY_POSTGRES_DSN=postgres://cliproxy:<密码>@127.0.0.1:55432,156.225.27.154:55432/cliproxy?target_session_attrs=read-write&sslmode=verify-ca&sslrootcert=/etc/clirelay-cluster/tls/ca.crt&sslcert=/etc/clirelay-cluster/tls/node.crt&sslkey=/etc/clirelay-cluster/tls/node.key&connect_timeout=5
# 集群共享 Redis
CLIRELAY_CLUSTER_REDIS_ADDR=103.231.58.53:6380
CLIRELAY_CLUSTER_REDIS_PASSWORD=<密码>
CLIRELAY_CLUSTER_REDIS_TLS_CA_FILE=/etc/clirelay-cluster/tls/ca.crt
CLIRELAY_CLUSTER_REDIS_TLS_CERT_FILE=/etc/clirelay-cluster/tls/node.crt
CLIRELAY_CLUSTER_REDIS_TLS_KEY_FILE=/etc/clirelay-cluster/tls/node.key
CLIRELAY_CLUSTER_REDIS_TLS_SERVER_NAME=relay
```

- 顶层的 `redis` 仍然是节点本地的实例，与 `cluster.redis` 互不影响。
- `trusted-proxies` 需要包含对端节点的公网 IP。nginx 溢出过来的请求来自对端，带着第一跳写好的 `X-Forwarded-For`。

### 4.6 nginx：本机优先，满载或故障时溢出到对端

```nginx
upstream clirelay_app {
    zone clirelay_app 64k;
    server 127.0.0.1:8319 max_conns=400;   # 本机 active slot，由部署脚本改写端口
    server 127.0.0.1:8446 backup;          # 溢出口：转给对端节点
}
# 用户入口（443 → SNI 分流 → 8444）里：
#   proxy_pass http://clirelay_app;
#   proxy_next_upstream error timeout;  proxy_next_upstream_tries 2;
server {                                    # 溢出口：以双向 TLS 转发给对端的 8445
    listen 127.0.0.1:8446;
    location / {
        proxy_pass https://<对端IP>:8445;
        proxy_ssl_certificate     /etc/clirelay-cluster/tls/node.crt;
        proxy_ssl_certificate_key /etc/clirelay-cluster/tls/node.key;
        proxy_ssl_trusted_certificate /etc/clirelay-cluster/tls/ca.crt;
        proxy_ssl_verify on;  proxy_ssl_name <对端节点名>;
    }
}
server {                                    # 接收对端溢出：只转本机 slot，不再溢出，避免环路
    listen <本机公网IP>:8445 ssl;
    ssl_verify_client on;  ssl_client_certificate /etc/clirelay-cluster/tls/ca.crt;
    location / { proxy_pass http://127.0.0.1:8319; }
}
```

- 本机 slot 连不上，或者正在用的连接数达到 `max_conns`，nginx 就改用 backup，也就是转给对端。
- `proxy_next_upstream` 只对"连接失败或超时"重试，这时请求还没有发出去，POST 也可以安全地换节点。
- 部署脚本切流时，会把同一文件里两处指向 active slot 的端口一起改写，并逐处校验。

### 4.7 DNS 健康检查（仲裁机）

`cmd/clirelay-dnswatch` 加上 `deploy/cluster/dnswatch/`。部署步骤见 `clirelay-dnswatch.service` 文件头；配置示例见 `dnswatch.example.yaml`。

- 每 10 秒以 `https://<节点IP>/readyz`（SNI 为域名，正常校验证书）探测一次。连续失败 3 次摘除；恢复后连续成功 3 次加回；两次切换之间至少间隔 60 秒。
- **出口检查（`probe.egress_path`）**：各节点到上游代理的线路并不相同（机房、运营商、中转都不一样），一个节点可能连不上代理商，而对端照常能连。2026-09-25 n156 到代理商 IP 段的路由断了，`/readyz` 却一直正常，落到 n156 的 Codex 请求连续失败约 43 分钟，直到人工把它从 DNS 摘掉。所以 DNS 健康判断要把出口算进去：
  - **节点侧**：CliRelay 每 15 秒对上游流量可能经过的每个代理端点（所有租户已启用的代理池条目，加上全局 `proxy-url`，按 host:port 去重）发起一次纯 TCP 连接（超时 3 秒），连上即关闭：不通过代理发送任何数据，不做代理握手，不用任何凭据。连续 2 次连不上才算不可达。`GET /readyz/egress` 在没有不可达端点时返回 204（没配代理、或启动后首轮检查还没跑完时也是 204），否则返回 503 `{"status":"degraded","unreachable":N,"total":M}`。响应体不含任何主机信息，节点日志里只记 `host:port`。和 `/readyz` 一样不受 IP 访问名单限制。
  - **仲裁机侧**：配置 `egress_path: /readyz/egress` 后，每轮额外请求这个路径；非 2xx 一律算失败，包括超时和旧版本返回的 404。就绪的节点连续 3 轮失败即为**降级**，连续 3 轮成功才恢复。DNS 按三档取舍：有健康节点（就绪且出口正常）就只保留健康节点；没有就保留降级节点，所以代理商整体故障、所有节点一起降级时 DNS 不动；连降级节点都没有就什么都不改（见下一条）。
  - 出口断掉的节点约 1 分钟后被摘掉。`min_change_interval`、hold 文件、dry-run 照常生效。降级节点会出现在日志、汇总行（`healthy=… degraded=… unhealthy=…`）、`GET /status`（`state`、`egress`）和告警 `node_egress_degraded` / `node_egress_recovered` 里。
  - 先把所有节点升级到带 `/readyz/egress` 的版本再开启。某个已启用的代理池条目如果在哪儿都连不上，会让所有节点一起降级，出口优先也就等于关掉了，这种条目要停用。`egress_path` 留空就是原来只看就绪的行为。
- 所有节点都不就绪时，DNS 保持不动并告警，**绝不会删光记录**。
- 先加新记录再删旧记录；只动名单内节点 IP 的 A 记录。
- 维护前 `touch /etc/clirelay-dnswatch/hold`，之后只探测、不改 DNS；维护完删除这个文件。
- 首次部署先用 `dry_run: true` 观察日志，确认无误后再关掉 dry-run。
- Cloudflare 令牌只需要该 zone 的 DNS 编辑权限，放在 root 0600 的文件里，由 systemd `LoadCredential` 交给服务。

### 4.8 备份与任意时间点恢复

复制能防机器宕机，防不了误删：一条错误的 `DELETE` 几毫秒内就同步到了备库。所以在仲裁机上另外保存"全量备份 + 连续 WAL"，可以恢复到最近 7 天内的任意时刻。部署文件在 `deploy/cluster/backup/`。

- **每日全量备份**（`clirelay-pg-basebackup.timer`，每天 04:20）：
  - 优先从备库拉取（`target_session_attrs=prefer-standby`），不给主库增加负担；
  - 用 `pg_verifybackup` 校验通过后才压缩保存，保留最近 7 份；
  - 完成后清理比最老那份备份还早的 WAL。
- **连续 WAL 归档**（`clirelay-pg-receivewal.service`）：
  - `pg_receivewal` 经多主机 DSN 连当前主库，持续把 WAL 写到 `/opt/clirelay-cluster/backups/wal`；
  - 使用 Patroni 的永久复制槽 `clirelay_walarchive`。Patroni 在每个成员上都维护这个槽，主库切换后归档自动接上，不丢段。
- **恢复到某个时间点**：
  - 把最近一份早于目标时间的全量备份解压到新目录；
  - 设置 `restore_command = 'gunzip -c /wal/%f.gz > %p'` 和 `recovery_target_time`，建空文件 `recovery.signal`；
  - 用同一镜像启动单实例 PostgreSQL 回放到目标时间，核对数据后，再决定导出需要的数据，或者以它作为新集群的起点；
  - 正在写入的最后一段是 `*.partial`。要恢复到最新时刻，先把它解压、去掉 `.partial` 后缀，再放进 `pg_wal/`。

### 4.9 逐台滚动发版

GitHub Actions 的 `Deploy CliRelay` 按节点逐台部署：

- 先完成一台，并在该节点上用 `--resolve` 绑定验证，覆盖整个排空窗口；
- 这台成功后才部署下一台，任何一台失败就停止，后面的节点不动；
- 单节点仓库不用改任何配置。

多节点需要设置的变量与 secrets 见 [节点初始化说明](multi-instance-node-bootstrap_CN.md)。

发版必须保持新旧版本能同时在线：数据库迁移只做"先扩展、后收缩"（加表、加列），删字段放到下一个版本。

## 5. 运维手册

| 要做的事 | 命令 |
|---|---|
| 一屏查看集群状态 | `deploy/cluster/bin/cluster-status.sh` |
| 查看应用集群成员 | `GET /v0/management/cluster`；响应头 `X-CliRelay-Node` 标明请求落在哪台 |
| 查看数据库拓扑 | `docker exec clirelay-patroni patronictl -c /etc/patroni/patroni.yml list` |
| 计划内切换主库 | `patronictl ... switchover --leader <当前> --candidate <目标> --force` |
| 维护某个节点 | 仲裁机上 `touch /etc/clirelay-dnswatch/hold`；nginx 把本机 upstream 标记为 `down` 后重载；维护完再恢复 |
| 故障节点恢复 | 启动它的 Patroni 容器即可，自动 rewind 后重新加入；`patronictl list` 看到 `Sync Standby` 即恢复完成 |
| 立刻做一份备份 | 仲裁机上 `systemctl start clirelay-pg-basebackup` |
| 查看 WAL 归档 | 仲裁机上 `systemctl status clirelay-pg-receivewal`；`ls /opt/clirelay-cluster/backups/wal` |
| 从备份恢复 | 解压到新的数据目录，用同一镜像启动单实例 PostgreSQL 核对数据，确认后再作为新集群的起点 |
| 证书轮换 | 用同一 CA 重新签发，分发后依次重启 etcd、Patroni、nginx、CliRelay |

## 6. 应用层如何保证多实例正确

集群模式下，所有节点共用同一个数据库。下面这些原本只存在于单个进程里的状态，都换成了跨节点一致的做法。单实例模式下，这些机制要么不启用，要么退回原来的实现。

### 6.1 协调器（`internal/cluster`）

- **事件总线**：用 PostgreSQL 的 `LISTEN/NOTIFY` 实现，专用一条连接，不从连接池取。
  - 事件里只放 ID 和版本号，不放数据。
  - 写数据的事务里用 `PublishTx` 发事件，只有事务提交了才会投递，回滚不会误通知。
  - NOTIFY 不持久。所以监听连接每次（重新）连上、以及有新节点加入时，各订阅者都会收到一次 `Resync`，按版本号对账补上可能漏掉的变化。
- **选主**：用 advisory lock，持锁会话每 2 秒确认一次对方仍是可写主库。
  - 主节点所在机器整台消失时，锁约 30 秒内释放，靠的是会话级 TCP keepalive。不设的话，Linux 默认要约 2 小时。
  - 以下维护任务只在主节点运行：账号状态探测、OpenRouter 价格同步、共享表的日志维护与保留、用量汇总追平、会话与审计清理、IP 规则清理、OAuth 会话与异步任务过期清理、warmup 策略调度。
- **成员表** `cluster_nodes`：每 5 秒心跳一次。`GET /v0/management/cluster` 可以查看所有节点、主节点和在线数；每个响应都带 `X-CliRelay-Node`，标明请求落在哪台。
- **迁移锁**：数据库迁移、YAML 导入和各类回填都在集群级 advisory lock 下执行，两台同时升级也不会互相踩踏。

### 6.2 凭据（OAuth 账号）

- **存储**：集群模式下，凭据以 PostgreSQL 的 `auth_credentials` 表为准，本地 `auth-dir` 只是镜像，所以下载、OAuth 回调、模型注册都照旧可用。
- **两类写入**：
  - 凭据本身（token 等）的写入按版本号做比较后写入（CAS）。基于旧版本的写入会被拒绝，本节点随即对齐到库里的最新值。**旧 token 永远不会覆盖新 token。**
  - 每次请求都会产生的运行态回写（配额状态、Claude OAuth 健康）按账号合并，约每秒写一次单独的 `runtime` 列，不碰 token，不升版本。
- **刷新**：同一时刻只有一个节点能拿到某个账号的刷新租约。拿到后先采用库里可能更新的凭据，再判断是否还需要刷新；拿不到就跳过，由正在刷新的节点广播结果。
- **首次启用**：表为空时导入本地文件，`auth_index` 保持原值，请求日志、配额快照、账号绑定都不会断链。新加入的节点不导入本地的陈旧文件，而是移到 `.pre-cluster-backup-*`（不删除），再用库里的数据重写镜像。
- 集群模式下，请通过管理面板上传凭据。手工放进 `auth-dir` 的文件不会被导入。

### 6.3 管理配置

- **只写改动项**：保存时只写真正改动的 key，并按行版本做比较后写入；整表替换按集合版本号串行化。基于旧数据的保存返回 409 `config_version_conflict`（"数据已被其他节点修改，请刷新"）。
- **广播与按需重载**：每次写入在事务内广播，其他节点按类别只重载受影响的部分，约 300ms–1s 内生效。
  - 吊销、删除 API Key 只重建鉴权表，不重建执行器，所以不会打断 Codex 上游 WebSocket 会话。
  - 路由、代理池、价格、模型配置、IP 规则、租户各有自己的重载路径。
- **原来只存在本机 YAML 的开关迁入数据库**：debug、request-retry、proxy-url 等 14 项，所有节点共用一份。节点本地的设置（端口、TLS、`auth-dir`、`postgres`、`redis`、`cluster`、`trusted-proxies`、`auto-update` 等）仍在各自的 YAML 里。

### 6.4 OAuth 登录与异步任务

- **OAuth 登录**：会话存在 `oauth_sessions` 表。回调落到任意节点都会写进会话并唤醒发起节点，由它用自己内存里的 PKCE verifier 换取 token。发起节点中途宕机时，会话 60 秒内被标为失败，提示重新登录。
- **异步任务**：视频任务的"任务 → 账号"对应关系存在 `async_task_routes`，查询固定回原账号。管理端的图片、视频、模型测试和账号状态刷新任务的进度快照存在 `management_jobs`，任意节点都能查询。
- **warmup 策略**：持久化到数据库，只在主节点执行。

### 6.5 全局限流、冷却同步与会话粘性

依赖集群共享 Redis（`cluster.redis`）：

- **全局限流**：API Key 和终端用户的 RPM、TPM、并发，以及上游账号并发，都用 Redis 上的原子脚本实现，是全局上限。并发名额带租约，进程崩溃后到期自动回收。
- **共享 Redis 不可用时**：自动退回本机计数，上限按"总上限 ÷ 在线节点数"执行，不会因为 Redis 故障而放大限额。
- **冷却同步**：某节点遇到 429、额度耗尽或上游故障而冷却某个账号或模型时，会广播给其他节点。接收方只延长、不缩短冷却，所以另一台不会继续打这个账号。
- **会话粘性**：同一会话在两台节点上绑定到同一个账号，上游的 prompt cache 仍能命中。
- **合成会话 ID**：由全集群只铸造一次，格式与原来一致。
- **登录安全计数**：登录限流和 IP 自动封禁的计数在节点之间共享。

### 6.6 数据库切换期间业务不中断

- **用量写入**：连接类错误会退避重试约 20 秒，然后写入本地磁盘缓冲，数据库恢复后按顺序回放。每条记录带幂等键，所以在"提交成功但回包丢失"、切换丢弃本地已提交事务这类情况下，也保证只落库一次。
- **额度准入与租户校验**：数据库不可用时，分别用 120 秒内和 10 分钟内的最近读数兜底，请求照常放行或拒绝。
- **驱动层**：遇到只读错误（25006，切换后的旧主库）或连接断开的连接，一律不再复用。新连接经多主机 DSN 找到新主库。确定没有执行过的语句会在新连接上自动重试一次。
- **实测**：生产计划内切换时写入中断 2.6 秒，期间没有任何用户请求失败。

## 7. 限制

- AI Studio 的 WebSocket 中转账号（wsrelay）只在它接入的那一台可用。
- 系统监控、运行日志这类面板页面只显示处理该请求的那一台的数据。
- DNS 分流是按客户端的：个别用量很大的客户端可能一直落在同一台上，由 nginx 溢出兜底。
- 仲裁机宕机期间不会自动切换，需要另外监控它（Sonar 等）。
