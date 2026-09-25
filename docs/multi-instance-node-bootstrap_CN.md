# 多节点部署：节点初始化说明

本文说明一台服务器要满足哪些条件，才能加入 `Deploy CliRelay` 与 `Deploy Frontend` 两个工作流的逐台滚动发布，也说明现有节点升级到本版部署脚本（`SCRIPT_VERSION=2026.09.25.2`）时要做的一次性操作。

文中的 IP 都是文档保留地址（`203.0.113.x`、`198.51.100.x`），实际操作时换成节点真实地址。

## 1. 部署链路

```
GitHub Actions ──ssh(deploy 用户)──> 节点
  build     构建一次二进制，作为 artifact 供所有节点使用
  preflight 并行检查所有节点，全部通过才进入下一步，此时不改动任何节点
  deploy    按 CLIRELAY_DEPLOY_NODES 的顺序逐台发布（max-parallel: 1，fail-fast）
            1. 把二进制写入 /opt/clirelay2/incoming/cli-proxy-api-new
            2. sudo -n /usr/local/sbin/clirelay-gha-deploy      (root 入口)
               └─ /opt/clirelay2/scripts/deploy-blue-green.sh     (root 管理，GHA 从不覆盖)
            3. 用 curl --resolve 把探测绑定到该节点，持续 drain+30 秒（至少 210 秒），
               再确认旧 slot 已真正退出，新 slot 返回本次构建版本，然后才进入下一个节点
  docker    不绑定节点、经 DNS 做一次公网探测，然后触发镜像构建
```

任一节点失败，后续节点保持旧版本。已发布的节点不会自动回退。数据库迁移只做扩展，新旧版本可以同时在线。

## 2. 目录与属主

root 会执行或信任的文件，连同它上面的每一级目录，都必须归 root 所有，且组和其他用户不可写。入口脚本每次运行都会检查这一点，不满足就拒绝部署。

| 路径 | 属主:属组 | 权限 | 说明 |
|---|---|---|---|
| `/opt/clirelay2` | root:root | 0755 | 不能让 deploy 或 slot 用户可写，否则可以替换 `scripts/` |
| `/opt/clirelay2/scripts/` | root:root | 0755 | |
| `/opt/clirelay2/scripts/deploy-blue-green.sh`<br>`cleanup-drained-slot.sh`<br>`reconcile-active-slot.sh` | root:root | 0755 | 手工同步，见第 7 节 |
| `/opt/clirelay2/incoming/` | deploy:deploy | 0755 | GHA 只往这里写二进制 |
| `/usr/local/sbin/clirelay-gha-deploy` | root:root | 0755 | 来自仓库 `scripts/clirelay-gha-deploy` |
| `/etc/clirelay2/` | root:root | 0755 | |
| `/etc/clirelay2/deploy.env` | root:root | 0644 | 节点设置，见第 4 节 |
| `/home/web/html/` | deploy 可写 | 0755 | 前端发布目录 |
| `/home/web/html/relay-panel-releases/` | deploy:deploy | 0755 | 前端各版本；`relay-panel` 是指向其中一个版本的软链 |

slot 进程（第 5 节的运行用户）需要写的只有：`config.yaml`（管理面板会回写；目录不可写时会原地写入）、`logs/` 等运行时目录，以及配置里 auth 目录指向的位置。只对这些文件和目录 `chown`，不要改 `/opt/clirelay2` 本身的属主。

```bash
# root 执行（新节点）
id deploy >/dev/null 2>&1 || useradd -m -s /bin/bash deploy
install -d -m 0700 -o deploy -g deploy /home/deploy/.ssh
# 把部署公钥写入 /home/deploy/.ssh/authorized_keys（0600，deploy 所有）

install -d -m 0755 -o root -g root /opt/clirelay2 /opt/clirelay2/scripts /etc/clirelay2
install -d -m 0755 -o deploy -g deploy /opt/clirelay2/incoming
install -d -m 0755 -o deploy -g deploy /home/web/html /home/web/html/relay-panel-releases
```

