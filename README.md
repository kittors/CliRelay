<p align="center">
  <img src="https://img.shields.io/badge/Go-1.26+-00ADD8?style=for-the-badge&logo=go&logoColor=white" alt="Go">
  <img src="https://img.shields.io/badge/License-MIT-22c55e?style=for-the-badge" alt="License">
  <img src="https://img.shields.io/github/stars/kittors/CliRelay?style=for-the-badge&color=f59e0b" alt="Stars">
  <img src="https://img.shields.io/github/forks/kittors/CliRelay?style=for-the-badge&color=8b5cf6" alt="Forks">
</p>

<h1 align="center">🔀 CliRelay</h1>

<p align="center">
  <strong>A unified proxy server for AI CLI tools — use your <em>existing</em> subscriptions with any OpenAI / Gemini / Claude / Codex compatible client.</strong>
</p>

<p align="center">
  English | <a href="README_CN.md">中文</a>
</p>

<p align="center">
  <a href="#fork-improvements-over-upstream">🚀 Fork Improvements</a> ·
  <a href="https://help.router-for.me/">📖 Docs</a> ·
  <a href="https://github.com/kittors/codeProxy">🖥️ Management Panel</a> ·
  <a href="https://github.com/kittors/CliRelay/issues">🐛 Report Bug</a> ·
  <a href="https://github.com/kittors/CliRelay/pulls">✨ Request Feature</a>
</p>

---

## 🚀 Fork Improvements Over Upstream

