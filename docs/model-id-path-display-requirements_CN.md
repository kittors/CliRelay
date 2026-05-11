# 模型 ID 与调用路径展示需求草案

更新时间：2026-05-11

## 1. 背景

当前管理端里容易把几个概念混在一起：

- 当前可用模型：运行时 registry 中能够被某些渠道处理的模型集合。
- 模型别名：客户端请求时看到的模型 ID，可能会在执行前映射回上游原始模型。
- 默认路径可直连模型：不依赖自定义路径、路径分组或别名，在默认 API 路径下可以直接调用的原始模型 ID。
- 路径相关模型：同一个模型 ID 可能只适用于某些接口族，例如聊天、补全、生图、Claude Messages、Gemini 原生路径，或者某个自定义路径分组。

本次需求的目标是把这些概念拆开展示：

1. `manage/models` 作为完整模型清单入口，展示模型 ID 与它支持的调用路径，路径可以有多个并聚合显示。
2. `manage/system` 只展示默认根路径下 `GET /v1/models` 返回的模型 ID，并用感叹号提示说明这个列表不是完整模型清单。
3. 后续实现前，需要明确哪些位置会影响模型 ID，哪些位置会影响请求地址或路径生成。

## 2. 术语约定

| 名称 | 含义 |
| --- | --- |
| 上游模型 ID | provider 原始模型名，例如某个 API key 或 OAuth 账号真实可调用的模型名。 |
| 对外模型 ID | 客户端请求体中的 `model`，也是 `/models` 或管理端表格里展示给用户的 ID。它可能是原始模型，也可能是别名或 prefix 后的模型。 |
| 别名 | `name -> alias` 形式的映射。用户请求 alias，执行层需要映射回 name。 |
| prefix 模型 | 渠道配置了 `prefix` 后生成的 `prefix/model` 形式。 |
| 默认路径 | 不带自定义路径前缀的公共 API 路径，例如 `/v1/chat/completions`、`/v1/completions`、`/v1/images/generations`。 |
| 自定义路径 | 渠道分组或 path-routes 生成的路径前缀，例如 `/{group}/v1/...` 或配置的多段路径前缀。 |
| 路径族 | 一组同协议接口，例如 OpenAI v1、Claude Messages、Gemini v1beta、AMP provider alias。 |

## 3. 当前客户端可调用路径清单

### 3.1 默认 OpenAI/Claude v1 路径

后端在 `CliRelay/internal/api/server.go` 注册了默认 `/v1` 路径：

| 方法 | 路径 | 模型 ID 来源 |
| --- | --- | --- |
| `GET` | `/v1/models` | registry 中 OpenAI 或 Claude 视图，受 API Key 权限过滤。 |
| `POST` | `/v1/chat/completions` | 请求体 `model`。 |
| `POST` | `/v1/completions` | 请求体 `model`，内部会转为 chat completions 处理。 |
| `POST` | `/v1/images/generations` | 请求体或 multipart/form 中的 `model`；未传时当前默认 `gpt-image-2`。 |
| `POST` | `/v1/images/edits` | multipart/form 中的 `model`；未传时当前默认 `gpt-image-2`。 |
| `POST` | `/v1/messages` | 请求体 `model`，Claude Messages 兼容路径。 |
| `POST` | `/v1/messages/count_tokens` | 请求体 `model`，Claude token count 路径。 |
| `GET` | `/v1/responses` | websocket responses 路径。 |
| `POST` | `/v1/responses` | 请求体 `model`。 |
| `POST` | `/v1/responses/compact` | 请求体 `model`。 |

### 3.2 默认 Gemini v1beta 路径

后端同样注册了不带自定义路径前缀的 `/v1beta`：

| 方法 | 路径 | 模型 ID 来源 |
| --- | --- | --- |
| `GET` | `/v1beta/models` | Gemini 原生模型列表。 |
| `POST` | `/v1beta/models/*action` | URL path 中的 `models/{model}:action` 或等价 action path。 |
| `GET` | `/v1beta/models/*action` | URL path 中的模型或 action。 |

### 3.3 渠道分组与自定义路径

当前存在两种分组路径入口：

