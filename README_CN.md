<p align="center">
  <img src="https://img.shields.io/badge/Go-1.26+-00ADD8?style=for-the-badge&logo=go&logoColor=white" alt="Go">
  <img src="https://img.shields.io/badge/License-MIT-22c55e?style=for-the-badge" alt="License">
  <img src="https://img.shields.io/github/stars/kittors/CliRelay?style=for-the-badge&color=f59e0b" alt="Stars">
  <img src="https://img.shields.io/github/forks/kittors/CliRelay?style=for-the-badge&color=8b5cf6" alt="Forks">
</p>

<h1 align="center">🔀 CliRelay</h1>

<p align="center">
  <strong>统一的 AI CLI 代理服务器 — 用你<em>现有的</em>订阅接入任何 OpenAI / Gemini / Claude / Codex 兼容客户端。</strong>
</p>

<p align="center">
  <a href="README.md">English</a> | 中文
</p>

<p align="center">
  <a href="https://help.router-for.me/cn/">📖 文档</a> ·
  <a href="https://github.com/kittors/codeProxy">🖥️ 管理面板</a> ·
  <a href="https://github.com/kittors/CliRelay/issues">🐛 报告问题</a> ·
  <a href="https://github.com/kittors/CliRelay/pulls">✨ 功能请求</a>
</p>

---

## ⚡ CliRelay 是什么？

> **✨ 基于 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 的深度增强版** — 补强了生产级管理层、Web 控制面板托管能力，以及面向日常运维的终端 TUI。

CliRelay 会把 AI CLI 订阅、OAuth 凭据、API Key 以及兼容上游服务整合成一个可管理的 API 层。它可以让 Claude Code、Gemini CLI、OpenAI Codex、Qwen、iFlow、Kimi、Antigravity、xAI/Grok、OpenCode Go、ClinePass、Ollama Cloud、Bedrock、Amp、Vertex、OpenAI 兼容客户端等工具通过统一端点访问多类上游，同时围绕流量提供分组路由、故障转移、请求日志、配额管控、模型价格、生图配置、API Key 自助查询、在线更新、`/manage` Web 面板托管和终端管理流程。

当前运行时数据栈是 PostgreSQL 15+、Redis 7+ 和 Ent ORM。PostgreSQL 是运行时数据事实源；Redis 只承担缓存、锁、限流、队列和可重建状态。SQLite 已经是旧版本数据源，只在迁移导入时保留支持。

```
┌───────────────────────┐         ┌──────────────┐         ┌────────────────────┐
│   AI 编程工具          │         │              │         │  上游服务商          │
│                       │         │              │ ──────▶ │  Google Gemini      │
│  Claude Code          │ ──────▶ │   CliRelay   │ ──────▶ │  OpenAI / Codex    │
│  Gemini CLI           │         │   :8317      │ ──────▶ │  Anthropic Claude  │
│  OpenAI Codex         │         │              │ ──────▶ │  Qwen / iFlow      │
│  Amp CLI / IDE        │         │              │ ──────▶ │  Antigravity/xAI   │
│  任意 OAI 兼容客户端   │         └──────────────┘         │  Vertex / Bedrock  │
└───────────────────────┘                                  │  OpenCode/Cline    │
                                                           │  Ollama / Amp      │
                                                           └────────────────────┘
```

## ✨ 核心特性

### 🔌 多服务商代理引擎

| 特性 | 说明 |
|:-----|:-----|
| 🌐 **统一端点** | 一个 `http://localhost:8317` 统一承接 Gemini、Claude、Codex、Qwen、iFlow、Kimi、Antigravity、xAI/Grok、Vertex、Bedrock、OpenCode Go、ClinePass、Ollama Cloud、OpenAI 兼容上游以及 Amp 集成 |
| ⚖️ **智能负载均衡** | 跨多个 API Key 的轮询或填充优先调度策略 |
| 🧭 **分组与路径路由** | 将渠道绑定到分组，按 API Key 限制可用分组，并为团队或业务暴露自定义路径命名空间 |
| 🔄 **自动故障转移** | 配额耗尽或发生错误时自动切换到备用渠道 |
| 🧠 **多模态支持** | 完整支持文本 + 图片输入、生图路由、Function Calling（工具调用）和 SSE 流式响应 |
| 🔗 **OpenAI 兼容** | 支持任何兼容 OpenAI Chat Completions 协议的上游服务 |

