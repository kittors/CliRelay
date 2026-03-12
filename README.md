<p align="center">
  <img src="https://img.shields.io/badge/Go-1.21+-00ADD8?style=for-the-badge&logo=go&logoColor=white" alt="Go">
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
  <a href="https://help.router-for.me/">📖 Docs</a> ·
  <a href="https://github.com/kittors/codeProxy">🖥️ Management Panel</a> ·
  <a href="https://github.com/kittors/CliRelay/issues">🐛 Report Bug</a> ·
  <a href="https://github.com/kittors/CliRelay/pulls">✨ Request Feature</a>
</p>

---

## ⚡ What is CliRelay?

> **✨ Heavily enhanced fork of the [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) project** — rebuilt with a production-grade management layer, enterprise-quality monitoring, and a full React-based admin dashboard.

CliRelay lets you **proxy requests** from AI coding tools (Claude Code, Gemini CLI, OpenAI Codex, Amp CLI, Kiro, etc.) through a single unified endpoint. Authenticate once with OAuth, add your API keys — or both — and CliRelay handles intelligent routing, load balancing, failover, and usage logging automatically.

```
┌───────────────────────┐         ┌──────────────┐         ┌────────────────────┐
│   AI Coding Tools     │         │              │         │  Upstream Providers │
│                       │         │              │ ──────▶ │  Google Gemini      │
│  Claude Code          │ ──────▶ │   CliRelay   │ ──────▶ │  OpenAI / Codex    │
│  Gemini CLI           │         │   :8317      │ ──────▶ │  Anthropic Claude  │
│  OpenAI Codex         │         │              │ ──────▶ │  Qwen / iFlow      │
│  Amp CLI / IDE        │         │              │ ──────▶ │  Kiro / Vertex     │
│  Kiro / Any OAI-compat│         └──────────────┘         └────────────────────┘
└───────────────────────┘
```

## ✨ Key Features

### 🔌 Multi-Provider Proxy Engine

| Feature | Description |
|:--------|:------------|
| 🌐 **Unified Endpoint** | One `http://localhost:8317` handles requests for all providers (Gemini, Claude, OpenAI, Codex, Qwen, iFlow, Vertex, Kiro, MiniMax, Grok, and more) |
| ⚖️ **Smart Load Balancing** | Round-robin or fill-first scheduling across multiple API keys for the same provider |
| 🔄 **Auto Failover** | Automatically switches to backup channels when quotas are exhausted or errors occur |
| 🧠 **Multimodal Support** | Full support for text + image inputs, function calling (tools), and streaming SSE responses |
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
| 🔒 **Key Masking** | API keys are always displayed masked (`sk-***xxx`) in UI and logs |
| 🌍 **Public Lookup Page** | End users can query their own usage stats and request logs via a public self-service page (no login required) |

### 🔗 Provider Channel Management

| Feature | Description |
|:--------|:------------|
| 📋 **Multi-Tab Config** | Manage channels organized by provider type: Gemini, Claude, Codex, Vertex, OpenAI Compatible, Ampcode |
| 🏷️ **Channel Naming** | Each channel can have a custom name, notes, proxy URL, custom headers, and model alias mappings |
| ⏱️ **Latency Tracking** | Average latency (`latency_ms`) tracked per channel with visual indicators |
| 🔄 **Enable/Disable** | Individually toggle channels on/off without deletion |
| 🚫 **Model Exclusions** | Exclude specific models from a channel (e.g., block expensive models on backup keys) |
| 📊 **Channel Stats** | Per-channel success/fail counts and model availability displayed on each channel card |

### 🛡️ Security & Authentication

| Feature | Description |
|:--------|:------------|
| 🔐 **OAuth Support** | Native OAuth flows for Gemini, Claude, Codex, Qwen, iFlow (service-account or browser-based) |
| 🔒 **TLS Handling** | Configurable TLS settings for upstream communication |
| 🏠 **Panel Isolation** | Management panel access controlled independently with admin password |
| 🛡️ **Request Cloaking** | Upstream requests are stripped of client-identifying headers for privacy |

### 🗄️ Data Persistence

| Feature | Description |
|:--------|:------------|
| 💾 **SQLite Storage** | All usage data, request logs, and message bodies stored in local SQLite database |
| 🔄 **Redis Backup** | Optional Redis integration for periodic snapshotting and cross-restart metric preservation |
| 📦 **Config Snapshots** | Import/export entire system configuration as JSON for backup and migration |

## 📸 Management Panel Preview

