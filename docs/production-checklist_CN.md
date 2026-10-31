# 团队网关部署检查清单

向团队开放 CliRelay 前，请按实际部署检查以下项目。协议兼容是针对提供商、模型和客户端组合验证过的能力子集，不代表所有 OpenAI、Claude 或 Gemini 功能完全相同。

## 访问与凭据

- 将上游提供商凭据与下游网关 API Key 分开管理。每个人或集成使用独立网关 Key，仅授予所需模型、渠道、租户权限与额度。
- 保持 `allow-unauthenticated: false`。公开地址前，先确认不带 Key 的客户端 API 请求会被拒绝。
- 安装时生成的密钥也是真实凭据。对于曾被共享、写入示例或泄露的 Key，应新建替代 Key、更新并验证客户端，再撤销旧 Key 并确认旧值已被拒绝；怀疑泄露时应立即撤销。
- 仅公开实际需要的客户端路径，例如 `/v1/*` 或 `/v1beta/*`。将 `/manage`、`/management.html` 和 `/v0/management/*` 限制在 VPN、内网或反向代理 IP 白名单内。看到登录页不等于后端管理 API 已受到保护。
- 从外部连接不带凭据请求敏感管理端点，确认不能读到配置或认证文件。网络限制之外，后端仍须校验管理权限。
- `trusted-proxies` 只填写自己管理的代理，并由代理覆盖客户端伪造的转发头。信任代理不等于允许远程管理，`remote-management.allow-remote` 是独立策略。TCP 转发在 HTTP 层可能与本机访问无法区分，仍须保留网络边界。

## 兼容性与日志

- 上线前使用真实客户端和模型验证非流式、流式、工具调用及结果、实际使用的 namespace/MCP 工具、思考参数、提供商专用请求头与模型别名，同时验证取消、限流及故障转移。
- 请求元数据可能包含 Key、账号、模型、路由、状态、耗时和用量等信息，应限制读取权限，并纳入备份与保留策略。
- `request-log-storage.store-content` 控制数据库是否保存完整请求与响应体，默认 `false`。`content-retention-days` 和 `max-total-size-mb` 分别限制正文保留时间与体积，设置为零会关闭对应限制；轻量元数据的保留行为独立管理。
- 文件请求日志是另一条存储路径。发送私有代码或个人数据前，同时核对文件日志、数据库正文与错误日志配置。不要在公开 Issue 中粘贴配置导出、认证文件、原始正文或含凭据的日志。
- 安全备份 PostgreSQL、配置和凭据，升级前验证恢复流程；记录部署的后端和面板版本，以便复现回归。

## 慢盘与 Synology NAS

应用以 `10001:10001` 运行。确保该用户能访问 `auths`、`logs`、`data` 挂载目录并读取 `config.yaml`；面板保存配置还需要写权限。Synology/DSM ACL 可能阻止 entrypoint 修复属主，应在宿主机针对实际挂载路径调整 ACL 和属主，不要向所有用户开放权限。

PostgreSQL 运行时迁移支持 `CLIRELAY_MIGRATION_TIMEOUT`，接受 `120s`、`5m` 等正数 Go duration，默认 `30s`。变量必须传入应用进程。Compose 可以增加以下覆盖文件，重启前检查最终配置：

```yaml
# compose.override.yaml
services:
  cli-proxy-api:
    environment:
      CLIRELAY_MIGRATION_TIMEOUT: "5m"
```

该设置只控制运行时迁移，不涵盖所有启动操作或配置存储引导。若仍然超时，应先根据脱敏错误确认失败阶段，不要盲目增加其他超时。

遇到 dirty migration 时，先备份，再对照所安装版本的准确迁移 SQL 和 checksum 检查实际 schema。不要只清除 `dirty`、盲目重放 SQL、修改 checksum 或删除数据卷，因为超时可能发生在 SQL 提交之前或之后。保持 PostgreSQL 的 `fsync`、`synchronous_commit` 和 `full_page_writes` 开启，关闭持久性保护不是推荐的慢盘修复方式。

`CLIRELAY_ADMIN_PASSWORD` 用于空数据库首次创建管理员，标准安装流程会生成有效密码；修改该变量不会轮换已有管理员密码。updater sidecar 会禁用应用镜像继承的 healthcheck，避免探测该容器内不存在的应用端口，也不能用其他容器的健康状态证明 updater 自身健康。

## 敏感词守卫开关

敏感词守卫在选择账号之前，按平台、租户、API Key 三层词表检查每个请求。读取词表时只等很短的时间（75 毫秒）：数据库超时未答，请求就沿用守卫上次见过的词表继续，从未见过则不做检查。若守卫本身出现故障，给应用进程设置 `CLIRELAY_WORD_GUARD=off` 并重启即可绕过：在去掉该变量之前，不检查任何请求，也不过滤任何回复。其他取值（包括写错）都会保持守卫开启。启动时会记一条日志（`word guard is switched off by CLIRELAY_WORD_GUARD`）。
