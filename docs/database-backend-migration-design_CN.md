# 数据库后端与三库迁移设计文档

状态：设计草案
目标版本：后续功能分支实现
适用仓库：CliRelay 后端与管理面板
最后更新：2026-05-04

## 背景

CliRelay 当前把请求日志、请求/响应正文、API Key、模型配置、模型价格、OpenRouter 同步状态、路由配置、代理池、运行时设置和配额快照保存在本地 SQLite 数据库中。服务启动时会在配置文件所在目录下创建 `data/usage.db`，并由 `internal/usage` 包直接使用 SQLite 语法建表、查询、写入和维护。

仓库里已经存在 `internal/store/postgresstore.go`，它可以把 `config.yaml` 和 auth 元数据镜像到 PostgreSQL，但这套逻辑和 `internal/usage` 的 SQLite 数据库是两条链路。也就是说，现状不是“应用数据库可选 PostgreSQL”，而是“运行时数据固定 SQLite，配置/auth 可选 PostgreSQL 镜像”。

本设计要把数据库能力收敛成一个统一体验：用户部署时可以选择 SQLite、PostgreSQL 或 MySQL 之一作为运行数据库；已经运行的实例可以在系统信息页备份数据库内容，并在 SQLite、PostgreSQL、MySQL 之间迁移；迁移失败时不影响旧数据库，功能保持可用。

## 目标

1. SQLite 继续作为默认数据库，现有部署不改配置也能照常启动。
2. 新部署可以通过 `config.yaml` 或环境变量选择 `sqlite`、`postgresql`、`mysql`。
3. 系统信息页显示当前数据库类型、连接状态、版本、表数量、行数、数据大小、最近备份和迁移状态。
4. 系统信息页提供数据库备份入口，支持下载一个可验证的备份包。
5. 系统信息页提供数据库迁移向导，支持三种数据库任意方向迁移。
6. 迁移采用“复制到目标库、校验、确认切换”的流程；确认前不改当前运行数据库。
7. 迁移失败、连接失败或校验失败时，旧数据库保持 active，服务功能不受影响。
8. 所有 DB-backed 功能在三种数据库下表现一致，包括管理面板、API Key 限额、请求日志、模型配置、路由配置、代理池和运行时设置。
9. 提供系统化测试：单元测试、handler 测试、三库集成测试、迁移往返测试和失败回滚测试。

## 非目标

1. 不在本功能中自动部署、重启或替换生产服务器。
2. 不在迁移成功后自动删除源数据库。
3. 不做双写或实时跨库同步。
4. 不把数据库密码明文返回给前端或写入日志。
5. 不把文件型 OAuth token 强行塞进运行数据库；若需要完整系统备份，应作为单独的“配置/auth 备份”能力处理。
6. 不要求用户手写 SQL 或直接操作数据库表。

## 当前数据范围

第一阶段要覆盖 `internal/usage` 当前管理的数据库表：

| 数据 | 当前用途 |
| --- | --- |
| `request_logs` | 请求日志元数据、Token、延迟、状态、渠道、模型 |
| `request_log_content` | 压缩后的请求/响应正文与 detail 内容 |
| `auth_file_quota_snapshots` | 按天保留的 auth 配额快照 |
| `auth_file_quota_snapshot_points` | 更细粒度的配额时间序列 |
| `api_keys` | API Key、名称、限额、模型/渠道限制、system prompt |
| `model_pricing` | 模型价格 |
| `model_configs` | 自定义模型配置 |
| `model_owner_presets` | 模型 owner 预设 |
| `model_openrouter_sync_state` | OpenRouter 模型同步状态 |
| `routing_config` | 分组路由和路径路由配置 |
| `proxy_pool` | 可复用代理池 |
| `runtime_settings` | 运行时 provider keys、兼容配置、身份指纹等设置 |

备份包默认只覆盖这些数据库表。系统可在 UI 中提示：如果用户使用本地文件保存 OAuth/auth，需要另行备份 auth 目录，或在后续实现“完整系统备份包”时纳入。

## 配置设计

新增顶层配置 `database`：

```yaml
database:
  type: sqlite

  sqlite:
    path: ""

  postgresql:
    dsn: ""
    schema: ""
    max-open-conns: 10
    max-idle-conns: 5
    conn-max-lifetime-seconds: 3600

  mysql:
    dsn: ""
    max-open-conns: 10
    max-idle-conns: 5
    conn-max-lifetime-seconds: 3600

  migration:
    batch-size: 1000
    verify-samples: 50
    backup-dir: ""
```