This repository is the enhanced `zuohuadong/CliRelay` fork of upstream [`kittors/CliRelay`](https://github.com/kittors/CliRelay). The fork focuses on production Codex stability, large-context handling, multimodal routing, image workflows, management reliability, and automated upstream sync.

Quick links:

- [Context retrieval and smart compression](#context-retrieval-and-smart-compression)
- [Multimodal adapters](#multimodal-adapters)
- [Request policies and provider preferences](#request-policies-and-provider-preferences)
- [Auth and credential lifecycle](#auth-and-credential-lifecycle)
- [Codex WebSocket and session stability](#codex-websocket-and-session-stability)
- [Image generation and image edits](#image-generation-and-image-edits)
- [OpenAI-compatible executor improvements](#openai-compatible-executor-improvements)
- [Configuration and runtime operations](#configuration-and-runtime-operations)
- [CI/CD and upstream synchronization](#cicd-and-upstream-synchronization)

<a id="context-retrieval-and-smart-compression"></a>
### 🧠 Context Retrieval and Smart Compression

Adds `internal/contextretrieval`, a configurable local reducer for oversized OpenAI / Responses payloads. It keeps system and recent turns, retrieves older relevant content with SQLite FTS-style matching, preserves Codex tool-call pairs atomically, inserts Codex-aware summaries, and falls back to a secondary truncation pass when a retained item is still too large.

<a id="multimodal-adapters"></a>
### 🔌 Multimodal Adapters

Adds `internal/multimodaladapter`, a route-scoped media-to-text preprocessing layer for text-only upstreams. It can extract visual context through HTTP, ZAI Vision HTTP, or MCP tools; then strip, reject, pass through, or inject the extracted visual context into OpenAI / Responses requests without affecting native multimodal channels.

<a id="request-policies-and-provider-preferences"></a>
### 🛡️ Request Policies and Provider Preferences

Adds generic `request-policies` and `provider-preferences` config. Policies can skip a channel or reject locally when requests exceed provider-specific limits such as payload size, tools, or media features. Provider preferences let a requested model prefer a specific upstream provider/model first while keeping normal fallback behavior.

<a id="auth-and-credential-lifecycle"></a>
### 🔐 Auth and Credential Lifecycle

Strengthens channel selection around revoked credentials, token refresh, transient network failures, quota recovery, weighted rotation, and config hot-apply behavior. Revoked auth entries can be blocked or removed, unauthorized suspensions can recover after refresh, and network timeouts are treated as transient instead of permanently restricting credentials.

<a id="codex-websocket-and-session-stability"></a>
### 🌐 Codex WebSocket and Session Stability

Hardens Codex Responses WebSocket handling for warmup sessions, reconnects, idle upstreams, close ordering, read-loop panics, incremental input compatibility, request/response tracing, and output assembly. Non-streaming Responses output can be recovered from `response.output_item.done` events when the final completed event is incomplete.

<a id="image-generation-and-image-edits"></a>
### 🖼️ Image Generation and Image Edits

Improves `/v1/images/generations` and `/v1/images/edits` routing across Codex and OpenAI-compatible providers. Native edits can pass through, while providers without native edit semantics can be configured to use image-generations or chat-multimodal conversion, including configurable image field names for Qwen-style upstreams.

<a id="openai-compatible-executor-improvements"></a>
### 🧩 OpenAI-Compatible Executor Improvements

Extends the OpenAI-compatible executor with multimodal adaptation, image edit conversion, compact fallback, upstream error preservation, Kimi payload normalization, identity fingerprinting, response tracing, and safer stream parsing. This makes non-OpenAI upstreams behave more consistently for Codex and OpenAI-compatible clients.

<a id="configuration-and-runtime-operations"></a>
### ⚙️ Configuration and Runtime Operations

Adds config surface for context retrieval, multimodal adapters, request policies, provider preferences, OpenAI-compatible image edit modes, GPT-5.4 / GPT-5.5 1M-context registry updates, management auth-rate tuning, runtime settings persistence, and safer updater behavior in bind-mounted or containerized deployments.

<a id="cicd-and-upstream-synchronization"></a>
### 🔄 CI/CD and Upstream Synchronization

Adds an upstream sync workflow for `kittors/CliRelay`, Docker publishing adjustments, Node 24 action updates, and conflict-aware PR creation. The fork is designed to keep local production patches reviewable while still regularly absorbing upstream `dev`.

## ⚡ What is CliRelay?

> **✨ Heavily enhanced fork of the [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) project** — rebuilt with a production-grade management layer, web control panel hosting, and a terminal TUI for day-2 operations.

CliRelay turns AI CLI subscriptions, OAuth credentials, API keys, and compatible upstream services into one managed API layer. It proxies Claude Code, Gemini CLI, OpenAI Codex, Amp CLI, OpenAI-compatible clients, and other AI coding tools through a unified endpoint, then adds routing groups, failover, request logging, quota control, model pricing, image-generation support, API-key self-service, online updates, `/manage` web hosting, and terminal management workflows around that traffic.

```
┌───────────────────────┐         ┌──────────────┐         ┌────────────────────┐
│   AI Coding Tools     │         │              │         │  Upstream Providers │
│                       │         │              │ ──────▶ │  Google Gemini      │
│  Claude Code          │ ──────▶ │   CliRelay   │ ──────▶ │  OpenAI / Codex    │
│  Gemini CLI           │         │   :8317      │ ──────▶ │  Anthropic Claude  │
│  OpenAI Codex         │         │              │ ──────▶ │  Qwen / iFlow      │
│  Amp CLI / IDE        │         │              │ ──────▶ │  Antigravity       │
│  Any OAI-compatible   │         └──────────────┘         │  Vertex / OpenAI   │
└───────────────────────┘                                  │  iFlow / Qwen /    │
                                                           │  Kimi / Claude     │
                                                           └────────────────────┘
```

## ✨ Key Features

### 🔌 Multi-Provider Proxy Engine

| Feature | Description |
|:--------|:------------|
| 🌐 **Unified Endpoint** | One `http://localhost:8317` fronts Gemini, Claude, Codex, Qwen, iFlow, Antigravity, Vertex-compatible endpoints, OpenAI-compatible upstreams, and Amp integration |
| ⚖️ **Smart Load Balancing** | Round-robin or fill-first scheduling across multiple API keys for the same provider |
| 🧭 **Group & Path Routing** | Bind channels into groups, restrict API keys to allowed groups, and expose custom path namespaces for teams or workloads |
| 🔄 **Auto Failover** | Automatically switches to backup channels when quotas are exhausted or errors occur |
| 🧠 **Multimodal Support** | Full support for text + image inputs, image-generation routing, function calling (tools), and streaming SSE responses |
| 🔗 **OpenAI-Compatible** | Works with any upstream that speaks the OpenAI Chat Completions protocol |

### 📊 Request Logging & Monitoring (SQLite)

| Feature | Description |
|:--------|:------------|
| 📝 **Full Request Capture** | Every API request is logged to SQLite with timestamp, model, tokens (in/out/reasoning/cache), latency, status, and source channel |
| 💬 **Message Body Storage** | Full request/response message content captured in compressed SQLite storage, with separate retention for content vs. metadata |
| 🔍 **Advanced Querying** | Filter logs by API Key, model, status, time range with efficient pagination (LIMIT/OFFSET) |
| 📈 **Analytics Aggregation** | Pre-computed dashboards: daily trends, model distribution, hourly heatmaps, per-key statistics |
| 🏥 **Health Score Engine** | Real-time 0–100 health score considering success rate, latency, active channels, and error patterns |
| 📡 **WebSocket Monitoring** | Live system stats streamed via WebSocket: CPU, memory, goroutines, network I/O, DB size |
| 🗄️ **No-CGO SQLite** | Uses `modernc.org/sqlite` — pure Go, no CGO dependency, easy cross-compilation |

### 🔐 API Key & Access Management

| Feature | Description |
|:--------|:------------|
| 🔑 **API Key CRUD** | Create, edit, delete API keys via Management API — each with custom name, notes, and independent enable/disable toggle |
| 📊 **Per-Key Quotas** | Set max token / request quotas per key with automatic enforcement |
| ⏱️ **Rate Limiting** | Per-key rate limiting (requests per minute/hour) |
| 👥 **Team Permissions** | Assign API keys to different users or groups with scoped channel access and model permissions |
| 🔒 **Key Masking** | API keys are always displayed masked (`sk-***xxx`) in UI and logs |
| 🌍 **Public Lookup Page** | End users can query their own usage stats and request logs via a public self-service page (no login required) |

### 🔗 Provider Channel Management

| Feature | Description |
|:--------|:------------|
| 📋 **Multi-Tab Config** | Manage channels organized by provider type: Gemini, Claude, Codex, Vertex, OpenAI Compatible, Ampcode |
| 🏷️ **Channel Naming** | Each channel can have a custom name, notes, proxy URL, custom headers, and model alias mappings |
| 🧩 **Reusable Proxy Pool** | Maintain outbound proxy entries once and attach them to OAuth/auth channels when needed |
| ⏱️ **Latency Tracking** | Average latency (`latency_ms`) tracked per channel with visual indicators |
| 🔄 **Enable/Disable** | Individually toggle channels on/off without deletion |
| 🚫 **Model Exclusions** | Exclude specific models from a channel (e.g., block expensive models on backup keys) |
| 🧾 **Model Library Sync** | Maintain custom models and sync model IDs/pricing from OpenRouter for quota accounting |
| 📊 **Channel Stats** | Per-channel success/fail counts and model availability displayed on each channel card |

### 🛡️ Security & Authentication

| Feature | Description |
|:--------|:------------|
| 🔐 **OAuth Support** | Native OAuth flows for Gemini, Claude, Codex, Qwen, iFlow, Antigravity, and Kimi, plus device/browser/cookie variants where supported |
| 🪪 **Identity Fingerprints** | Centralize upstream identity metadata so providers receive consistent client fingerprints |
| 🔒 **TLS Handling** | Configurable TLS settings for upstream communication |
| 🏠 **Panel Isolation** | Management panel access controlled independently with admin password |
| 🛡️ **Request Cloaking** | Upstream requests are stripped of client-identifying headers for privacy |

### 🛠️ Operator Experience

| Feature | Description |
|:--------|:------------|
| 🖥️ **Visual Management Panel** | Configure providers, auth, API keys, models, routing, logs, updates, and system status from `/manage` |
| 🌐 **Chinese / English UI** | Built-in i18n for the management panel and Compose/TUI language selection |
| 🌙 **Dark Mode** | Full dark theme for long-running operational sessions |
| 🧬 **Visual Config Editor** | Edit runtime config visually or inspect source YAML when you need exact control |
| 🔄 **Online Update Flow** | Check versions, review update notes, trigger the updater sidecar, and wait for backend recovery from the panel |
| 📥 **CC Switch Import** | Import cc-switch style configuration into the managed model/channel workspace |

### 🗄️ Data Persistence

| Feature | Description |
|:--------|:------------|
| 💾 **SQLite Storage** | All usage data, request logs, and message bodies stored in local SQLite database |
| 🔄 **Redis Backup** | Optional Redis integration for periodic snapshotting and cross-restart metric preservation |
| 🗃️ **Pluggable Auth/Config Backends** | Local files by default, with optional PostgreSQL, Git, or S3-compatible object storage backends for config/auth persistence |
| 📦 **Config Snapshots** | Import/export entire system configuration as JSON for backup and migration |

## 📸 Management Panel Preview

CliRelay can expose a built-in web control panel at `/manage`. The server can host bundled SPA assets or fall back to synced management assets from the configured panel repository.

The gallery below uses the latest supplied screenshots, covering the current end-to-end management workflow.

### Dashboard, Locale & Theme

| Home overview | Operations overview |
| :------------ | :------------------ |
| <img src="docs/images/readme-showcase/home-overview-1.png" width="100%" alt="CliRelay dashboard overview" /> | <img src="docs/images/readme-showcase/home-overview-2.png" width="100%" alt="CliRelay operations dashboard" /> |

| Chinese / English interface | Dark mode |
| :-------------------------- | :-------- |
| <img src="docs/images/readme-showcase/home-i18n.png" width="100%" alt="Chinese and English management panel locale" /> | <img src="docs/images/readme-showcase/dark-mode.png" width="100%" alt="Management panel dark mode" /> |

### Monitoring, Logs & Self-Service

| Monitor center | Request logs |
| :------------- | :----------- |
| <img src="docs/images/readme-showcase/monitor-center.png" width="100%" alt="Monitor center charts and request metrics" /> | <img src="docs/images/readme-showcase/request-logs.png" width="100%" alt="Request log table with filters" /> |

| Request details | Log query system |
| :-------------- | :--------------- |
| <img src="docs/images/readme-showcase/request-details.png" width="100%" alt="Request details viewer" /> | <img src="docs/images/readme-showcase/log-query-system.png" width="100%" alt="Log query system" /> |

| API key lookup |
| :------------- |
| <img src="docs/images/readme-showcase/api-key-lookup.png" width="100%" alt="Public API key lookup page" /> |

### Auth, Identity & Access

| Unified OAuth management | Identity fingerprints |
| :----------------------- | :-------------------- |
| <img src="docs/images/readme-showcase/oauth-management.png" width="100%" alt="Unified OAuth management" /> | <img src="docs/images/readme-showcase/identity-fingerprint-management.png" width="100%" alt="Identity fingerprint management" /> |

| Team permissions | OAuth proxy assignment |
| :--------------- | :--------------------- |
| <img src="docs/images/readme-showcase/team-permissions.png" width="100%" alt="Team API key assignment and permissions" /> | <img src="docs/images/readme-showcase/proxy-config-for-oauth.png" width="100%" alt="Proxy configuration assigned to OAuth auth records" /> |

### Channels, Routing & Configuration

| Multi-channel API setup | Group routing and custom paths |
| :---------------------- | :----------------------------- |
| <img src="docs/images/readme-showcase/multi-channel-api-add.png" width="100%" alt="Add multiple API channels" /> | <img src="docs/images/readme-showcase/group-routing-custom-path.png" width="100%" alt="Channel group routing and custom path configuration" /> |

| Visual config | Upstream debug passthrough |
| :------------ | :------------------------- |
| <img src="docs/images/readme-showcase/visual-config.png" width="100%" alt="Visual configuration editor" /> | <img src="docs/images/readme-showcase/upstream-debug-passthrough.png" width="100%" alt="Debug passthrough content sent to upstream" /> |

| CC Switch import |
| :--------------- |
| <img src="docs/images/readme-showcase/cc-switch-import.png" width="100%" alt="Configurable CC Switch import" /> |

### Models, Image Generation & Updates

| OpenRouter model sync | Custom model maintenance |
| :-------------------- | :----------------------- |
| <img src="docs/images/readme-showcase/model-openrouter-sync.png" width="100%" alt="OpenRouter model ID and pricing sync" /> | <img src="docs/images/readme-showcase/custom-model-maintenance.png" width="100%" alt="Custom model maintenance" /> |

| Image generation config | Online update flow |
| :---------------------- | :----------------- |
| <img src="docs/images/readme-showcase/image-generation-config.png" width="100%" alt="Image generation configuration" /> | <img src="docs/images/readme-showcase/online-update.png" width="100%" alt="Online update mechanism" /> |

| System information |
| :----------------- |
| <img src="docs/images/readme-showcase/system-info.png" width="100%" alt="System information page" /> |

> 🔗 The runtime panel source is configurable via `remote-management.panel-github-repository`. The default repository is [kittors/codeProxy](https://github.com/kittors/codeProxy).

## 🏗️ Supported Providers

| Provider / Channel | Auth | Notes |
|:-------------------|:-----|:------|
| Google Gemini | OAuth + API Key | Gemini CLI / AI Studio style flows |
| Anthropic Claude | OAuth + API Key | Claude Code and Claude-compatible clients |
| OpenAI Codex | OAuth + API Key | Includes Responses and WebSocket bridging |
| Qwen | OAuth | Qwen Code style login flow |
| iFlow / GLM | OAuth + Cookie | Supports iFlow routing and related model families |
| Kimi | OAuth | Browser-based login flow |
| Antigravity | OAuth | Dedicated OAuth channel with model backfill support |
| Vertex-compatible endpoints | API Key | Custom base URL, headers, aliases, exclusions |
| OpenAI-compatible upstreams | API Key | OpenRouter, Grok-compatible endpoints, and custom providers |
| Amp integration | Upstream API key + mappings | Direct Amp upstream fallback or mapped local routing |

## 🚀 Quick Start

### 🐳 Install With Docker Compose

Docker Compose is the recommended installation path for CliRelay. The included `docker-compose.yml` uses the published `ghcr.io/kittors/clirelay:latest` image by default and starts both the API service and updater sidecar.

```bash
git clone https://github.com/kittors/CliRelay.git
cd CliRelay
cp config.example.yaml config.yaml
docker compose up -d
```

Edit `config.yaml` to add your API keys or OAuth credentials, then restart the service:

```bash
docker compose restart cli-proxy-api
```

By default, client API routes (`/v1`, `/v1beta`) require an API key. To run without client keys, set `allow-unauthenticated: true` in `config.yaml` (not recommended for production).

After startup:

- API endpoint: `http://localhost:8317`
- Web panel: `http://localhost:8317/manage`
- Logs: `docker compose logs -f cli-proxy-api`
- Restart: `docker compose restart cli-proxy-api`
- Stop: `docker compose down`
- TUI: `docker compose exec cli-proxy-api ./cli-proxy-api -tui`
- OAuth login modes: `docker compose exec cli-proxy-api ./cli-proxy-api -login`

Set `CLIRELAY_LOCALE=en` or `CLIRELAY_LOCALE=zh` in your Compose environment to control the default TUI language.

For cloud platforms that only allow one mounted directory, set `AUTH_PATH` to the authentication directory inside the container, for example `/CLIProxyAPI/auths`. `CLI_PROXY_AUTH_PATH` remains the host-side bind path, while `AUTH_PATH` is also used to override `auth-dir` at runtime.

To disable automatic update prompts, set the following in `config.yaml` or turn off **Automatic Update Checks** in the Config page:

```yaml
auto-update:
  enabled: false
```

Update checks follow the stable `main` Docker image by default. To test dev builds, set `channel: dev` in `config.yaml` or choose **Development (dev)** from **Update Channel** in the Config page:

```yaml
auto-update:
  channel: dev
```

### 🗄️ Enabling Data Persistence

By default, API usage logs are stored in SQLite for persistence. For additional backup:
1. Ensure you have a Redis server running.
2. Edit `config.yaml` and set `redis.enable: true` with your Redis address.
CliRelay will automatically snapshot and restore traffic metrics on every startup!

For large installations, tune `request-log-storage` in `config.yaml` to control how full request/response bodies are retained. By default, full bodies are compressed, kept for 30 days, and capped at ~1GB (1024MB); lightweight request metadata remains queryable for longer-term statistics. Set `content-retention-days: 0` to keep full content indefinitely, set `store-content: false` to stop new body storage without deleting existing historical content, and adjust `max-total-size-mb` to cap body storage so the oldest full bodies are pruned before the retention window is reached.

If you need non-local config/auth persistence, the server also supports PostgreSQL, Git-backed, and S3-compatible object-store backends through environment-based bootstrap settings.

### 3️⃣ Point Your Tools

Set your AI tool's API base to `http://localhost:8317` and start coding!

**Example: OpenAI Codex (`~/.codex/config.toml`)**
```toml
[model_providers.tabcode]
name = "openai"
base_url = "http://localhost:8317/v1"
requires_openai_auth = true
```

> 📖 **Full setup guides →** [help.router-for.me](https://help.router-for.me/)

## 🖥️ Management Panel

When the control panel is enabled, open:

```bash
http://localhost:8317/manage
```

- `remote-management.disable-control-panel` defaults to `false` in the example config, so the control panel is reachable after a standard Docker Compose deployment.
- When enabled, the current panel route is `/manage/login`. The old `management.html#/login` route is legacy-only.
- Docker Compose deployments expose the panel at `/manage`.
- The server can serve a bundled SPA directory or auto-fetch panel assets when needed.
- This repository contains the hosting/update path for `/manage`; the standalone web panel source is maintained separately from the Go server code.
- Make UI/interaction/copy changes in the panel source repository (default: `kittors/codeProxy`) and ship them via its release artifacts for the server to fetch.
- Terminal-first management is also available through `docker compose exec cli-proxy-api ./cli-proxy-api -tui`.
- If you want to customize the panel asset source, set `remote-management.panel-github-repository`.

## 📐 Architecture

```text
CliRelay/
├── cmd/server/               # Binary entry point and CLI mode dispatch
├── internal/api/             # HTTP server, management routes, middleware
├── internal/auth/            # Provider OAuth / cookie / browser auth flows
├── internal/config/          # Config parsing, defaults, migrations
├── internal/store/           # Local, Git, PostgreSQL, object-store auth/config persistence
├── internal/tui/             # Terminal management UI
├── internal/usage/           # SQLite usage DB, retention, analytics
├── internal/managementasset/ # /manage panel hosting and asset sync
├── sdk/                      # Reusable Go SDK, handlers, executors
├── auths/                    # Local credential storage
├── examples/                 # SDK / custom provider examples
├── docs/                     # Local docs and panel screenshots
└── docker-compose.yml        # Container deployment entry
```

## 📚 Documentation

| Doc | Description |
|:----|:------------|
| [Getting Started](https://help.router-for.me/) | Full installation and setup guide |
| [Management API](https://help.router-for.me/management/api) | REST API reference for management endpoints |
| [Amp CLI Guide](https://help.router-for.me/agent-client/amp-cli.html) | Integrate with Amp CLI & IDE extensions |
| [SDK Usage](docs/sdk-usage.md) | Embed the proxy in Go applications |
| [SDK Advanced](docs/sdk-advanced.md) | Executors & translators deep-dive |
| [SDK Access](docs/sdk-access.md) | Authentication in SDK context |
| [SDK Watcher](docs/sdk-watcher.md) | Credential loading & hot-reload |

## 🤝 Contributing

Contributions are welcome! Here's how to get started:

```bash
# 1. Clone the repository
git clone https://github.com/kittors/CliRelay.git
cd CliRelay

# 2. Create a feature branch from the latest dev baseline
git fetch origin
git switch -c feature/amazing-feature origin/dev

# 3. Make your changes & commit
git commit -m "feat: add amazing feature"

# 4. Push to your branch & open a PR targeting dev
git push origin feature/amazing-feature
```

Please target pull requests at `dev`, not `main`. Maintainers merge verified changes into `dev` first; `main` is updated separately for release/stable integration. See [CONTRIBUTING.md](CONTRIBUTING.md) for the full branch and merge workflow.

## 📜 License

This project is licensed under the **MIT License** — see the [LICENSE](LICENSE) file for details.

---

## 🙏 Acknowledgements & Special Thanks

This project is a deeply enhanced fork built upon the excellent core logic of the open-source **[router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)** project.
We want to express our deepest gratitude to the original **CLIProxyAPI** project and all its contributors!

It is thanks to the solid, innovative proxy distribution foundation built by the upstream that we were able to stand on the shoulders of giants. This allowed us to develop unique advanced management features (like API Key tracking & control, full request logging with SQLite, and real-time system monitoring) and rebuild an entirely new frontend dashboard from scratch.

A huge salute to the spirit of open source! ❤️