## 3. sudoers：env_keep 白名单，不要 SETENV

旧规则带 `SETENV`，deploy 用户因此可以给 root 进程设置任意环境变量，包括 `LD_PRELOAD`、`BASH_ENV` 和旧脚本里的 `RECONCILE_SCRIPT`，等同于拿到 root。新规则只放行工作流实际传的 7 个变量，入口脚本还会逐个校验它们的格式。

```sudoers
# /etc/sudoers.d/90-deploy-clirelay
Defaults:deploy env_reset
Defaults:deploy env_keep = "COMMIT_SHA EXPECTED_SCRIPT_VERSION SERVICE_CPU_QUOTA SERVICE_MEMORY_HIGH SERVICE_MEMORY_MAX SERVICE_TASKS_MAX SERVICE_GO_MEM_LIMIT"
deploy ALL=(root) NOPASSWD: /usr/local/sbin/clirelay-gha-deploy "", /usr/local/sbin/clirelay-gha-deploy --preflight
```

`""` 表示不带参数，第二条只允许 `--preflight`。写入方式：

```bash
# root 执行：先写临时文件校验，通过后再替换
cat > /tmp/90-deploy-clirelay <<'EOF'
Defaults:deploy env_reset
Defaults:deploy env_keep = "COMMIT_SHA EXPECTED_SCRIPT_VERSION SERVICE_CPU_QUOTA SERVICE_MEMORY_HIGH SERVICE_MEMORY_MAX SERVICE_TASKS_MAX SERVICE_GO_MEM_LIMIT"
deploy ALL=(root) NOPASSWD: /usr/local/sbin/clirelay-gha-deploy "", /usr/local/sbin/clirelay-gha-deploy --preflight
EOF
cp -p /etc/sudoers.d/90-deploy-clirelay /root/90-deploy-clirelay.bak-$(date +%Y%m%d%H%M%S) 2>/dev/null || true
visudo -cf /tmp/90-deploy-clirelay && install -m 0440 -o root -g root /tmp/90-deploy-clirelay /etc/sudoers.d/90-deploy-clirelay
visudo -c
```

以 deploy 用户自检：

```bash
sudo -n -l                                  # 规则里不应再出现 SETENV
sudo -n CLIRELAY_SETENV_PROBE=1 /usr/local/sbin/clirelay-gha-deploy --preflight
                                            # 必须被 sudo 拒绝：not allowed to set ... CLIRELAY_SETENV_PROBE
sudo -n EXPECTED_SCRIPT_VERSION=2026.09.25.2 /usr/local/sbin/clirelay-gha-deploy --preflight
                                            # 最后一行应为 CLIRELAY_DEPLOY_CHECK ok ...
```

工作流的 preflight 会做同样的 SETENV 探测。规则仍带 SETENV 的节点会让整次发布在动手前失败。

## 4. /etc/clirelay2/deploy.env

每个节点一份，只能由 root 写。文件按 `KEY=value` 逐行解析，不会被 source，所以值里的 `$(...)` 不会执行。出现未知的键或格式不对的值时，部署直接拒绝，避免拼写错误被静默忽略。文件不存在时全部使用下表的默认值，这组默认值就是原单节点主机的配置。

