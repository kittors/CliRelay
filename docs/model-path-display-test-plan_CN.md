# 模型路径展示全路径测试方案

更新时间：2026-05-11

## 1. 目标

验证模型 ID、路径作用域、系统默认路径、渠道分组路径在管理端展示和真实接口返回之间保持一致。

本测试覆盖三类页面：

- `manage/system`：只展示默认根路径 `GET /v1/models` 返回的模型。
- `manage/models`：展示模型 ID 与其可用调用路径，支持一个模型聚合多个路径。
- `manage/channel-groups`：展示系统默认根路径 `/`，展示路径可用能力，新增/编辑路径时做唯一性和保留路径校验。

测试部署环境的具体地址、访问密钥、服务器信息不写入本文档。执行时由测试人员在本地环境或临时变量中提供。

## 2. 前置条件

1. 后端功能分支已合并到 `dev`。
2. 前端功能分支已合并到 `dev`。
3. GitHub Actions 已完成，并确认部署流水线成功。
4. 测试部署环境已经拉取到包含本次变更的后端版本与前端面板版本。
5. 测试人员持有管理端访问权限和至少一个可调用的 API Key。

建议本地设置：

```bash
export TEST_BASE_URL="https://<test-deployment-host>"
export TEST_MANAGEMENT_KEY="<management-key>"
export TEST_API_KEY="<api-key>"
```

## 3. 敏感信息规则

测试记录中不得写入：

- 测试部署域名
- IP 地址
- SSH 命令
- 管理端访问密钥
- API Key
- Auth 文件 ID、账号 ID、邮箱
- 上游 token 或 cookie

需要截图时，只保留页面主体，不展示浏览器地址栏、密钥、账号、认证文件详情。

## 4. 接口基线检查

### 4.1 默认根路径模型

执行：

```bash
curl -sS "$TEST_BASE_URL/v1/models" \
  -H "Authorization: Bearer $TEST_API_KEY"
```

验证：

- 返回 HTTP 200。
- 返回体包含 `data` 数组。
- 记录模型 ID 集合，作为 `manage/system` 的对照基线。

### 4.2 默认根路径主要能力

至少验证这些路径可被路由识别：

```text
GET  /v1/models
POST /v1/chat/completions
POST /v1/completions
POST /v1/responses
POST /v1/images/generations
POST /v1/images/edits
POST /v1/messages
POST /v1/messages/count_tokens
GET  /v1beta/models
```

不要求每个路径都真实消耗上游额度。对高成本或需要特殊账号的路径，可以只使用最小请求确认鉴权、模型校验或预期错误是否来自对应 handler，而不是 404。

### 4.3 自定义路径模型发现

对每个已配置 path-route，执行：

```bash
curl -sS "$TEST_BASE_URL/<path>/v1/models" \
  -H "Authorization: Bearer $TEST_API_KEY"
```

验证：

- 返回 HTTP 200，或在 API Key 无权限时返回明确的 403。
- 返回模型集合只代表该路径作用域。
- 该模型集合能在 `manage/models` 对应路径列中体现。

### 4.4 Gemini 路径模型发现

执行：

```bash
curl -sS "$TEST_BASE_URL/v1beta/models" \
  -H "Authorization: Bearer $TEST_API_KEY"
```

如果存在自定义路径，也执行：

```bash
curl -sS "$TEST_BASE_URL/<path>/v1beta/models" \
  -H "Authorization: Bearer $TEST_API_KEY"
```

验证：

- `manage/system` 不混入 `/v1beta/models` 独有模型。
- `manage/models` 能把 v1beta 能力展示为独立路径能力。

## 5. `manage/system` 页面测试

步骤：

1. 打开管理端 `manage/system`。
2. 定位“可用模型”区域。
3. 点击刷新。
4. 对比页面模型列表和接口基线 `GET /v1/models` 的模型 ID 集合。

验证：

- 页面模型集合与默认根路径 `GET /v1/models` 返回一致。
- 不出现只在自定义路径 `/v1/models` 下返回的模型。
- 不出现只在 AMP provider alias 路径下返回的模型。
- 不出现只在 `/v1beta/models` 下返回的模型。
- 标题旁有感叹号或提示图标。
- hover 提示图标时，tooltip 文案为：