兼容规则：

- `database` 缺失时等价于 `database.type: sqlite`。
- `database.sqlite.path` 为空时使用当前默认路径：`<config-dir>/data/usage.db`。
- `postgres` 可以作为 `postgresql` 的别名，便于用户输入。
- MySQL DSN 推荐包含 `parseTime=true` 和 `charset=utf8mb4`，系统在连接测试时给出提示。
- 管理 API 和日志只展示脱敏后的 DSN。
- `batch-size` 默认 `1000`，限制在 `100..10000`。
- `verify-samples` 默认 `50`，限制在 `0..1000`。
- `backup-dir` 为空时使用 `<config-dir>/data/backups`。

环境变量补充：

| 环境变量 | 说明 |
| --- | --- |
| `CLIRELAY_DATABASE_TYPE` | 覆盖数据库类型 |
| `CLIRELAY_SQLITE_PATH` | 覆盖 SQLite 路径 |
| `CLIRELAY_POSTGRES_DSN` | 覆盖 PostgreSQL DSN |
| `CLIRELAY_POSTGRES_SCHEMA` | 覆盖 PostgreSQL schema |
| `CLIRELAY_MYSQL_DSN` | 覆盖 MySQL DSN |

环境变量优先级高于 YAML，适合 Docker Compose、systemd 和云部署。系统信息页保存数据库设置时只更新 YAML 中可安全持久化的字段；如果当前字段来自环境变量，UI 需要标记“由环境变量控制”，并禁止覆盖。

## 部署体验

SQLite 默认部署：

```yaml
database:
  type: sqlite
```

PostgreSQL 部署：

```yaml
database:
  type: postgresql
  postgresql:
    dsn: "postgres://clirelay:<password>@postgres:5432/clirelay?sslmode=disable"
    schema: "public"
```

MySQL 部署：

```yaml
database:
  type: mysql
  mysql:
    dsn: "clirelay:<password>@tcp(mysql:3306)/clirelay?parseTime=true&charset=utf8mb4&collation=utf8mb4_unicode_ci"
```

Docker Compose 示例后续应在 `README.md`、`README_CN.md` 和 `config.example.yaml` 中补齐。默认 compose 仍使用 SQLite，用户取消注释 PostgreSQL 或 MySQL 服务即可切换。

## 后端架构

### 数据库抽象

在 `internal/usage` 中引入统一数据库后端：

- `DatabaseBackend`：记录类型、DSN、SQLite 路径、schema、连接池设置。
- `OpenDatabaseBackend`：按配置打开 `*sql.DB`，完成 ping、连接池配置、版本查询。
- `Dialect`：封装 SQL 方言差异。
- `InitDatabase`：替代 SQLite-only `InitDB`，负责建表、迁移 schema、初始化缓存和后台维护。
- `InitDB` 保留为测试兼容 wrapper，内部调用 SQLite backend。

Dialect 至少负责：

- 占位符：SQLite/MySQL 使用 `?`，PostgreSQL 使用 `$1`。
- 自增主键：SQLite `INTEGER PRIMARY KEY AUTOINCREMENT`，PostgreSQL `BIGSERIAL` 或 identity，MySQL `BIGINT AUTO_INCREMENT`。
- 布尔值：统一在 Go 层转换，表内优先用 integer/bool 的可移植映射。
- BLOB：SQLite `BLOB`，PostgreSQL `BYTEA`，MySQL `LONGBLOB`。
- 时间：统一用 UTC `time.Time`，必要时在 MySQL DSN 要求 `parseTime=true`。
- upsert：PostgreSQL `ON CONFLICT`，SQLite `ON CONFLICT`，MySQL `ON DUPLICATE KEY UPDATE`。
- truncate/clear：只作用于已知 CliRelay 表。
- 表/列 introspection：替代 SQLite-only 错误字符串判断和 PRAGMA 逻辑。
- 数据库大小查询：SQLite 文件大小，PostgreSQL `pg_database_size`，MySQL information_schema。

### Schema 管理

把当前散落在 `usage_db.go`、`apikey_db.go`、`model_config_db.go`、`pricing_db.go`、`routing_config_db.go`、`proxy_pool_db.go`、`runtime_settings_db.go` 中的建表 SQL 拆成可跨库的 schema 定义。

建议新增：

- `internal/usage/db_schema.go`：声明表、列、索引、主键、外键和初始化顺序。
- `internal/usage/db_migrations.go`：声明 schema version 和增量迁移步骤。
- `schema_migrations` 表：记录已应用的内部 schema 版本。