| 键 | 默认值 | 说明 |
|---|---|---|
| `DOMAIN` | `relay.07230805.xyz` | 用它查找要切流的 nginx 文件，也作为切流后 smoke 的主机名 |
| `NODE_PUBLIC_IP` | 空 | 切流后 smoke 使用 `curl --resolve DOMAIN:443:NODE_PUBLIC_IP`，走完整链路（443 → stream 按 SNI 分流 → 127.0.0.1:8444 → slot）。为空时直连 `SMOKE_LOCAL_TLS_ADDR` |
| `SMOKE_LOCAL_TLS_ADDR` | `127.0.0.1:8444` | 未设置 `NODE_PUBLIC_IP` 时，smoke 用 `--connect-to` 连到这里，仍带 SNI 并校验证书 |
| `NGINX_CONF` | 空（自动查找） | 切流文件的绝对路径 |
| `NGINX_SEARCH_DIRS` | `/etc/nginx/conf.d /etc/nginx/sites-enabled /etc/nginx/sites-available` | 自动查找的目录 |
| `NGINX_CONTAINER` | `nginx` | nginx 跑在容器里时的容器名 |
| `SLOT_USER` / `SLOT_GROUP` | 空 | slot 的运行用户和组。为空时从基础单元复制 `User=`/`Group=`；**没有基础单元时必须填写**，否则部署失败，不会以 root 运行 |
| `DRAIN_SECONDS` | `180` | 旧 slot 在切流后继续运行的秒数。工作流会读取节点实际值，按“该值 + 30 秒”安排探测时长 |
| `SHUTDOWN_GRACE_SECONDS` | `300` | 旧 slot 收到 SIGTERM 后完成在途请求的上限 |
| `HEALTH_TIMEOUT_SECONDS` | `90` | 新 slot 就绪等待时间 |
| `SMOKE_TIMEOUT_SECONDS` | `30` | 切流后 smoke 的等待时间 |
| `MIN_AVAILABLE_MB` | `512` | 部署前要求的可用内存 |
| `GO_MEM_LIMIT_PERCENT` | `85` | GOMEMLIMIT 相对 MemoryHigh 的比例 |
| `SERVICE_CPU_QUOTA` | workflow 传入（`170%`） | slot 单元的 `CPUQuota=`，格式如 `120%` |
| `SERVICE_MEMORY_HIGH` | workflow 传入（`1400M`） | `MemoryHigh=`，格式如 `900M`、`2G`，或 `infinity` |
| `SERVICE_MEMORY_MAX` | workflow 传入（`1600M`） | `MemoryMax=`，格式同上 |
| `SERVICE_TASKS_MAX` | workflow 传入（`512`） | `TasksMax=`，整数或 `infinity` |
| `SERVICE_GO_MEM_LIMIT` | 按 MemoryHigh 的 85% 推导 | `GOMEMLIMIT`，纯字节数或 `700MiB` 这种 Go 格式（不接受 `700M`） |

资源上限的优先级：**deploy.env 高于 workflow**。workflow 的值来自仓库级变量（`CLIRELAY_SERVICE_*`），所有节点共用一份；deploy.env 由节点本机的 root 管理，更贴近那台机器的实际情况。写了哪个键，就只覆盖哪个键。这四个限制不能写成空值，因为空值会让对应的限制从单元里消失；要解除内存或任务数限制，请明确写 `infinity`。

GOMEMLIMIT 必须低于 MemoryHigh。节点设置了自己的 `SERVICE_MEMORY_HIGH`、但没设置 `SERVICE_GO_MEM_LIMIT` 时，会丢弃 workflow 传入的 GOMEMLIMIT，改按本机 MemoryHigh 的 `GO_MEM_LIMIT_PERCENT` 重新推导，以免照搬为 1400M 设计的值。实际生效值可以用 `--print-settings` 查看，preflight 汇总里的 `limits=` 字段也会列出。

示例（两台节点各一份）：

```bash
# n43：总内存 3.9G，同机还有 PostgreSQL（Patroni）、etcd 和 Redis。root 执行
cat > /tmp/deploy.env <<'EOF'
DOMAIN=relay.07230805.xyz
NODE_PUBLIC_IP=203.0.113.43
NGINX_CONF=/etc/nginx/conf.d/relay.07230805.xyz.conf
# 与该机现有 slot 单元一致：CPUQuota=120%、MemoryHigh=900M、MemoryMax=1100M、GOMEMLIMIT=700MiB
SERVICE_CPU_QUOTA=120%
SERVICE_MEMORY_HIGH=900M
SERVICE_MEMORY_MAX=1100M
SERVICE_GO_MEM_LIMIT=734003200
# 没有基础单元 clirelay2.service 时必须填写：
# SLOT_USER=clirelay
# SLOT_GROUP=clirelay
EOF
install -m 0644 -o root -g root /tmp/deploy.env /etc/clirelay2/deploy.env

# n156（8G）同理：NODE_PUBLIC_IP=198.51.100.156，NGINX_CONF 写该机的实际文件；
# 资源键可以不写，沿用 workflow 的 170% / 1400M / 1600M / 512，GOMEMLIMIT 按 85% 推导
```