```text
这里只展示默认根路径 GET /v1/models 返回的模型；自定义路径、渠道分组、别名与其他协议路径的模型请到“模型”页面查看。
```

## 6. `manage/models` 页面测试

步骤：

1. 打开管理端 `manage/models`。
2. 检查模型表是否存在“调用路径”列。
3. 搜索一个在默认路径和自定义路径都可用的模型。
4. 搜索一个只在自定义路径可用的模型。
5. 搜索一个 alias 或 prefix 暴露出的模型 ID。

验证：

- 同一模型 ID 可聚合多个路径 badge。
- 路径 badge 可区分 `root:/v1/...`、`group:{path}/v1/...`、`root:/v1beta/...`、`amp:/api/provider/{provider}/...`。
- hover 路径 badge 可以看到完整 method + path。
- alias/prefix 模型和上游模型关系清晰，不把 alias 当成上游真实 ID。
- `GET <path>/v1/models` 返回的路径作用域模型能在对应模型行体现。
- 不使用模型数量来表达路径能力。

## 7. `manage/channel-groups` 页面测试

### 7.1 默认根路径展示

步骤：

1. 打开管理端 `manage/channel-groups`。
2. 查看路径列表或分组路径区域。

验证：

- 存在系统默认根路径 `/`。
- 默认根路径标记为系统内置或只读。
- 默认根路径不能编辑、删除、关闭。
- 页面不展示“模型数/模型数量”列。
- 页面展示可用能力标签，例如 `models`、`chat`、`completions`、`responses`、`messages`、`images`、`v1beta`。
- hover 能力标签时展示完整接口路径。

### 7.2 新增路径必填与唯一性

步骤：

1. 新增 channel group，只填分组名称，不填访问路径。
2. 新增 channel group，只填访问路径，不填分组名称。
3. 新增 channel group，访问路径填 `/`。
4. 新增 channel group，访问路径填已有路径。
5. 新增 channel group，访问路径填保留系统入口。
6. 新增 channel group，访问路径填包含空 segment 的路径。
7. 新增 channel group，访问路径填合法且不重复的路径。

验证：

- 分组名称必填。
- 访问路径必填。
- `/` 不能保存。
- 重复路径不能保存。
- 保留系统入口不能保存。
- 空 segment 路径不能保存。
- 合法路径可保存。
- 错误在输入框附近即时展示，不等保存后静默失败。

### 7.3 编辑路径校验

步骤：

1. 编辑已有 channel group，保持原 path 不变。
2. 编辑已有 channel group，把 path 改成另一个已有 path。
3. 编辑已有 channel group，把 path 改成 `/`。
4. 编辑已有 channel group，把 path 改成合法新 path。

验证：

- 保持自身原 path 可保存。
- 改成其它已有 path 会阻止。
- 改成 `/` 会阻止。
- 改成合法新 path 可保存。
- 保存后 `GET <new-path>/v1/models` 可按新路径返回模型或权限错误。

## 8. 回归测试

需要确认这些既有能力没有被破坏：

- API Key 创建、编辑、权限模板选择。
- ccswitch 导出配置中的 route path 拼接。
- image-generation 测试入口仍然指向 `/v1/images/generations` 或 `/v1/images/edits`。
- OpenAI compatibility provider 拉取 `/models` 不受管理端路径展示逻辑影响。
- `manage/system` 版本信息、API Base、管理端 endpoint 等原有信息正常。

## 9. 通过标准

本次功能通过标准：

1. `manage/system` 与默认根路径 `GET /v1/models` 返回一致。
2. `manage/models` 能按模型聚合展示默认路径、自定义路径和协议路径。
3. `manage/channel-groups` 展示系统默认根路径 `/` 和路径能力标签。
4. 路径新增/编辑校验覆盖必填、重复、根路径、保留路径和格式错误。
5. 前后端自动化测试通过。
6. GitHub Actions 部署完成后，测试部署环境按本文档验证通过。