### 📊 请求日志与监控（PostgreSQL）

| 特性 | 说明 |
|:-----|:-----|
| 📝 **完整请求捕获** | 每个 API 请求记录到 PostgreSQL：时间戳、模型、Token（输入/输出/推理/缓存）、延迟、状态、来源渠道 |
| 💬 **消息体存储** | 完整的请求/响应消息内容以压缩形式存入 PostgreSQL，并支持将正文保留策略与元数据保留策略分离 |
| 🔍 **高级查询** | 按 API Key、模型、状态、时间范围过滤日志，高效分页（LIMIT/OFFSET） |
| 📈 **分析聚合** | 预计算仪表盘：每日趋势、模型分布、每小时热力图、单 Key 统计 |
| 🏥 **健康评分引擎** | 实时 0–100 健康评分，综合考虑成功率、延迟、活跃渠道和错误模式 |
| 📡 **WebSocket 监控** | 通过 WebSocket 实时推送系统状态：CPU、内存、goroutines、网络 I/O、数据库大小 |
| 🗄️ **Ent + PostgreSQL** | 使用 PostgreSQL 15+ 作为运行时主数据库，并保留 Ent 生成的 schema 元数据 |

### 🔐 API Key 与权限管理

| 特性 | 说明 |
|:-----|:-----|
| 🔑 **API Key CRUD** | 通过管理 API 创建、编辑、删除 API Key — 支持自定义名称、备注和独立启用/禁用开关 |
| 📊 **单 Key 配额** | 为每个 Key 设置最大 Token / 请求配额，系统自动执行限制 |
| ⏱️ **速率限制** | 单 Key 速率限制（每分钟/每小时请求数） |
| 👥 **多人权限划分** | 可将 API Key 分配给不同用户或团队，并限制可用渠道分组和模型权限 |
| 🔒 **Key 脱敏** | API Key 在 UI 和日志中始终脱敏显示（`sk-***xxx`） |
| 🌍 **公开查询页面** | 终端用户可通过公开自助页面查询自己的用量统计和请求日志（无需登录） |

### 🔗 服务商渠道管理

| 特性 | 说明 |
|:-----|:-----|
| 📋 **多标签页配置** | 按服务商类型组织渠道管理：Gemini、Claude、Codex、OpenCode Go、ClinePass、Ollama Cloud、Vertex、Bedrock、OpenAI 兼容、Ampcode |
| 🏷️ **渠道命名** | 每个渠道支持自定义名称、备注、代理 URL、自定义 Headers 和模型别名映射 |
| 🧩 **可复用代理池** | 统一维护出站代理配置，并按需分配给 OAuth / auth 渠道 |
| ⏱️ **延迟追踪** | 每渠道平均延迟（`latency_ms`）追踪，带可视化指标 |
| 🔄 **启用/禁用** | 单独切换渠道开关，无需删除 |
| 🚫 **模型排除** | 从渠道中排除特定模型（例如：在备用 Key 上屏蔽高价模型） |
| 🧾 **模型库同步** | 支持自定义模型维护，并从 OpenRouter 同步模型 ID 与价格用于配额核算 |
| 📊 **渠道统计** | 每渠道成功/失败次数和模型可用性展示在渠道卡片上 |

### 🛡️ 安全与认证

| 特性 | 说明 |
|:-----|:-----|
| 🔐 **OAuth 支持** | 原生 OAuth 流程覆盖 Gemini、Claude、Codex、Qwen、iFlow、Antigravity、Kimi、xAI/Grok，并在支持的渠道中提供设备码 / 浏览器 / Cookie 变体 |
| 🪪 **身份指纹维护** | 集中维护上游身份信息，让请求在不同 provider 侧保持一致的客户端指纹 |
| 🔒 **TLS 处理** | 可配置的上游通信 TLS 设置 |
| 🏠 **面板隔离** | 管理面板访问由管理员密码独立控制 |
| 🛡️ **请求伪装** | 上游请求自动剥离客户端标识 Headers，保护隐私 |

### 🛠️ 运维体验