| 入口 | 说明 |
| --- | --- |
| `/:group/v1/...`、`/:group/v1beta/...` | 单段 group 入口。group 可来自已知 channel group，也可来自 path-routes。 |
| `/{custom/path}/v1/...`、`/{custom/path}/v1beta/...` | NoRoute rewrite 会在最后一个 `/v1/` 或 `/v1beta/` 前拆出自定义路径，支持多段自定义路径。 |

这些路径会把请求限制到指定渠道分组，并继续叠加 API Key 的 `allowed-channel-groups`、`allowed-channels` 和 `allowed-models` 权限。

同一个模型发现接口也会在这些路径下生效：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/{group}/v1/models` | 获取该 group 路径下当前 API Key 可使用的模型。 |
| `GET` | `/{custom/path}/v1/models` | 获取该自定义路径下当前 API Key 可使用的模型。 |
| `GET` | `/{group}/v1beta/models` | 获取该 group 路径下的 Gemini 原生模型列表。 |
| `GET` | `/{custom/path}/v1beta/models` | 获取该自定义路径下的 Gemini 原生模型列表。 |

因此，`/models` 不能只按默认路径理解。它也是路径作用域模型发现接口：同一个 API Key 在默认 `/v1/models`、某个 group 的 `/v1/models`、某个多段自定义路径的 `/v1/models` 下，返回的可用模型集合可能不同。后续 `manage/models` 的“调用路径”列应把这些路径作用域纳入模型来源，而不是只展示全局 registry 的合并结果。

### 3.4 AMP provider alias 路径

AMP 模块注册了 `/api/provider/:provider` 系列路径：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/api/provider/:provider/models` | 按 provider 返回 OpenAI、Claude 或 Gemini 视图。 |
| `POST` | `/api/provider/:provider/chat/completions` | OpenAI 兼容聊天路径。 |
| `POST` | `/api/provider/:provider/completions` | OpenAI 兼容补全路径。 |
| `POST` | `/api/provider/:provider/responses` | OpenAI responses 路径。 |
| `GET` | `/api/provider/:provider/v1/models` | v1 形式模型列表。 |
| `POST` | `/api/provider/:provider/v1/chat/completions` | v1 形式聊天路径。 |
| `POST` | `/api/provider/:provider/v1/completions` | v1 形式补全路径。 |
| `POST` | `/api/provider/:provider/v1/responses` | v1 形式 responses 路径。 |
| `POST` | `/api/provider/:provider/v1/messages` | Claude Messages 路径。 |
| `POST` | `/api/provider/:provider/v1/messages/count_tokens` | Claude token count 路径。 |
| `GET` | `/api/provider/:provider/v1beta/models` | Gemini v1beta 模型列表。 |
| `POST` | `/api/provider/:provider/v1beta/models/*action` | Gemini v1beta action 路径。 |
| `GET` | `/api/provider/:provider/v1beta/models/*action` | Gemini v1beta action 查询路径。 |
| `ANY` | `/api/provider/google/v1beta1/*path` | Google v1beta1 passthrough/bridge 路径，模型通常在 URL path 中。 |

AMP 路径还会受到 AMP model mapping 与 fallback 逻辑影响，不能简单等同于默认 `/v1` 的模型集合。

### 3.5 其它路径

| 路径 | 说明 |
| --- | --- |
| `/v0/management/image-generation/test` | 管理端生图测试接口，不是普通客户端模型调用路径。它可以产生日志和验证链路，但不应作为 `manage/models` 的客户端路径列展示。 |

## 4. 影响模型 ID 的位置

### 4.1 后端运行时与配置

