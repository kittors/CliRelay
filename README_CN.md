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

## ⚡ CliRelay 是什么？

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

## ✨ 核心特性

| 特性 | 说明 |
|:-----|:-----|
| 🔌 **多服务商** | OpenAI、Gemini、Claude、Codex、Qwen、iFlow、Vertex 及任何 OpenAI 兼容上游 |
| 🔑 **OAuth & API Key** | 支持浏览器 OAuth 登录和 API Key 方式，两者可同时使用 |
| ⚖️ **负载均衡** | 多账户轮询（round-robin）/ 填充优先（fill-first） |
| 🔄 **自动故障转移** | 配额用完时自动切换项目/模型 |
| 🖥️ **管理面板** | 内置 Web UI 监控、配置和用量统计 — [codeProxy](https://github.com/kittors/codeProxy) |
| 🧩 **Go SDK** | 将代理嵌入到你自己的 Go 应用中 |
| 🛡️ **安全** | API Key 鉴权、TLS、本地管理、请求伪装 |
| 🎯 **模型映射** | 自动将不可用模型路由到替代方案 |
| 🌊 **流式输出** | 完整的 SSE 流式和非流式响应，支持 Keep-Alive |
| 🧠 **多模态** | 支持文本 + 图片输入，函数调用 / 工具 |

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

### 3️⃣ 配置工具

将 AI 工具的 API 地址设为 `http://localhost:8317`，开始编码！

> 📖 **完整教程 →** [help.router-for.me](https://help.router-for.me/cn/)

## 🖥️ 管理面板

**[codeProxy](https://github.com/kittors/codeProxy)** 前端为 CliRelay 提供了现代化的管理后台：

- 📊 实时用量监控与统计
- ⚙️ 可视化配置编辑
- 🔐 OAuth 服务商管理
- 📋 结构化日志查看

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

## 🌍 生态系统

基于 CliRelay 构建的项目：

| 项目 | 平台 | 说明 |
|:-----|:-----|:-----|
| [vibeproxy](https://github.com/automazeio/vibeproxy) | macOS | 菜单栏应用，用 Claude Code & ChatGPT 订阅 |
| [Subtitle Translator](https://github.com/VjayC/SRT-Subtitle-Translator-Validator) | Web | Gemini 驱动的 SRT 字幕翻译器 |
| [CCS](https://github.com/kaitranntt/ccs) | CLI | 多 Claude 账户即时切换 |
| [ProxyPal](https://github.com/heyhuynhgiabuu/proxypal) | macOS | GUI 管理服务商和端点 |
| [Quotio](https://github.com/nguyenphutrong/quotio) | macOS | 统一订阅管理，实时配额追踪 |
| [CodMate](https://github.com/loocor/CodMate) | macOS | SwiftUI CLI AI 会话管理器 |
| [ProxyPilot](https://github.com/Finesssee/ProxyPilot) | Windows | Windows 原生版，TUI + 系统托盘 |
| [Claude Proxy VSCode](https://github.com/uzhao/claude-proxy-vscode) | VSCode | 快速模型切换，内置后端 |
| [ZeroLimit](https://github.com/0xtbug/zero-limit) | Windows | Tauri + React 配额监控面板 |
| [CPA-XXX Panel](https://github.com/ferretgeek/CPA-X) | Web | 管理面板，健康检查与请求统计 |
| [CLIProxyAPI Tray](https://github.com/kitephp/CLIProxyAPI_Tray) | Windows | PowerShell 托盘应用，自动更新 |
| [霖君 (LinJun)](https://github.com/wangdabaoqq/LinJun) | 跨平台 | AI 编程助手管理桌面应用 |
| [CLIProxyAPI Dashboard](https://github.com/itsmylife44/cliproxyapi-dashboard) | Web | Next.js 仪表盘，实时日志与配置同步 |

**受 CliRelay 启发的项目：**

| 项目 | 说明 |
|:-----|:-----|
| [9Router](https://github.com/decolua/9router) | Next.js 实现，组合系统与自动回退 |
| [OmniRoute](https://github.com/diegosouzapw/OmniRoute) | AI 网关，智能路由、缓存与可观测性 |

> [!NOTE]
> 基于 CliRelay 开发了项目？欢迎提交 PR 添加到这里！

## 🤝 贡献

欢迎贡献！以下是参与方式：

```bash
# 1. Fork 并克隆
git clone https://github.com/<your-username>/CliRelay.git

# 2. 创建功能分支
git checkout -b feature/amazing-feature

# 3. 提交更改
git commit -m "feat: add amazing feature"

# 4. 推送并提交 PR
git push origin feature/amazing-feature
```

## 📜 许可证

本项目采用 **MIT 许可证** — 详见 [LICENSE](LICENSE) 文件。

---

<p align="center">
  <sub>由 CliRelay 社区用 ❤️ 打造</sub>
</p>

## 写给所有中国网友的

QQ 群：188637136

或

Telegram 群：https://t.me/CLIProxyAPI