The **[codeProxy](https://github.com/kittors/codeProxy)** dashboard provides a stunning, modern web UI for managing your CliRelay instance:

<p align="center">
  <img src="docs/images/dashboard.png" width="100%" />
</p>
<p align="center"><em>Dashboard — KPI metrics, health score, real-time system monitoring, channel latency ranking</em></p>

<p align="center">
  <img src="docs/images/monitor.png" width="48%" />
  <img src="docs/images/providers.png" width="48%" />
</p>
<p align="center"><em>Monitor Center with charts & analysis | AI Provider channel management</em></p>

<p align="center">
  <img src="docs/images/request-logs.png" width="100%" />
</p>
<p align="center"><em>Request Logs — Virtual scrolling, multi-filter, token hover, error detail modal</em></p>

> 🔗 See the full [codeProxy README](https://github.com/kittors/codeProxy) for more screenshots and feature details.

## 🏗️ Supported Providers

<table>
<tr>
<td align="center"><strong>🟢 Google Gemini</strong><br/>OAuth + API Key</td>
<td align="center"><strong>🟣 Anthropic Claude</strong><br/>OAuth + API Key</td>
<td align="center"><strong>⚫ OpenAI Codex</strong><br/>OAuth</td>
</tr>
<tr>
<td align="center"><strong>🔵 Qwen Code</strong><br/>OAuth</td>
<td align="center"><strong>🟡 iFlow (GLM)</strong><br/>OAuth</td>
<td align="center"><strong>🟠 Vertex AI</strong><br/>API Key</td>
</tr>
<tr>
<td align="center"><strong>🔴 Kimi</strong><br/>API Key</td>
<td align="center"><strong>🟤 Kiro</strong><br/>API Key</td>
<td align="center"><strong>🟣 MiniMax</strong><br/>API Key</td>
</tr>
<tr>
<td align="center" colspan="3"><strong>🔗 Any OpenAI-compatible upstream</strong> (OpenRouter, Grok, etc.)</td>
</tr>
</table>

## 🚀 Quick Start

### 1️⃣ Download & Configure

```bash
# Download the latest release for your platform from GitHub Releases
# Then copy the example config
cp config.example.yaml config.yaml
```

Edit `config.yaml` to add your API keys or OAuth credentials.

### 2️⃣ Run

```bash
./clirelay
# Server starts at http://localhost:8317
```

### 🐳 Docker (Recommended)

**One-Click Deploy**:

- Linux `amd64` / `arm64`: supported for automatic Docker installation
- macOS `arm64` / `amd64`: supported when Docker Desktop / OrbStack / Colima is already installed and running

```bash
curl -fsSL https://raw.githubusercontent.com/kittors/CliRelay/main/install.sh | bash
```

The script will:

- automatically install Docker when needed
- detect the host architecture and pin the correct Docker platform for `amd64` / `arm64`
- let you choose **English** or **Chinese** during installation
- persist that language into the container so the built-in TUI starts in the selected language by default
- install a local `clirelay` helper command for day-2 operations

On macOS, the installer will **not** try to install Docker for you. It will reuse your existing Docker runtime and stop with a clear message if Docker Desktop / OrbStack / Colima is not running.

After installation, use:

```bash
clirelay status
clirelay update
clirelay restart
clirelay logs
clirelay tui
```

`clirelay update` keeps the existing configuration and only refreshes the image + containers, so ongoing upgrades stay simple.

> 💡 If `curl` is not installed, install it first:
> ```bash
> # Debian / Ubuntu
> apt-get update && apt-get install -y curl
>
> # CentOS / RHEL / Fedora
> yum install -y curl
> ```

Or deploy manually with Docker Compose:

```bash
docker compose up -d
```

For manual Docker deployments, you can also set `CLIRELAY_LOCALE=en` or `CLIRELAY_LOCALE=zh` in your Compose environment to control the default TUI language.

### 🗄️ Enabling Data Persistence

By default, API usage logs are stored in SQLite for persistence. For additional backup:
1. Ensure you have a Redis server running.
2. Edit `config.yaml` and set `redis.enable: true` with your Redis address.
CliRelay will automatically snapshot and restore traffic metrics on every startup!

For large installations, tune `request-log-storage` in `config.yaml` to control how long full request/response bodies are retained. By default, full content is compressed and kept for 30 days while lightweight request metadata remains queryable for longer-term statistics.

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

Install and run the **[codeProxy](https://github.com/kittors/codeProxy)** frontend:

```bash
git clone https://github.com/kittors/codeProxy.git
cd codeProxy
bun install
bun run dev
# Visit http://localhost:5173
```

## 📐 Architecture

```
CliRelay/
├── cmd/              # Entry point
├── internal/         # Core proxy logic, translators, handlers
│   ├── handler/      # HTTP request handlers (chat, models, management)
│   ├── translator/   # Provider-specific request/response translators
│   ├── scheduler/    # Load balancing & channel selection
│   ├── database/     # SQLite operations & migration
│   └── monitor/      # Health check & system stats
├── sdk/              # Reusable Go SDK
├── auths/            # OAuth authentication flows
├── examples/         # Custom provider examples
├── docs/             # SDK & API documentation
├── config.yaml       # Runtime configuration
└── docker-compose.yml
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

# 2. Create a feature branch
git checkout -b feature/amazing-feature

# 3. Make your changes & commit
git commit -m "feat: add amazing feature"

# 4. Push to your branch & open a PR
git push origin feature/amazing-feature
```

## 📜 License

This project is licensed under the **MIT License** — see the [LICENSE](LICENSE) file for details.

---

## 🙏 Acknowledgements & Special Thanks

This project is a deeply enhanced fork built upon the excellent core logic of the open-source **[router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)** project.
We want to express our deepest gratitude to the original **CLIProxyAPI** project and all its contributors!

It is thanks to the solid, innovative proxy distribution foundation built by the upstream that we were able to stand on the shoulders of giants. This allowed us to develop unique advanced management features (like API Key tracking & control, full request logging with SQLite, and real-time system monitoring) and rebuild an entirely new frontend dashboard from scratch.

A huge salute to the spirit of open source! ❤️