| 特性 | 说明 |
|:-----|:-----|
| 🖥️ **可视化管理面板** | 在 `/manage` 中配置服务商、认证、API Key、模型、路由、日志、更新与系统状态 |
| 🌐 **中英文界面** | 管理面板内置 i18n，Docker Compose 和 TUI 也支持语言选择 |
| 🌙 **Dark Mode** | 为长时间运维提供完整暗色主题 |
| 🧬 **可视化配置编辑** | 可通过表单编辑运行时配置，也能切换到 YAML 源码视图精细控制 |
| 🔄 **在线更新机制** | 在面板中检查版本、查看更新内容、触发 updater sidecar，并等待后端恢复 |
| 📥 **CC Switch 导入** | 将 cc-switch 风格配置导入到可管理的模型/渠道工作区 |

### 🗄️ 数据持久化

| 特性 | 说明 |
|:-----|:-----|
| 💾 **PostgreSQL 存储** | 用量、请求日志、消息正文、API Key、路由、代理池、模型配置和配额状态都存入 PostgreSQL |
| 🔄 **Redis 运行时状态** | Redis 7+ 负责缓存、锁、限流、队列和可重建快照；PostgreSQL 始终是事实源 |
| 🗃️ **可插拔认证/配置后端** | 默认使用本地文件，也支持通过 PostgreSQL、Git 或 S3 兼容对象存储持久化配置和认证信息 |
| 📦 **配置快照** | 导入/导出整个系统配置为 JSON，便于备份和迁移 |

## 🛠️ 运行时与技术栈

| 层级 | 技术 |
|:-----|:-----|
| 运行时 | Go 1.26、Gin、Docker Compose |
| 数据 | PostgreSQL 15+ / Ent ORM，Redis 7+ 承担可重建运行时状态 |
| 认证 / 配置存储 | 本地文件、PostgreSQL、Git 或 S3 兼容对象存储 |
| 代理核心 | OpenAI Chat Completions / Responses、Anthropic Messages、Gemini、服务商专用执行器、SSE 与 WebSocket 链路 |
| 运维 | Bubble Tea / Lipgloss TUI、`/manage` Web 面板托管、updater sidecar |
| 可观测性 | PostgreSQL 请求日志、压缩消息正文、实时日志、系统状态 WebSocket |

## 📸 管理面板预览

CliRelay 可以在 `/manage` 暴露内置 Web 控制面板。服务端既可以托管打包后的 SPA 资源，也可以回退到同步的管理面板资源。

下面这组 gallery 使用了最新提供的截图素材，覆盖当前管理面板的完整工作流。

### 仪表盘与监控

| 仪表盘概览 | 系统健康 |
| :--------- | :------- |
| <img src="docs/images/readme-showcase/dashboard-overview.png" width="100%" alt="CliRelay 仪表盘概览" /> | <img src="docs/images/readme-showcase/dashboard-health.png" width="100%" alt="CliRelay 健康分与系统监控" /> |

| 流量趋势 | 监控汇总 |
| :------- | :------- |
| <img src="docs/images/readme-showcase/dashboard-traffic.png" width="100%" alt="CliRelay 流量趋势图" /> | <img src="docs/images/readme-showcase/monitor-summary.png" width="100%" alt="监控中心汇总图表" /> |

| 监控拆分 | 请求日志 |
| :------- | :------- |
| <img src="docs/images/readme-showcase/monitor-breakdown.png" width="100%" alt="监控中心模型与 API Key 拆分" /> | <img src="docs/images/readme-showcase/request-logs.png" width="100%" alt="请求日志表格与过滤器" /> |

| 请求详情 | API Key 独立查询页 |
| :------- | :----------------- |
| <img src="docs/images/readme-showcase/request-details.png" width="100%" alt="请求详情查看器" /> | <img src="docs/images/readme-showcase/api-key-lookup.png" width="100%" alt="API Key 独立查询页面" /> |

### 服务商、认证与权限

| OpenCode Go 认证文件 | Claude 认证控制 |
| :------------------- | :-------------- |
| <img src="docs/images/readme-showcase/auth-files-opencode-go.png" width="100%" alt="OpenCode Go 认证文件管理" /> | <img src="docs/images/readme-showcase/auth-files-claude.png" width="100%" alt="Claude 认证文件管理" /> |

| Claude OAuth 健康 | API Keys |
| :---------------- | :------- |
| <img src="docs/images/readme-showcase/auth-files-claude-oauth.png" width="100%" alt="Claude OAuth 健康与账号状态" /> | <img src="docs/images/readme-showcase/api-keys.png" width="100%" alt="API Key 管理表格" /> |