查看生效值和节点检查结果：

```bash
sudo bash /opt/clirelay2/scripts/deploy-blue-green.sh --print-settings
sudo bash /opt/clirelay2/scripts/deploy-blue-green.sh --check     # 只读
```

## 5. 基础单元 clirelay2.service

slot 单元 `clirelay2-8318` / `clirelay2-8319` 由部署脚本生成，会从基础单元 `clirelay2.service` 复制 `User=`、`Group=`、`WorkingDirectory=` 和 `Environment=`，并从 `ExecStart` 中读取 `-config` 路径。基础单元只作为模板，**不要 enable 或 start**；排空旧 slot 时，清理脚本也会顺手把它 disable。

```ini
# /etc/systemd/system/clirelay2.service
[Unit]
Description=CliRelay base unit (template for the blue-green slots; do not start)
After=network.target

[Service]
Type=simple
User=clirelay
Group=clirelay
WorkingDirectory=/opt/clirelay2
ExecStart=/opt/clirelay2/clirelay2 -config /opt/clirelay2/config.yaml
```

- `EnvironmentFile` 不会被复制。部署脚本发现 `/opt/clirelay2/.env` 存在时会自动加上，环境变量统一放在那里。
- 基础单元存在但没有 `User=` 的旧主机，slot 仍以 root 运行，行为与以前一致，部署日志会给出警告。要改为非 root，在 deploy.env 里设置 `SLOT_USER`，并按第 2 节调整可写文件的属主。
- 没有基础单元时，`SLOT_USER` 必填。`config.yaml` 默认取 `/opt/clirelay2/config.yaml`。

```bash
# root 执行
useradd --system --home-dir /opt/clirelay2 --shell /usr/sbin/nologin clirelay 2>/dev/null || true
systemctl daemon-reload     # 写好 clirelay2.service 之后
```

## 6. nginx 布局要求

- 反代 `DOMAIN` 到 `127.0.0.1:8318/8319` 的 live 文件必须**恰好一个**。upstream 块、8446 溢出口、接收对端溢出的 8445 监听块要和 `server_name` 放在**同一个文件**里。
- 这个文件以外的 live 文件（`conf.d`、`sites-enabled`）不能再出现 `127.0.0.1:8318/8319`。否则旧 slot 永远排空不了，preflight 会直接拒绝。退役的旧文件改名为 `*.bak*` 或 `*bak-before*`，或者移出这两个目录。
- 文件里所有指向 slot 的位置必须是同一个端口。切流时会逐处改写并逐处校验，各处不一致的配置会被拒绝，脚本不会猜哪一半才是生效的。注释里写到的端口不算路由。
- 新节点第一次部署从 8318 起，模板里写 8318 或 8319 都可以。已有节点写当前 active 的端口（`cat /opt/clirelay2/.active-port`）。

## 7. 现有节点升级到本版脚本（一次性）

顺序：先改 sudoers，再同步脚本并写 deploy.env，然后配置 GitHub，最后合并 PR。脚本同步之后、PR 合并之前，旧工作流触发的部署会因为版本不匹配在节点上失败，这是安全的失败，不会碰线上 slot。