| 位置 | 影响 |
| --- | --- |
| `sdk/cliproxy/service.go` 的 `registerModelsForAuth` | 将不同 provider/auth 的模型注册到全局 registry，是运行时模型可见性的核心入口。 |
| `buildConfigModels` | API key provider 配置中的 `models[].alias` 会成为对外模型 ID；无 alias 时使用 `models[].name`。 |
| OpenAI compatibility 配置 | OpenAI 兼容 provider 的 `models[].alias` 同样会成为对外模型 ID。 |
| `applyOAuthModelAlias` | OAuth 模型别名会改变 registry 中展示的模型 ID；如果 alias 配置了 fork，则可能同时保留原始模型和 alias。 |
| `applyModelPrefixes` | 渠道 `prefix` 会生成 `prefix/model`；当 `force-model-prefix` 开启时，原始模型 ID 可能不再暴露。 |
| `applyExcludedModels` | `excluded-models` 支持通配符过滤，过滤后模型不会进入 registry。 |
| `sdk/cliproxy/auth/conductor.go` 的 `rewriteModelForAuth` | 执行前会把匹配当前 auth prefix 的 `prefix/model` 去掉，再交给上游。 |
| `applyAPIKeyModelAlias` | API key provider 的 alias 在执行前映射回上游模型名。 |
| `applyOAuthModelAlias` 执行层逻辑 | OAuth alias 在执行前映射回上游模型名，并保留 thinking suffix。 |
| `codex-auto-review` 内置映射 | Codex OAuth 场景存在内置模型 alias，会在执行层映射到实际上游模型。 |
| `internal/api/modules/amp/model_mapping.go` | AMP 的 `from -> to` 或 regex mapping 会把请求模型改写为另一个本地可用模型。 |
| AMP `force-model-mappings` | 开启时 AMP mapping 优先于本地同名 provider 选择。 |
| Bedrock executor | Bedrock 的模型配置、region、provider 格式和自定义 mapping 会影响最终上游模型 ID。 |
| 静态模型库与数据库模型配置 | `internal/registry` 提供静态模型定义；`model-configs`/SQLite 保存价格、描述、启用状态等元数据，但这些元数据不等同于运行时可调用性。 |

### 4.2 后端权限与过滤

| 位置 | 影响 |
| --- | --- |
| API Key `allowed-models` | 过滤 `/v1/models` 返回；POST 请求会通过 `ModelRestrictionMiddleware` 校验请求体 `model`。 |
| API Key `allowed-channels` | 缩小可选渠道集合，也会影响管理端按渠道过滤模型。 |
| API Key `allowed-channel-groups` | 缩小默认路径候选渠道集合；访问自定义路径时也要和路径 group 求交集。 |
| Channel group `allowed-models` | 限制某个渠道分组可服务的模型集合。 |
| Path route group | 自定义路径命中后会限制到对应渠道分组。 |
| `CanServeModelWithScopes` | 管理端模型筛选会用它判断某模型在指定渠道/分组范围内是否可服务。 |

### 4.3 前端管理入口

| 页面/模块 | 影响 |
| --- | --- |
| `manage/ai-providers` | 配置 provider 的 `models[].name`、`models[].alias`、`prefix`、`excludedModels`、OpenAI compatibility models、AMP model mappings 等。 |
| `manage/auth-files` | 配置 OAuth excluded models、OAuth model alias；模型 owner group 映射目前是前端本地展示辅助，不应被当成后端路由事实。 |
| `manage/ccswitch-import-settings` | 配置导出的客户端 `request-model`、`target-model`、`route-path`、`endpoint-path`、默认模型；影响客户端实际请求的模型 ID 和 base URL。 |
| `manage/image-generation` | 当前硬编码围绕 `gpt-image-2` 展示和测试；会影响用户对生图模型与 `/v1/images/*` 路径的理解。 |
| `manage/channel-groups` | 配置 channel group、`allowed-models`、`path-routes`，并会按 selected channels/groups 获取模型候选；后续还需要展示系统默认根路径，并校验自定义路径唯一性。 |
| `manage/api-keys` | 配置 API Key 的 `allowed-models`、`allowed-channels`、`allowed-channel-groups`；导入客户端配置时会拼接 route path。 |
| `manage/api-key-permissions` | 权限模板会批量影响 API Key 的模型/渠道/分组权限。 |
| `manage/models` | 当前主要展示模型配置和 active/library scope；后续应成为“模型 ID + 路径族”的完整展示入口。 |
| `manage/system` | 当前展示模型 tag；后续应明确展示默认根路径 `GET /v1/models` 的返回模型，并提示不是全量模型清单。 |

## 5. 影响请求地址或路径生成的位置