| API Key 权限配置 | 代理池 |
| :--------------- | :----- |
| <img src="docs/images/readme-showcase/api-key-permissions.png" width="100%" alt="API Key 权限配置" /> | <img src="docs/images/readme-showcase/proxy-pool.png" width="100%" alt="可复用代理池管理" /> |

### 路由、模型与配置

| CC Switch 导入 | 生图配置 |
| :------------- | :------- |
| <img src="docs/images/readme-showcase/cc-switch-import.png" width="100%" alt="CC Switch 导入设置" /> | <img src="docs/images/readme-showcase/image-generation.png" width="100%" alt="生图渠道配置" /> |

| 渠道分组 | 模型目录 |
| :------- | :------- |
| <img src="docs/images/readme-showcase/channel-groups.png" width="100%" alt="配置分组调用策略与自定义调用路径" /> | <img src="docs/images/readme-showcase/models.png" width="100%" alt="模型目录与价格管理" /> |

| 运行时配置 | 系统信息 |
| :--------- | :------- |
| <img src="docs/images/readme-showcase/config.png" width="100%" alt="运行时配置编辑器" /> | <img src="docs/images/readme-showcase/system-info.png" width="100%" alt="系统信息页面" /> |

| 运行时日志 |
| :--------- |
| <img src="docs/images/readme-showcase/live-logs.png" width="100%" alt="运行时日志查看器" /> |