```bash
# 本机执行（在 CliRelay 仓库根目录），<root@node> 换成节点的 root ssh 目标
for f in deploy-blue-green.sh cleanup-drained-slot.sh reconcile-active-slot.sh; do
  cat "scripts/$f" | ssh <root@node> "cat > /tmp/$f"
done
cat scripts/clirelay-gha-deploy | ssh <root@node> 'cat > /tmp/clirelay-gha-deploy'
shasum -a 256 scripts/deploy-blue-green.sh scripts/cleanup-drained-slot.sh scripts/reconcile-active-slot.sh scripts/clirelay-gha-deploy

ssh <root@node> 'set -e
  sha256sum /tmp/deploy-blue-green.sh /tmp/cleanup-drained-slot.sh /tmp/reconcile-active-slot.sh /tmp/clirelay-gha-deploy
  for f in deploy-blue-green.sh cleanup-drained-slot.sh reconcile-active-slot.sh clirelay-gha-deploy; do bash -n "/tmp/$f"; done
  ts=$(date +%Y%m%d%H%M%S)
  for f in deploy-blue-green.sh cleanup-drained-slot.sh reconcile-active-slot.sh; do
    [ ! -f "/opt/clirelay2/scripts/$f" ] || cp -p "/opt/clirelay2/scripts/$f" "/root/$f.bak-$ts"
    install -m 0755 -o root -g root "/tmp/$f" "/opt/clirelay2/scripts/$f"
  done
  [ ! -f /usr/local/sbin/clirelay-gha-deploy ] || cp -p /usr/local/sbin/clirelay-gha-deploy "/root/clirelay-gha-deploy.bak-$ts"
  install -m 0755 -o root -g root /tmp/clirelay-gha-deploy /usr/local/sbin/clirelay-gha-deploy
  grep -m1 "^SCRIPT_VERSION=" /opt/clirelay2/scripts/deploy-blue-green.sh'
```

两边的 sha256 要一致，最后一行应输出 `SCRIPT_VERSION='2026.09.25.2'`。然后按第 3 节自检一次。

## 8. GitHub 变量与 secrets

节点名只能由字母、数字和下划线组成（它会成为 secret 名的后缀；GitHub 的 secret 名不区分大小写，所以节点名也不能只靠大小写区分）。名为 `default` 的节点使用原来不带后缀的 secrets，所以不配置 `CLIRELAY_DEPLOY_NODES` 时就是原来的单节点部署。

| 用途 | 后端仓库 kittors/CliRelay | 前端仓库 kittors/codeProxy | 缺省时 |
|---|---|---|---|
| 节点与发布顺序（变量） | `CLIRELAY_DEPLOY_NODES` | `RELAY_DEPLOY_NODES` | `["default"]` |
| SSH 地址（secret） | `SERVER_HOST_<node>` | 同左 | 必填；`default` 用 `SERVER_HOST` |
| SSH 端口（secret） | `SERVER_PORT_<node>` | 同左 | 22；`default` 用 `SERVER_PORT` |
| 部署私钥（secret） | `SSH_PRIVATE_KEY_<node>` | 同左 | 用共享的 `SSH_PRIVATE_KEY` |
| known_hosts（secret） | `DEPLOY_SSH_KNOWN_HOSTS_<node>` | 同左 | 用共享的 `DEPLOY_SSH_KNOWN_HOSTS` |
| SSH 跳板（secret，可选） | `SSH_JUMP_<node>`，格式 `user@host[:port]` | 同左 | 不设则直连节点 |
| 探测绑定的公网 IP | 变量 `CLIRELAY_SMOKE_IP_<node>` 或 secret `SMOKE_IP_<node>` | 变量 `RELAY_SMOKE_IP_<node>` 或 secret `SMOKE_IP_<node>` | `SERVER_HOST` 为 IP 时直接用它 |

只有私钥和 known_hosts 会回退到共享 secret；地址和端口不回退，否则漏配的节点会被当成另一台机器重复部署，自己却被报告为已完成。非 22 端口的 known_hosts 条目必须是 `[host]:port` 形式（`ssh-keyscan -p <port> <host>` 的输出就是这种形式），preflight 会逐节点核对。