| 位置 | 影响 |
| --- | --- |
| 后端默认 route registration | 决定 `/v1`、`/v1beta`、`/api/provider` 等公共入口是否存在。 |
| `splitGroupedAPIPath` | 支持多段自定义路径在最后一个 `/v1/` 或 `/v1beta/` 前被拆分并重写。 |
| `resolvePathRouteContext` | 将路径前缀解析为 channel group 或 path-route。 |
| Channel group `path-routes` | 管理端配置的路径前缀，是自定义调用地址的来源。 |
| `manage/channel-groups` | 用户编辑 route path，也会把完整访问 URL 规范化为保存用 path；新增/编辑时必须校验不能和系统默认根路径或已有路径重复。 |
| `manage/api-keys` 的 `appendRoutePath` | 导入客户端配置时把 base API URL 与 route path 拼成客户端 base URL。 |
| `manage/ccswitch-import-settings` | `endpoint-path` 决定 Claude/Codex/Gemini 客户端实际 endpoint；`route-path` 决定是否使用自定义路径入口。 |
| `ccswitchImport.ts` 的 `joinCcSwitchEndpoint` | 生成导出配置中的 endpoint，避免重复拼接同一个 endpoint path。 |
| OpenAI compatibility provider 的 models fetch URL | 前端用 provider base URL 拼 `/models` 拉取上游模型，用于生成 provider models。 |
| `manage/image-generation` | 生图说明和测试固定使用当前服务 base URL + `/v1/images/generations` 或 `/v1/images/edits`。 |
| 路径作用域 `/models` | `GET /v1/models`、`GET /{group}/v1/models`、`GET /{custom/path}/v1/models` 会按当前路径、API Key、channel group 和权限返回不同模型集合，是判断某条路径可用模型的重要来源。 |

## 6. `manage/channel-groups` 展示与校验需求

`manage/channel-groups` 需要把系统默认根路径作为一个明确的只读入口展示出来，避免用户误以为只有手动创建的 channel group path 才能调用模型。

系统默认路径的展示口径：

| 项 | 要求 |
| --- | --- |
| 展示名称 | `系统默认` 或 `默认根路径`。 |
| 路径 | 用 `/` 表示根入口，对应实际调用时不带自定义前缀的 `/v1/...`、`/v1beta/...`。 |
| 分组 | 不绑定某个 channel group，表示走默认全局调度，并继续受 API Key 权限限制。 |
| 状态 | 只读展示，不允许编辑、删除、关闭。 |
| 模型发现 | 默认路径可通过 `GET /v1/models` 和 `GET /v1beta/models` 获取对应协议下可用模型。 |

在 channel group 列表或路径配置区域中，系统默认路径应和用户自定义路径放在同一个信息层级里展示，但视觉上要标记为系统内置/只读。这样用户查看路径时能同时看到：

- 默认根路径 `/`
- 每个 channel group 或 path-route 对应的自定义路径
- 每条路径对应的 group、策略、allowed models 和可用模型入口

路径列表不需要展示“模型数”列。模型数量本身不能说明路径能力，容易让用户误解为完整可调用性。这里应展示“可用能力/可用入口”：

| 展示项 | 要求 |
| --- | --- |
| 可用能力 | 用紧凑标签展示该路径可用的接口能力，例如 `models`、`chat`、`completions`、`responses`、`messages`、`images`、`v1beta`。 |
| Tooltip | 鼠标悬停能力标签时展示完整接口路径，例如 `GET /v1/models`、`POST /v1/chat/completions`、`POST /v1/images/generations`。 |
| 默认根路径 | `/` 行展示默认路径下可用能力，例如 `/v1/models`、`/v1/chat/completions`、`/v1/images/generations`、`/v1beta/models` 等。 |
| 自定义路径 | 自定义 path 行展示该 path 前缀下可用能力，例如 `/{path}/v1/models`、`/{path}/v1/chat/completions`。 |

新增或编辑渠道分组路径时必须做规范化后的唯一性校验：

