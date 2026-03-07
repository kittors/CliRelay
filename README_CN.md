<p align="center">
  <img src="https://img.shields.io/badge/Go-1.21+-00ADD8?style=for-the-badge&logo=go&logoColor=white" alt="Go">
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

## ⚡ CliRelay (Enhanced Fork) 是什么？

> **✨ 这是一个经过大量功能增强的魔改版本！**
> 本项目并非原版上游，而是基于原核心逻辑进行的深度二次开发版本。我们不仅提升了后端的多渠道管理能力（支持 API Key 的 CRUD 管理、渠道分类备注、精准延迟统计 `latency_ms`、单服务启用/禁用控制、公开的用量查询接口 `/manage/` 等），还**完全从零重构了前端管理面板 [codeProxy](https://github.com/kittors/codeProxy)**——由 React 19 + Vite 7 + Tailwind CSS v4 打造的专业级后台，支持深色模式、精美的全数据监控面板和强大的导入导出配置快照管理。

CliRelay 让你可以将 AI 编程工具（Claude Code、Gemini CLI、OpenAI Codex、Amp CLI 等）的请求**统一代理**到一个本地端点。通过 OAuth 登录或添加 API 密钥即可使用，CliRelay 自动处理路由和负载均衡：

```
┌───────────────────────┐         ┌──────────────┐         ┌────────────────────┐
│   AI 编程工具          │         │              │         │  上游服务商          │
│                       │         │              │ ──────▶ │  Google Gemini      │
│  Claude Code          │ ──────▶ │   CliRelay   │ ──────▶ │  OpenAI / Codex    │
│  Gemini CLI           │         │   :8317      │ ──────▶ │  Anthropic Claude  │
│  OpenAI Codex         │         │              │ ──────▶ │  Qwen / iFlow      │
│  Amp CLI / IDE        │         │              │ ──────▶ │  OpenRouter / ...  │
│  其他 OAI 兼容工具     │         └──────────────┘         └────────────────────┘
└───────────────────────┘
```

## ✨ 魔改版核心增强特性

| 特性 | 说明 |
|:-----|:-----|
| 🔑 **增强的 API Key 管理** | 完整的 API Key CRUD 控制，支持独立备注命名，以及一键**启用/禁用**单条 Key |
| ⏱️ **精准监控与追踪** | 增加延迟追踪 (`latency_ms`)、详细用量统计、暴露公开的查询端点机制与 `/manage/` 路由 |
| 🖥️ **全新 React 19 面板** | **完全重构**的 [codeProxy](https://github.com/kittors/codeProxy) 前端：搭载可视化监控大屏、暗黑模式、KPI 指标与快照导入导出功能 |
| 🔌 **多服务商生态** | OpenAI、Gemini、Claude、Codex、Qwen、iFlow、Vertex 及任何 OpenAI 兼容上游 |
| ⚖️ **负载均衡与转移** | 智能调度的多账户轮询/填充优先，配额用尽时**自动故障转移**（Failover）至可用模型 |
| 🧩 **Go SDK 与流式输出** | 原生 Go SDK 支持嵌入代理；完整的 SSE 流式 / 非流式响应控制与 Keep-Alive 支持 |
| 🧠 **多模态与工具回调** | 无缝支持文本 + 图片文件解析识别，以及 AI Function Calling (工具调用) 能力 |
| 🛡️ **安全与防御隔离** | 基于 API Key 鉴权、TLS、管理后台本地化隔离与上游请求伪装替换策略 |
| 🗄️ **Redis 数据持久化** | 自动定时及在服务优雅停机时将 API 统计快照存入 Redis，确保服务重启或更新时前端面板监控数据 **零丢失** |

## 🚀 快速开始

### 1️⃣ 下载 & 配置

```bash
# 从 GitHub Releases 下载适合你平台的最新版本
# 然后复制示例配置文件
cp config.example.yaml config.yaml
```

编辑 `config.yaml` 添加你的 API 密钥或 OAuth 凭据。

### 2️⃣ 运行

```bash
./clirelay
# 服务启动在 http://localhost:8317
```

### 🐳 Docker 部署

```bash
docker compose up -d
```

### 🗄️ 开启持久化（防数据丢失）

默认情况下 API 调用统计数据存活于内存中，重启即焚。如果你希望在重启或发版后保留看板监控数据：
1. 准备一个可用的 Redis 数据库。
2. 编辑 `config.yaml` 找到最上方，将 `redis` 块下的 `enable` 改为 `true`，并填入地址。
配置完成后，无论怎么重新启动服务，它都将自动完成快照恢复！

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

**[codeProxy](https://github.com/kittors/codeProxy)** 前端为 CliRelay 提供了现代化的管理后台：

- 📊 实时用量监控与统计
- ⚙️ 可视化配置编辑
- 🔐 OAuth 服务商管理
- 📋 结构化日志查看

<details>
<summary>📸 前端面板截图</summary>

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
# 克隆并启动管理面板
git clone https://github.com/kittors/codeProxy.git
cd codeProxy
bun install
bun run dev
# 访问 http://localhost:5173
```

## 🏗️ 支持的服务商

<table>
<tr>
<td align="center"><strong>🟢 Google Gemini</strong><br/>OAuth + API Key</td>
<td align="center"><strong>🟣 Anthropic Claude</strong><br/>OAuth + API Key</td>
<td align="center"><strong>⚫ OpenAI Codex</strong><br/>OAuth</td>
</tr>
<tr>
<td align="center"><strong>🔵 通义千问 Qwen</strong><br/>OAuth</td>
<td align="center"><strong>🟡 iFlow (GLM)</strong><br/>OAuth</td>
<td align="center"><strong>🟠 Vertex AI</strong><br/>API Key</td>
</tr>
<tr>
<td align="center" colspan="3"><strong>🔗 任意 OpenAI 兼容上游</strong>（OpenRouter 等）</td>
</tr>
</table>

## 📐 项目结构

```
CliRelay/
├── cmd/              # 入口
├── internal/         # 核心代理逻辑、翻译器、处理器
├── sdk/              # 可复用的 Go SDK
├── auths/            # 身份验证流程
├── examples/         # 自定义 Provider 示例
├── docs/             # SDK 与 API 文档
├── config.yaml       # 运行时配置
└── docker-compose.yml
```

## 📚 文档

| 文档 | 说明 |
|:-----|:-----|
| [新手入门](https://help.router-for.me/cn/) | 完整的安装与配置指南 |
| [管理 API](https://help.router-for.me/cn/management/api) | 管理端点 REST API 参考 |
| [Amp CLI 指南](https://help.router-for.me/cn/agent-client/amp-cli.html) | 集成 Amp CLI 和 IDE 扩展 |
| [SDK 使用](docs/sdk-usage_CN.md) | 在 Go 应用中嵌入代理 |
| [SDK 进阶](docs/sdk-advanced_CN.md) | 执行器与翻译器深入解析 |
| [SDK 认证](docs/sdk-access_CN.md) | SDK 认证上下文 |
| [SDK Watcher](docs/sdk-watcher_CN.md) | 凭据加载与热重载 |

## 🤝 贡献

欢迎贡献！以下是参与方式：

```bash
# 1. 克隆代码仓库
git clone https://github.com/kittors/CliRelay.git

# 2. 创建功能分支
git checkout -b feature/amazing-feature

# 3. 提交更改
git commit -m "feat: add amazing feature"

# 4. 推送到你的分支并提交 PR
git push origin feature/amazing-feature
```

## 📜 许可证

本项目采用 **MIT 许可证** — 详见 [LICENSE](LICENSE) 文件。

---

## 🙏 特别鸣谢

本项目是基于优秀的开源项目 **[router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)** 核心逻辑深度二次开发（魔改）而来。
在此，我们想要对原上游项目 **CLIProxyAPI** 以及全体贡献者表达最诚挚的感谢！

正是由于上游构建的坚实且极具创新的代理分发底座，我们才能站在巨人的肩膀上，衍生出这些独特的高级（如 API Key 追踪管控）管理功能，并重构出全新的前端管理大屏。

饮水思源，向开源精神致敬！❤️

