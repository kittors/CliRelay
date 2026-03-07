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

## ⚡ What is CliRelay (Enhanced Fork)?

> **✨ This is a heavily modified and enhanced fork of the original project!**
> Built upon the original core logic, this fork introduces massive improvements. We added precise backend channel management (API Key CRUD, channel naming/notes, `latency_ms` tracking, independent enable/disable toggles for keys, and a public `/manage/` usage query endpoint) and **completely rebuilt the frontend dashboard [codeProxy](https://github.com/kittors/codeProxy) from scratch** using modern tools (React 19 + Vite 7 + Tailwind CSS v4, featuring dark mode, immersive data monitoring, KPI metrics, and configuration snapshot tools).

CliRelay lets you **proxy requests** from AI coding tools (Claude Code, Gemini CLI, OpenAI Codex, Amp CLI, etc.) through a single local endpoint. Authenticate once with OAuth, add your API keys — or both — and CliRelay handles the rest:

```
┌───────────────────────┐         ┌──────────────┐         ┌────────────────────┐
│   AI Coding Tools     │         │              │         │  Upstream Providers │
│                       │         │              │ ──────▶ │  Google Gemini      │
│  Claude Code          │ ──────▶ │   CliRelay   │ ──────▶ │  OpenAI / Codex    │
│  Gemini CLI           │         │   :8317      │ ──────▶ │  Anthropic Claude  │
│  OpenAI Codex         │         │              │ ──────▶ │  Qwen / iFlow      │
│  Amp CLI / IDE        │         │              │ ──────▶ │  OpenRouter / ...  │
│  Any OAI-compatible   │         └──────────────┘         └────────────────────┘
└───────────────────────┘
```

## ✨ Enhanced Fork Features

| Feature | Description |
|:--------|:------------|
| � **Advanced API Key Mgmt** | Full CRUD control for API Keys, with support for custom naming/notes and independent **enable/disable** toggles. |
| ⏱️ **Precision Tracking** | Added precise latency tracking (`latency_ms`), detailed usage stats, and public query endpoints over `/manage/` routes. |
| 🖥️ **All-New React 19 Panel** | A **completely rebuilt** dashboard [codeProxy](https://github.com/kittors/codeProxy): featuring ECharts monitoring, dark mode, KPI metrics, and config snapshot import/export. |
| � **Multi-Provider Environment** | Support for OpenAI, Gemini, Claude, Codex, Qwen, iFlow, Vertex, and any OpenAI-compatible upstream providers. |
| ⚖️ **Load Balancing & Failover** | Intelligent round-robin or fill-first scheduling, with **auto-failover** to backup projects when account quotas are fully consumed. |
| 🧩 **Go SDK & Streaming Hub** | Native Go SDK for embedding the proxy target; full SSE streaming and non-streaming response handling with Keep-Alive. |
| 🧠 **Multimodal & Tool Calling** | Seamless support for text + image multimodal inputs and comprehensive abstract Function Calling (Tools) capabilities. |
| 🛡️ **Security & Cloaking Shield** | Robust API Key authentication, TLS handling, strict localhost panel isolation, and precise upstream request cloaking. |
| 🗄️ **Redis Data Persistence** | Auto-snapshots API statistics to Redis periodically and on shutdown, guaranteeing **zero data loss** of usage charts across server restarts. |

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

### 🐳 Docker

```bash
docker compose up -d
```

### 🗄️ Enabling Data Persistence

By default, API usage logs and charts are stored in memory and reset on restart. To permanently save your data across deployments:
1. Ensure you have a Redis server running.
2. Edit `config.yaml` and set `redis.enable: true` with your Redis address.
CliRelay will automatically snap and restore traffic metrics on every startup!

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

The **[codeProxy](https://github.com/kittors/codeProxy)** frontend provides a modern management dashboard for CliRelay:

- 📊 Real-time usage monitoring & statistics
- ⚙️ Visual configuration editing
- 🔐 OAuth provider management
- 📋 Structured log viewer

<details>
<summary>📸 Dashboard Screenshots</summary>

<p align="center">
  <img src="assets/codeproxy/iShot_2026-03-06_10.51.59.png" width="48%" />
  <img src="assets/codeproxy/iShot_2026-03-06_10.52.09.png" width="48%" />
</p>
<p align="center">
  <img src="assets/codeproxy/iShot_2026-03-06_10.52.33.png" width="48%" />
  <img src="assets/codeproxy/iShot_2026-03-06_10.54.03.png" width="48%" />
</p>
</details>

```bash
# Clone and start the management panel
git clone https://github.com/kittors/codeProxy.git
cd codeProxy
bun install
bun run dev
# Visit http://localhost:5173
```

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
<td align="center" colspan="3"><strong>🔗 Any OpenAI-compatible upstream</strong> (OpenRouter, etc.)</td>
</tr>
</table>

## 📐 Architecture

```
CliRelay/
├── cmd/              # Entry point
├── internal/         # Core proxy logic, translators, handlers
├── sdk/              # Reusable Go SDK
├── auths/            # Authentication flows
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

It is thanks to the solid, innovative proxy distribution foundation built by the upstream that we were able to stand on the shoulders of giants. This allowed us to develop unique advanced management features (like API Key tracking & control) and rebuild an entirely new frontend dashboard from scratch.

A huge salute to the spirit of open source! ❤️