1. 输入完整 URL 时，先提取 path，再保存为规范化 path。
2. path 必须以 `/` 开头，去掉末尾多余 `/`。
3. `/` 和空 path 保留给系统默认根路径，不能作为自定义路径保存。
4. 同一个规范化 path 不能重复绑定到多个 channel group。
5. 新增渠道分组时，分组名称必填，访问路径也必填，不能只填分组名称后由系统静默生成路径。
6. 访问路径需要在输入时即时校验，不允许保存前才发现错误。
7. 编辑 path 时，如果只等于当前记录自己的旧 path，可以保存；如果等于其它记录或系统默认根路径，必须阻止。
8. path 不允许和管理端/API 内置入口冲突，例如 `/manage`、`/management.html`、`/v0`、`/v1`、`/v1beta`、`/api`、`/anthropic/callback`、`/codex/callback` 等。
9. path 不允许包含空 segment，例如 `/team//a`。
10. 校验失败时，在输入框附近给出明确错误，不等到保存后才静默失败。

## 7. `manage/system` 展示需求

`manage/system` 的模型区域应明确展示默认根路径下 `GET /v1/models` 返回的模型集合：

1. 数据来源就是默认根路径 `GET /v1/models`。
2. 不展示 `/{group}/v1/models`、`/{custom/path}/v1/models` 或 AMP provider alias 路径下才出现的模型。
3. 不展示 `/v1beta/models` 的 Gemini 原生列表，除非后续单独增加 v1beta 区块。
4. 不把 `manage/models` 的完整聚合结果当作 `manage/system` 的展示来源。
5. 不把 ccswitch 的 `request-model`、AMP mapping 的 `from` 等客户端侧映射混入这个列表，除非它本身已经出现在默认根路径 `GET /v1/models` 返回中。

页面提示需要非常明确，建议使用带感叹号图标的紧凑提示条，放在模型列表标题附近：

```text
此处展示默认根路径 GET /v1/models 返回的模型。完整模型、别名和自定义路径请到“模型”页面查看。
```

视觉要求：

- 继续沿用当前系统页的紧凑卡片和 tag 风格。
- 提示条不要做成大面积说明卡，不要影响系统页信息密度。
- 可使用现有图标体系中的 warning/alert 图标。
- 搜索、刷新、数量统计可以保留，但数量含义必须是“GET /v1/models 返回数量”。

## 8. `manage/models` 展示需求

`manage/models` 应升级为模型 ID 的完整解释入口：

1. 模型表增加“调用路径”列。
2. 一个模型 ID 支持多个路径时，路径列聚合显示多个路径。
3. 路径显示应区分默认路径、自定义路径、AMP provider 路径、Gemini 原生路径等路径族。
4. 别名、原始模型、prefix 模型需要能被区分，避免用户误以为 alias 就是上游真实 ID。
5. 路径列不应堆出很长文本；建议使用紧凑 badge，超过数量后用 `+N` 与 hover tooltip 展开。
6. 表格仍保持当前管理端风格：高信息密度、轻量筛选、不要新增营销式说明区。
7. 对于配置了 path-routes 或 channel group 的路径，应通过对应路径下的 `/models` 接口确认该路径实际可用的模型集合。

建议列结构：

| 列 | 内容 |
| --- | --- |
| 模型 ID | 对外模型 ID。 |
| 类型 | 原始 / alias / prefix / mapping / seed 元数据等，后端最好提供明确字段。 |
| 上游模型 | alias 或 mapping 对应的真实上游模型；原始模型可为空或等于自身。 |
| Provider/来源 | owned_by、provider、auth/channel 来源。 |
| 调用路径 | 聚合路径 badge，例如 `/v1/chat/completions`、`/v1/images/generations`、`/{group}/v1/...`。 |
| 权限/范围 | 是否受 API Key、channel group、path route 限制。 |
| 价格/描述/启用 | 延续当前 `manage/models` 的配置能力。 |

路径作用域模型发现应作为 `manage/models` 的重要展示依据：

| 接口 | 用途 |
| --- | --- |
| `GET /v1/models` | 默认 OpenAI/Claude v1 路径的可用模型。 |
| `GET /{group}/v1/models` | 指定 channel group 路径的可用模型。 |
| `GET /{custom/path}/v1/models` | 指定自定义路径的可用模型。 |
| `GET /v1beta/models` | 默认 Gemini v1beta 路径的可用模型。 |
| `GET /{group}/v1beta/models` | 指定 channel group 下 Gemini v1beta 路径的可用模型。 |
| `GET /{custom/path}/v1beta/models` | 指定自定义路径下 Gemini v1beta 路径的可用模型。 |