```bash
REPO=kittors/CliRelay     # 前端改为 kittors/codeProxy，变量名换成 RELAY_ 前缀
gh variable set CLIRELAY_DEPLOY_NODES --repo "$REPO" --body '["n43","n156"]'
for node in n43 n156; do
  gh secret set "SERVER_HOST_${node}" --repo "$REPO" --body '<该节点 SSH 地址>'
  gh secret set "SERVER_PORT_${node}" --repo "$REPO" --body '<该节点 SSH 端口>'
  ssh-keyscan -p '<该节点 SSH 端口>' '<该节点 SSH 地址>' 2>/dev/null | gh secret set "DEPLOY_SSH_KNOWN_HOSTS_${node}" --repo "$REPO"
  # 可选：各节点使用独立部署密钥
  # gh secret set "SSH_PRIVATE_KEY_${node}" --repo "$REPO" < '<私钥文件>'
  # 可选：SSH 地址不是对外提供 HTTPS 的公网 IP 时
  # gh variable set "CLIRELAY_SMOKE_IP_${node}" --repo "$REPO" --body '<该节点公网 IP>'
done
gh variable list --repo "$REPO"
gh secret list --repo "$REPO"
```

### 经跳板连接节点

有的服务商会丢弃 GitHub 运行机部分地址段发来的入站 SSH（表现为 `ssh: connect to host … Connection timed out`，节点的 sshd 日志里没有任何连接记录）。这时给该节点设置 `SSH_JUMP_<node>`，工作流会经跳板机连接（`ProxyJump`）。跳板机的主机密钥要一起写进该节点的 `DEPLOY_SSH_KNOWN_HOSTS_<node>`，同样严格校验。

跳板机上用一个只能转发到这个节点 SSH 端口的受限账号，不给 shell：

```bash
# 跳板机 root 执行（示例：只允许转发到 156.225.27.154:47222）
useradd -m -s /sbin/nologin gha-jump
install -d -m 0700 -o gha-jump -g gha-jump /home/gha-jump/.ssh
printf 'restrict,port-forwarding,permitopen="156.225.27.154:47222" %s\n' '<部署公钥>' > /home/gha-jump/.ssh/authorized_keys
chown gha-jump:gha-jump /home/gha-jump/.ssh/authorized_keys && chmod 0600 /home/gha-jump/.ssh/authorized_keys

# 本机：写 secrets（known_hosts 同时包含节点和跳板机的主机密钥，写入前核对指纹）
gh secret set SSH_JUMP_n156 --repo "$REPO" --body 'gha-jump@<跳板IP>:<跳板SSH端口>'
cat node-known-hosts jump-known-hosts | gh secret set DEPLOY_SSH_KNOWN_HOSTS_n156 --repo "$REPO"
```

`--repo` 不能省略，否则 gh 会去查 upstream 仓库。known_hosts 写入前要人工核对指纹，不要盲信 `ssh-keyscan` 的结果。

## 9. 排障

| 现象 | 原因与处理 |
|---|---|
| preflight：`is version 'x', this workflow expects 'y'` | 该节点的 root 脚本没同步，按第 7 节处理 |
| preflight：`root preflight failed` 且没有 `CLIRELAY_DEPLOY_CHECK` | 入口是旧版（不认识 `--preflight`），或 `--check` 报错，看日志里的具体原因 |
| preflight：`still grants SETENV` | 按第 3 节替换 sudoers |
| preflight：`known_hosts has no entry for [host]:port` | known_hosts 的条目格式或端口不对 |
| `live nginx files besides ... also proxy to a slot` | slot 引用分散在多个文件里，见第 6 节 |
| `routes to both slots` | 同一文件内各处端口不一致，手工统一成当前 active 端口 |
| `base unit clirelay2.service not found ... set SLOT_USER` | 新节点没有基础单元，按第 5 节补上，或在 deploy.env 里设置 `SLOT_USER` |
| 节点验证：`old slot ... is still active` | 排空没有执行或被拒绝，root 执行 `journalctl -u 'clirelay2-drain-*'` 查看原因。该节点已在新版本上，后续节点未动 |
| 最终公网探测：`answered by a build other than ...` | DNS 指向的某台机器不在节点列表里 |