第一版可以把现有“启动时幂等建表 + 补列”迁移为显式版本：

```text
version 1: 基础 usage/request_logs 表
version 2: request_log_content
version 3: cost/api_key_name/first_token_ms/detail_content
version 4: api_keys/routing/proxy/runtime/model/proxy_pool/quotas
```

后续新增列不再依赖“duplicate column name”字符串判断，而是通过 dialect introspection 判断列是否存在。

### 服务启动

`internal/cmd/run.go` 当前固定生成 `<config-dir>/data/usage.db`。改造后流程为：

1. 读取并 normalize `cfg.Database`。
2. 解析配置文件目录和默认数据目录。
3. 根据数据库类型打开 backend。
4. 执行 schema 初始化和 schema migrations。
5. 初始化 request log storage、API key cache、model config cache、routing/proxy/runtime settings。
6. 继续执行现有 `MigrateAPIKeysFromConfig`、`ApplyStoredRoutingConfig` 等逻辑。

SQLite 的 WAL、busy timeout、VACUUM 仅在 SQLite dialect 下执行；PostgreSQL/MySQL 不执行 SQLite PRAGMA。

## 系统信息页体验

系统信息页新增“数据库”区域：

- 当前数据库：SQLite / PostgreSQL / MySQL。
- 连接状态：正常、异常、最近错误。
- 版本：SQLite/PostgreSQL/MySQL server version。
- 位置：SQLite 路径或脱敏 DSN。
- 数据大小：总大小、request body 存储大小、各表行数。
- 最近备份：时间、文件大小、校验状态。
- 最近迁移：来源、目标、状态、开始/结束时间。

操作按钮：

- 测试连接。
- 备份数据库。
- 恢复备份。
- 迁移数据库。
- 下载最近备份。

所有危险操作都需要二次确认。迁移和恢复默认不重启服务；如果最终切换数据库需要重启或重新打开连接，UI 必须明确提示“需要重启后生效”或由后端提供安全的热切换流程。

## 备份设计

备份接口：

- `GET /v0/management/database`
- `POST /v0/management/database/backups`
- `GET /v0/management/database/backups`
- `GET /v0/management/database/backups/:id/download`
- `POST /v0/management/database/backups/:id/restore-preview`
- `POST /v0/management/database/backups/:id/restore`

备份包格式建议使用 `.clirelay-db-backup.zip`：

```text
manifest.json
tables/request_logs.jsonl.zst
tables/request_log_content.jsonl.zst
tables/api_keys.jsonl.zst
...
checksums.sha256
```

`manifest.json` 示例：

```json
{
  "format": "clirelay-db-backup",
  "version": 1,
  "created_at": "2026-05-04T00:00:00Z",
  "source": {
    "type": "sqlite",
    "masked_dsn": "/root/.cli-proxy-api/data/usage.db"
  },
  "tables": [
    {"name": "request_logs", "rows": 1200, "bytes": 200000}
  ],
  "schema_version": 4
}
```

备份规则：

- 备份过程只读源数据库。
- 每张表按主键稳定排序导出。
- 大表流式写入，避免把所有数据放进内存。
- 备份包带 manifest 和 SHA256。
- 默认不包含明文数据库 DSN。
- API Key 是否脱敏需要谨慎：数据库迁移备份用于恢复，应保留真实值；下载前 UI 必须提示“备份包包含敏感数据，请妥善保管”。
- 备份文件写到 `backup-dir`，下载后是否删除由用户选择。

恢复规则：

- 恢复前先 preview，展示来源类型、表、行数、schema 版本、是否覆盖当前库。
- 默认恢复到一个新目标数据库或新 SQLite 文件，不默认覆盖 active 数据库。
- 覆盖 active 数据库必须要求二次确认，并建议先自动创建当前数据库备份。
- 恢复后执行完整校验。

## 迁移设计

迁移接口：

- `POST /v0/management/database/test`
- `POST /v0/management/database/migrations/preview`
- `POST /v0/management/database/migrations`
- `GET /v0/management/database/migrations/:id`
- `POST /v0/management/database/migrations/:id/confirm-cutover`
- `POST /v0/management/database/migrations/:id/cancel`

迁移状态：

```text
pending -> running -> verifying -> ready_for_cutover -> cutover_done
                 \-> failed
                 \-> cancelled
```

迁移流程：