如果同一个模型 ID 在多个路径作用域的 `/models` 返回中出现，`manage/models` 应聚合为同一行的多个路径 badge；如果同一个上游模型通过 alias 或 prefix 暴露出多个对外模型 ID，应按对外模型 ID 分别展示，并在“上游模型”列标明关系。

路径列建议分层展示：

| 显示 | 含义 |
| --- | --- |
| `default:/v1/chat/completions` | 默认 OpenAI chat 路径。 |
| `default:/v1/completions` | 默认 completions 路径。 |
| `default:/v1/images/generations` | 默认生图路径。 |
| `default:/v1/images/edits` | 默认图生图路径。 |
| `default:/v1/responses` | 默认 responses 路径。 |
| `default:/v1/messages` | 默认 Claude Messages 路径。 |
| `default:/v1beta/models/*action` | 默认 Gemini 原生 action 路径。 |
| `root:/v1/...` | 系统默认根路径，不带自定义前缀。 |
| `root:/v1beta/...` | 系统默认根路径下的 Gemini 原生入口。 |
| `group:{path}/v1/...` | 自定义路径或 channel group 路径。 |
| `amp:/api/provider/{provider}/...` | AMP provider alias 路径。 |

## 9. 后续实现建议

为了避免前端继续靠字符串猜测，建议后续新增一个管理端只读数据源，返回模型与路径的结构化关系。示例字段：

```json
{
  "id": "client-visible-model",
  "canonical_model": "upstream-model",
  "kind": "canonical | alias | prefix | mapping",
  "provider": "codex",
  "owned_by": "openai",
  "default_callable": true,
  "alias": false,
  "paths": [
    {
      "scope": "default",
      "method": "POST",
      "path": "/v1/chat/completions",
      "family": "openai-v1-chat"
    }
  ],
  "constraints": {
    "allowed_models": [],
    "allowed_channels": [],
    "allowed_channel_groups": [],
    "route_paths": []
  }
}
```

实现时需要避免只从 `manage/models` 现有 `model-configs` 推导，因为 `model-configs` 是模型元数据，不是运行时可调用性与路径关系的权威来源。更合理的来源应合并：

- 全局 registry 当前模型。
- auth/channel 的 provider、prefix、alias、excluded models。
- OAuth alias 与 API key alias 反向映射。
- channel group、path route、API Key 权限。
- 路由注册表或后端显式维护的路径族定义。
- 各路径作用域下的 `/models` 返回结果。
- `manage/channel-groups` 中系统默认根路径与用户自定义 path-routes 的规范化路径清单。
- AMP model mapping 与 provider alias 路径。

## 10. 验收口径

后续实现完成后，需要至少验证：

1. `manage/system` 展示结果和默认根路径 `GET /v1/models` 返回模型一致。
2. `manage/system` 不再展示只能通过自定义路径或 AMP provider 路径调用的模型。
3. `manage/system` 的提示说明清楚表达“这里展示的是 GET /v1/models，不是完整模型清单”。
4. `manage/models` 中同一个模型 ID 可以看到多个路径 badge。
5. `manage/models` 能区分 alias 与原始模型，并能展示 alias 对应上游模型。
6. 生图模型能显示 `/v1/images/generations` 与 `/v1/images/edits` 路径。
7. chat/completions/responses/messages/v1beta 等路径族不会互相覆盖。
8. channel group path-routes 下的模型能显示对应自定义路径。
9. API Key/channel group 限制不会被误展示成全局默认路径可用。
10. ccswitch 导出配置中的 `request-model` 与后端模型 alias 不再混为一谈。
11. 对每个配置了 path-routes 的路径，`GET {path}/v1/models` 返回的模型能够在 `manage/models` 对应路径列中体现。
12. `manage/channel-groups` 能看到只读的系统默认根路径 `/`。
13. 新增或编辑 channel group 路径时，不能保存与系统默认根路径、已有自定义路径或保留系统入口冲突的 path。
14. `manage/channel-groups` 不展示模型数量列，改为展示路径可用能力标签，并通过 tooltip 展示完整接口路径。
15. 新增 channel group 时分组名称和访问路径都必须填写，访问路径不能由系统静默生成。