> 🔗 面板资源仓库可通过 `remote-management.panel-github-repository` 配置，默认仓库为 [kittors/codeProxy](https://github.com/kittors/codeProxy)。

## 🏗️ 支持的服务商

| 服务商 / 通道 | 认证方式 | 说明 |
|:--------------|:---------|:-----|
| Google Gemini | OAuth + API Key | 适配 Gemini CLI / AI Studio 风格链路 |
| Anthropic Claude | OAuth + API Key | 面向 Claude Code 与 Claude 兼容客户端 |
| OpenAI Codex | OAuth + API Key | 包含 Responses 与 WebSocket 桥接能力 |
| Qwen | OAuth | 通义千问 Qwen Code 风格登录流程 |
| iFlow / GLM | OAuth + Cookie | 支持 iFlow 路由及相关模型族 |
| Kimi | OAuth | 浏览器登录流程 |
| xAI / Grok | OAuth | Grok CLI 兼容 OAuth 与配额元数据 |
| Antigravity | OAuth | 独立 OAuth 通道，支持模型回填 |
| Vertex 兼容端点 | API Key | 支持自定义 base URL、Header、别名与排除规则 |
| AWS Bedrock | API Key / SigV4 | 支持按区域访问 Bedrock Runtime 和 Claude 模型别名 |
| OpenCode Go | API Key | 固定 OpenCode Go 上游，支持用量查询和视觉模型 fallback |
| ClinePass | API Key | OpenAI 兼容 ClinePass 路由和模型访问控制 |
| Ollama Cloud | API Key | OpenAI 兼容 Ollama Cloud 路由和模型访问控制 |
| OpenAI 兼容上游 | API Key | OpenRouter、Grok 兼容端点及自定义 provider |
| Amp 集成 | 上游 API Key + 映射 | 可直接回退到 Amp 上游，也可映射到本地可用模型 |

## 🚀 快速开始

### 🐳 使用 Docker Compose 安装

Docker Compose 是 CliRelay 推荐的安装方式。仓库内的 `docker-compose.yml` 会启动 CliRelay、PostgreSQL 15、Redis 7 和 updater sidecar。`.env` 不是必需的：首次执行 `docker compose up -d` 时，`clirelay-init` 会自动创建 `.env`，生成缺失的 `CLIRELAY_UPDATER_TOKEN`、`CLIRELAY_ADMIN_PASSWORD` 和 `CLIRELAY_POSTGRES_PASSWORD`，保留已有非空值，并在缺少 `config.yaml` 时从 `config.example.yaml` 创建一份。`CLIRELAY_ADMIN_PASSWORD` 用于空数据库首次创建 `admin`；初始化脚本会生成 32 个十六进制字符的安全随机值，你也可以提前在 `.env` 中设置不少于 12 个字符的自定义密码。生产环境只有在需要固定自己的密钥或挂载路径时，才需要提前写 `.env`。

```bash
git clone https://github.com/kittors/CliRelay.git
cd CliRelay
mkdir -p auths logs data
# Linux 绑定挂载目录需要允许容器用户 10001:10001 访问：
sudo chown -R 10001:10001 auths logs data
docker compose up -d
```

镜像内的业务进程以 `10001:10001` 运行。`config.yaml` 至少要让该用户可读；如果要在 Web 管理面板中修改并保存配置，还必须可写。首次启动生成文件后如遇权限错误，可在宿主机执行 `sudo chown 10001:10001 config.yaml`，然后重启业务容器。

Synology/DSM 共享文件夹可能同时受 ACL 控制。如果容器 entrypoint 内的 `chown` 失败，请在 DSM 或宿主机 SSH 中调整绑定目录和 `config.yaml` 的 ACL/属主权限，不要依赖容器内重试；处理完成后执行 `docker compose restart cli-proxy-api`。

首次启动后，编辑自动生成的 `config.yaml` 添加你的 API 密钥或 OAuth 凭据，然后重启服务：

```bash
docker compose restart cli-proxy-api
```

默认情况下，客户端 API 路由（`/v1`、`/v1beta`）需要 API Key；如需在未配置 client key 的情况下运行，可设置 `allow-unauthenticated: true`（生产环境不推荐）。

启动后常用入口：

- API 地址：`http://localhost:8317`
- Web 面板：`http://localhost:8317/manage`
- 查看日志：`docker compose logs -f cli-proxy-api`
- 重启服务：`docker compose restart cli-proxy-api`
- 停止服务：`docker compose down`
- 打开 TUI：`docker compose exec cli-proxy-api ./cli-proxy-api -tui`
- OAuth 登录模式：`docker compose exec cli-proxy-api ./cli-proxy-api -login`

如果你使用 Docker Compose 部署，也可以在环境变量中设置 `CLIRELAY_LOCALE=en` 或 `CLIRELAY_LOCALE=zh`，控制 TUI 的默认语言。

如果云平台只允许一个挂载目录，可以把 `AUTH_PATH` 设置为容器内的认证目录，例如 `/CLIProxyAPI/auths`。`CLI_PROXY_AUTH_PATH` 仍表示宿主机侧绑定路径，`AUTH_PATH` 会同时作为容器内挂载目标，并在运行时覆盖 `auth-dir`。

如果不希望自动提示更新，可以在 `config.yaml` 中关闭，或在配置页关闭 **自动检查更新**：

```yaml
auto-update:
  enabled: false
```

更新检查默认跟随稳定的 `main` Docker 镜像。如果你想测试 dev 构建，可以在 `config.yaml` 中设置 `channel: dev`，或在配置页的 **更新渠道** 中选择 **开发版（dev）**：

```yaml
auto-update:
  channel: dev
```

### 🗄️ 运行时数据栈

CliRelay 当前运行时数据栈已经完全统一为 PostgreSQL 15+、Redis 7+ 和 Ent ORM：PostgreSQL 是业务数据唯一事实源，Redis 只负责缓存、锁、限流、队列和可重建状态。SQLite 不再作为运行时数据库，也不参与正常启动、健康检查或 OTA 在线更新。

标准 Docker Compose 部署会启动 `clirelay-init`、PostgreSQL、Redis、业务容器和 updater sidecar。正常执行 `docker compose up -d` 或在管理面板中在线更新时，系统**不会扫描 `usage.db`、不会执行 SQLite inventory、不会自动导入 SQLite，也不会在更新进度中出现 SQLite 迁移阶段**。旧 compose 中残留的 `clirelay-migrate` 服务和 `CLIRELAY_SQLITE_AUTO_*` 启动配置会在部署栈升级时移除，但原始 SQLite 文件不会被删除或修改。

OTA 更新状态由 updater sidecar 持有，并通过 SSE 实时发送。管理面板只展示 updater 返回的任务 ID、实际执行阶段、已完成步骤、当前/目标版本、管理面板版本、目标镜像、最新 Release 信息和最终结果，不再通过前端计时器模拟百分比。更新期间即使业务容器重启、页面刷新或 SSE 短暂断开，前端也会重新连接并读取 updater 的最新快照。Compose 默认把快照保存到 `.clirelay-updater-status.json`；更新器异常重启时，未完成任务会被明确标记为失败，不会继续显示为运行中。 业务容器通过健康检查后，当前 updater 会启动一个基于目标镜像的临时 helper，由 helper 安全重建 updater sidecar，使后续 OTA 继续使用目标版本的更新逻辑。

#### 仅旧版本用户需要的 SQLite 手工迁移

仓库和 Docker 镜像仍保留 `scripts/migrate-sqlite-to-postgres.sh`，只用于从旧版 SQLite `usage.db` 手工导入 PostgreSQL。该脚本是独立迁移工具，**不会被 CliRelay 启动流程或 OTA 自动调用**。全新安装、已经使用 PostgreSQL 的部署，以及不需要保留旧 SQLite 历史数据的用户都不应执行它。

迁移前必须：

1. 备份原始 `usage.db`，并保留只读副本；不要在迁移过程中让旧版本继续写入该文件。
2. 先准备并启动 PostgreSQL 15+ 和 Redis 7+，确认 `CLIRELAY_POSTGRES_DSN` 指向正确的目标库。
3. 先运行 inventory 和 PostgreSQL dry-run，检查表、行数、ID/时间范围、checksum 与计划写入数量。
4. 只有核对 dry-run 后才执行 apply；完成后再次校验 PostgreSQL 数据。脚本不会删除、移动或写入 SQLite 文件，重复导入由 PostgreSQL 中的导入记录和 advisory lock 保护。

非 Docker 部署可以直接执行：

```bash
CLIRELAY_BIN=/opt/clirelay2/clirelay2 \
CLIRELAY_POSTGRES_DSN='postgres://user:pass@127.0.0.1:5432/cliproxy?sslmode=disable' \
./scripts/migrate-sqlite-to-postgres.sh /path/to/usage.db
```

Docker Compose 部署先确保 PostgreSQL/Redis 已启动，再把旧库只读挂载到一次性容器中：

```bash
docker compose up -d postgres redis

docker compose run --rm --no-deps \
  -e CLIRELAY_BIN=/CLIProxyAPI/CLIProxyAPI \
  -v /absolute/path/to/usage.db:/migration/usage.db:ro \
  cli-proxy-api \
  /usr/local/bin/migrate-sqlite-to-postgres.sh /migration/usage.db
```

脚本默认依次执行 SQLite 只读 inventory、PostgreSQL 导入 dry-run 和实际 apply。若只想检查而不写入 PostgreSQL，增加 `-e CLIRELAY_SQLITE_AUTO_IMPORT=false`。也可以分步使用二进制命令：

```bash
./cli-proxy-api -sqlite-dry-run /path/to/usage.db

CLIRELAY_POSTGRES_DSN='postgres://user:pass@127.0.0.1:5432/cliproxy?sslmode=disable' \
./cli-proxy-api -sqlite-import /path/to/usage.db

CLIRELAY_POSTGRES_DSN='postgres://user:pass@127.0.0.1:5432/cliproxy?sslmode=disable' \
./cli-proxy-api -sqlite-import /path/to/usage.db -sqlite-import-dry-run=false
```

如果旧 Docker 部署仍是 SQLite-only compose，应先替换为仓库最新版 `docker-compose.yml` 并执行 `docker compose up -d postgres redis clirelay-updater`，再手工迁移数据。对于不支持 updater SSE 的旧 sidecar，需要额外执行一次 `docker compose up -d --force-recreate clirelay-updater`，之后的 OTA 才能使用新的实时进度与断线恢复协议。完整迁移边界见 [`docs/postgres-redis-migration.md`](docs/postgres-redis-migration.md)。

如果你的请求量较大，可以在 `config.yaml` 中调整 `request-log-storage`。全文请求/响应正文默认不保存；开启 `store-content` 后，正文会以压缩形式保留 30 天，并默认做约 1GB（1024MB）的总量上限，而轻量级请求元数据和请求详情仍可用于统计、筛选与排查。将 `content-retention-days: 0` 设为永久保留全文；在管理面板关闭正文存储时会同时清理已有输入与输出正文，但保留请求详情和请求记录；调整 `max-total-size-mb` 可让最老的全文在 retention 周期结束前提前裁剪。

如果你需要非本地磁盘的配置/认证持久化，服务端还支持通过环境变量启用 PostgreSQL、Git 和 S3 兼容对象存储后端。

### 3️⃣ 配置工具

将 AI 工具的 API 地址设为 `http://localhost:8317`，开始编码！

**示例：OpenAI Codex (`~/.codex/config.toml`)**
```toml
[model_providers.tabcode]
name = "openai"
base_url = "http://localhost:8317/v1"
requires_openai_auth = true
```

> 📖 **完整教程 →** [help.router-for.me](https://help.router-for.me/cn/)

## 🖥️ 管理面板

启用控制面板后，直接访问：

```bash
http://localhost:8317/manage
```

- `remote-management.disable-control-panel` 在示例配置里默认是 `false`，使用 Docker Compose 标准部署后即可访问控制面板。
- 开启后当前正式路由是 `/manage/login`，`management.html#/login` 仅保留给旧版兼容链路。
- Docker Compose 部署会在 `/manage` 暴露控制面板。
- 服务端既支持托管打包后的 SPA 目录，也支持在需要时自动拉取面板资源。
- 当前仓库只包含 `/manage` 的托管和同步链路，独立 Web 面板源码与 Go 服务端代码分仓维护。
- 面板的 UI/交互/文案 等改动请到面板源码仓库（默认 `kittors/codeProxy`）提交，并通过其发布产物（GitHub Release）供服务端拉取。
- 如果你偏向终端运维，也可以使用 `docker compose exec cli-proxy-api ./cli-proxy-api -tui`。
- 如果你希望自定义面板资源来源，可设置 `remote-management.panel-github-repository`。

## 📐 项目结构

```text
CliRelay/
├── cmd/server/               # 二进制入口和 CLI 模式分发
├── internal/api/             # HTTP 服务、管理路由、中间件
├── internal/auth/            # Provider 的 OAuth / Cookie / 浏览器认证流程
├── internal/config/          # 配置解析、默认值、迁移
├── internal/store/           # 本地、Git、PostgreSQL、对象存储配置/认证持久化
├── internal/tui/             # 终端管理 UI
├── internal/usage/           # PostgreSQL 支撑的用量数据、保留策略、分析聚合
├── internal/managementasset/ # /manage 面板托管与资源同步
├── sdk/                      # 可复用 Go SDK、handlers、executors
├── auths/                    # 本地凭据存储
├── examples/                 # SDK / 自定义 provider 示例
├── docs/                     # 本地文档与面板截图
└── docker-compose.yml        # 容器部署入口
```

## 📚 文档

| 文档 | 说明 |
|:-----|:-----|
| [新手入门](https://help.router-for.me/cn/) | 完整的安装与配置指南 |
| [管理 API](https://help.router-for.me/management/api) | 管理端点 REST API 参考 |
| [Amp CLI 指南](https://help.router-for.me/agent-client/amp-cli.html) | 集成 Amp CLI 和 IDE 扩展 |
| [SDK 使用](docs/sdk-usage.md) | 在 Go 应用中嵌入代理 |
| [SDK 进阶](docs/sdk-advanced.md) | 执行器与翻译器深入解析 |
| [SDK 认证](docs/sdk-access.md) | SDK 认证上下文 |
| [SDK Watcher](docs/sdk-watcher.md) | 凭据加载与热重载 |
| [PostgreSQL / Redis 迁移](docs/postgres-redis-migration.md) | 运行时数据栈配置、SQLite dry-run 清单与验证命令 |

## 🤝 贡献

欢迎贡献！以下是参与方式：

```bash
# 1. 克隆代码仓库
git clone https://github.com/kittors/CliRelay.git
cd CliRelay

# 2. 基于最新 dev 创建功能分支
git fetch origin
git switch -c feature/amazing-feature origin/dev

# 3. 提交更改
git commit -m "feat: add amazing feature"

# 4. 推送到你的分支，并提交目标为 dev 的 PR
git push origin feature/amazing-feature
```

请将 Pull Request 的目标分支设为 `dev`，不要直接提交到 `main`。维护者会先把验证通过的改动合并到 `dev`；`main` 只用于后续发布/稳定集成。完整分支与合并流程见 [CONTRIBUTING.md](CONTRIBUTING.md)。

## 📜 许可证

本项目采用 **MIT 许可证** — 详见 [LICENSE](LICENSE) 文件。

---

## 🙏 特别鸣谢

本项目是基于优秀的开源项目 **[router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)** 核心逻辑深度开发而来。
在此，我们想要对原上游项目 **CLIProxyAPI** 以及全体贡献者表达最诚挚的感谢！

正是由于上游构建的坚实且极具创新的代理分发底座，我们才能站在巨人的肩膀上，衍生出独特的高级管理功能（如 API Key 追踪管控、完整请求日志、实时系统监控），并完全重构了前端管理面板。

饮水思源，向开源精神致敬！❤️