1. 用户选择目标类型并填写 DSN 或 SQLite 路径。
2. 后端测试目标连接，读取版本，检查权限。
3. 后端 preview：检查目标是否为空、表是否存在、预计行数、潜在风险。
4. 用户确认开始迁移。
5. 后端在目标库创建 schema。
6. 如用户选择 overwrite，只清空 CliRelay 已知表。
7. 后端按表和主键顺序批量复制数据。
8. 后端校验每张表行数、关键聚合、随机样本哈希。
9. 校验通过后状态变为 `ready_for_cutover`。
10. 用户确认切换。
11. 后端更新配置文件或返回“需要按环境变量修改部署配置”的说明。
12. 后端尝试重新打开数据库连接；如果不支持热切换，则标记“重启后生效”。

切换规则：

- 如果数据库配置由 YAML 控制，可以由后端写入 `config.yaml`。
- 如果数据库配置由环境变量控制，后端不能改环境变量，只能生成操作建议。
- 如果当前服务不能安全热切换，确认切换只更新配置并提示重启；不得擅自重启服务。
- 任何切换失败都不删除源数据库。

迁移复制规则：

- 每张表使用稳定列清单，不使用 `SELECT *`。
- 按主键或唯一键排序；无自增主键的表按主键列组合排序。
- `request_log_content` 这类大 BLOB 表单独批量复制。
- 外键表按依赖顺序复制；恢复/迁移时先主表后子表。
- PostgreSQL schema 名称必须安全 quote。
- MySQL 表和列使用 backtick quote。
- 所有已知表以 transaction batch 写入目标库。

## 管理 API 响应草案

`GET /v0/management/database`：

```json
{
  "type": "sqlite",
  "connected": true,
  "version": "3.50.4",
  "masked_dsn": "/root/.cli-proxy-api/data/usage.db",
  "schema_version": 4,
  "size_bytes": 10485760,
  "request_log_content_bytes": 5242880,
  "tables": [
    {"name": "request_logs", "rows": 1200},
    {"name": "api_keys", "rows": 8}
  ],
  "config_source": "yaml",
  "last_backup": null,
  "active_migration": null
}
```

`POST /v0/management/database/test`：

```json
{
  "type": "postgresql",
  "dsn": "postgres://clirelay:<password>@127.0.0.1:5432/clirelay?sslmode=disable",
  "schema": "public"
}
```

响应：

```json
{
  "ok": true,
  "type": "postgresql",
  "version": "PostgreSQL 17.2",
  "masked_dsn": "postgres://clirelay:***@127.0.0.1:5432/clirelay?sslmode=disable",
  "warnings": []
}
```

`GET /v0/management/database/migrations/:id`：

```json
{
  "id": "mig_20260504_001",
  "source_type": "sqlite",
  "target_type": "mysql",
  "status": "running",
  "current_table": "request_logs",
  "tables_done": 3,
  "tables_total": 12,
  "rows_done": 15000,
  "rows_total": 42000,
  "percent": 35.7,
  "message": "Copying request_logs",
  "error": ""
}
```

## 前端交互设计

系统信息页数据库区域建议分为三层：

1. 概览卡片：当前类型、连接状态、大小、行数、版本。
2. 备份卡片：最近备份、创建备份、下载、恢复。
3. 迁移卡片：目标数据库、测试连接、预览、执行迁移、确认切换。

迁移弹窗步骤：

1. 选择目标数据库类型。
2. 填写 DSN 或 SQLite 文件路径。
3. 点击“测试连接”。
4. 展示 preview：源库、目标库、表数量、行数、目标是否为空。
5. 用户确认开始迁移。
6. 进度条每秒轮询，显示当前表、行数、状态。
7. 校验通过后展示“确认切换”。
8. 切换完成后展示当前数据库信息；如需重启，则显示明确提示。

UI 文案要避免误导：

- “迁移不会删除旧数据库。”
- “切换前建议先创建备份。”
- “备份包包含敏感数据。”
- “环境变量控制的数据库配置不能从面板直接保存。”
- “生产环境重启/部署需要人工确认。”

## 安全与审计

- 所有数据库管理接口必须走现有 management middleware。
- DSN、密码、API Key、备份下载都属于敏感操作。
- 日志只能输出 masked DSN。
- 备份文件默认权限 `0600`。
- 备份下载响应使用 attachment 文件名，不在 URL 中暴露真实路径。
- 迁移和恢复操作写入审计日志，记录操作者 IP、目标类型、masked DSN、表数、行数、状态、耗时。
- 禁止 API 接收任意 SQL。
- 目标表清理只能作用于固定表白名单。

## 错误处理

| 场景 | 行为 |
| --- | --- |
| 目标连接失败 | preview/test 返回错误，不创建 migration job |
| 目标库已有数据 | preview 返回 warning，默认禁止继续 |
| 复制中断 | job 标记 failed，旧库继续 active |
| 校验失败 | job 标记 failed，禁止 cutover |
| 写 config 失败 | 数据已复制但不切换，提示手工配置 |
| 热切换失败 | 回退旧连接，提示重启或手工处理 |
| 备份包校验失败 | 禁止恢复 |
| MySQL DSN 缺少 `parseTime=true` | test 返回 warning 或 error |

## 测试方案

### 单元测试

- 配置默认值和环境变量覆盖。
- DSN masking。
- Dialect placeholder、quote、upsert、truncate、version query。
- schema 定义生成。
- SQLite legacy `InitDB` wrapper 兼容。
- column/table introspection。
- 备份 manifest 和 checksum。
- 迁移状态机。

### Handler 测试

- `GET /database` 不泄露 DSN 密码。
- `POST /database/test` 成功/失败。
- preview 目标非空时返回 warning。
- migration job 进度查询。
- cutover 前失败不能修改 config。
- 环境变量控制时禁止从 UI 保存。
- backup download 权限和文件名。

### 集成测试

使用 Docker Compose 或 testcontainers 启动 PostgreSQL 和 MySQL：

- SQLite -> PostgreSQL。
- SQLite -> MySQL。
- PostgreSQL -> SQLite。
- PostgreSQL -> MySQL。
- MySQL -> SQLite。
- MySQL -> PostgreSQL。
- 每个方向都验证表行数、样本 hash、request body BLOB、时间字段、API Key 权限、routing/proxy/runtime settings。

### 端到端测试

- 启动本地 CliRelay，生成请求日志和 API Key。
- 在系统信息页创建备份。
- 迁移到 PostgreSQL，确认切换。
- 管理面板检查日志、模型配置、API Key、路由、代理池仍可读写。
- 再迁移到 MySQL，重复验证。
- 再迁回 SQLite，重复验证。

### 回归测试

至少运行：

```bash
go test ./internal/usage ./internal/config ./internal/api/handlers/management
go test ./...
```

三库集成测试可以用 build tag，例如：

```bash
go test -tags integration_db ./internal/usage ./internal/api/handlers/management
```

## 实施拆分建议

1. 配置层：新增 `database` 配置、默认值、环境变量、文档示例。
2. Dialect 层：抽象 SQLite/PostgreSQL/MySQL SQL 差异。
3. Schema 层：把当前 usage 表定义迁移成跨库 schema。
4. Runtime 初始化：服务启动按配置打开数据库，保持 SQLite 默认兼容。
5. 查询写入改造：逐个替换 SQLite-only SQL。
6. 备份服务：实现 manifest、流式导出、checksum、下载。
7. 迁移服务：实现 preview、copy、verify、状态机、cutover。
8. 管理 API：注册数据库信息、测试、备份、迁移接口。
9. 管理面板：系统信息页新增数据库卡片、备份、迁移向导。
10. 集成测试：补 PostgreSQL/MySQL 测试环境和三库互迁矩阵。
11. README 和部署示例：补三种数据库部署方式。

## 验收标准

- 未配置 `database` 的旧实例仍使用原 SQLite 路径。
- 新实例能只靠 YAML 或环境变量选择 SQLite/PostgreSQL/MySQL。
- 系统信息页能看到数据库类型、连接、版本、大小和表统计。
- 能从系统信息页创建数据库备份并下载。
- 能从 SQLite 迁移到 PostgreSQL/MySQL，迁移后所有管理功能可用。
- 能在 PostgreSQL/MySQL/SQLite 之间任意方向迁移。
- 迁移失败不会改变 active 数据库。
- DSN 密码不会出现在 API 响应、日志和 UI 展示中。
- 所有核心单元测试、handler 测试和三库集成测试通过。

## 生产操作边界

本功能的设计、开发和验证默认只在本地完成。即使有已部署实例，也不应因为本地测试通过就自动部署、重启或替换远端服务。生产环境数据库迁移需要单独的人工确认流程，至少包括：

1. 确认目标数据库连接和权限。
2. 创建当前数据库备份。
3. 在低峰期执行迁移。
4. 校验系统信息页、请求日志、API Key、路由、模型配置和代理池。
5. 由用户明确确认是否重启或切换生产配置。
