# 调度器与渠道额度检查现状评审

## 1. 评审范围

这份文档只回答 4 个问题：

1. 当前调度器在请求失败时，是否会自动切到别的渠道/凭证。
2. 如果对应渠道额度耗尽，是否会自动跳过该渠道。
3. 管理页上的额度显示，是否会因为检测到额度耗尽而自动更新。
4. 管理页上的调用次数统计，是否已经有完整链路。

本次结论基于以下代码路径交叉验证：

- 调度与失败切换：
  - `sdk/cliproxy/auth/conductor.go`
  - `sdk/cliproxy/auth/selector.go`
- 额度耗尽标记与恢复：
  - `sdk/cliproxy/auth/quota_reconcile.go`
  - `internal/runtime/executor/codex_executor.go`
  - `internal/registry/model_registry.go`
- 管理端 auth file 列表：
  - `internal/api/handlers/management/auth_files.go`
  - `internal/api/handlers/management/quota.go`
- 管理页展示：
  - `codeProxy/src/modules/auth-files/AuthFilesPage.tsx`
  - `codeProxy/src/modules/quota/quota-fetch.ts`
  - `codeProxy/src/lib/http/apis/quota.ts`
- 调用次数统计：
  - `internal/usage/logger_plugin.go`
  - `internal/usage/usage_db.go`
  - `internal/api/handlers/management/usage_logs_handler.go`

---

## 2. 先给结论

### 2.1 是否会自动切到别的渠道

**会，但要精确定义“渠道”。**

当前运行时不是简单按“渠道名称列表顺序”去切，而是：

1. 先筛出可用的 auth 候选。
2. 再按 `priority` 选出最高优先级的一组。
3. 然后根据路由策略决定怎么挑：
   - `round-robin`
   - `fill-first`
4. 当前 auth 执行失败后，只要错误不是“请求本身无效”，就继续尝试下一个未试过的 auth。

所以它的行为更准确地说是：

- **会自动 failover 到其它可用凭证/auth**
- **不等于按你在管理页看到的“渠道名”做固定顺序切换**
- **也不等于按配置文件里的书写顺序切换**

### 2.2 codex 额度用完后是否自动跳过

**有。**

当上游返回 `429` 时，运行时会把该 auth 或该 model state 标记为：

- `Unavailable = true`
- `Quota.Exceeded = true`
- `NextRetryAfter / NextRecoverAt = 某个恢复时间`

后续调度时，选择器会把这类还在 cooldown 的 auth 直接过滤掉。

### 2.3 检查到额度用完后，额度显示是否自动更新

**前端管理页有自动刷新逻辑，但它和运行时调度不是同一条链路。**

现在的额度显示不是后端在 `GET /auth-files` 里直接返回的，而是管理页自己再发一次额度探测请求去拿：

- Codex: `https://chatgpt.com/backend-api/wham/usage`
- Gemini CLI / Antigravity / Kiro 各自也有独立探测接口

管理页会：

1. 进入页面后自动为当前页支持额度查询的 auth 拉额度。
2. 按设置的自动刷新间隔继续轮询当前页额度。
3. 手动点击单个 auth 的刷新按钮也会重新拉。
4. 拉到新额度后，会顺手调一次后端 `/quota/reconcile`，把后端 runtime 的 quota cooldown 状态同步一下。

所以答案是：

- **会更新**
- **但更新来源是前端主动探测，不是后端调度器被动推送**
- **而且只会刷新当前页可见的 auth，不是全局所有 auth**

### 2.4 调度器逻辑和额度检查逻辑是否都有

**都有，但完整度不一样。**

- 调度器失败切换：**有**
- 429 后自动跳过：**有**
- quota 恢复探测：**有**
- 管理页额度展示刷新：**有**
- 调用次数统计展示：**有**
- `quota-exceeded.switch-project` / `quota-exceeded.switch-preview-model` 这两个“配额超出策略开关”真正接入运行时：**目前看没有**

这一点是这次评审里最重要的缺口。

---

## 3. 调度器真实行为

## 3.1 候选 auth 如何选出来

`pickNextMixed()` 会从内存中的全部 auth 里筛候选，筛选条件包括：

- provider 在允许集合内
- auth 没被手动禁用
- 没被当前请求的 `tried` 集合用过
- 如果请求限制了 channel，则只保留允许的 channel
- 如果请求带了 model，则只保留支持该 model 的 auth

关键代码：

- `sdk/cliproxy/auth/conductor.go:1861`
- `sdk/cliproxy/auth/conductor.go:1888`
- `sdk/cliproxy/auth/conductor.go:1911`
- `sdk/cliproxy/auth/conductor.go:1920`

这说明它不是“拿到一个渠道后一直硬打”，而是每次失败后重新从剩余候选里再挑。

## 3.2 候选 auth 的选择顺序

真正的选择顺序由 `selector.go` 控制：

1. 先按 `priority` 分组。
2. 只取最高优先级那组。
3. 同优先级内再走：
   - `RoundRobinSelector`
   - `FillFirstSelector`

关键代码：

- `sdk/cliproxy/auth/selector.go:194`
- `sdk/cliproxy/auth/selector.go:235`
- `sdk/cliproxy/auth/selector.go:255`
- `sdk/cliproxy/auth/selector.go:354`

这里有两个很关键的细节：

### 细节 A：不是按“渠道名”顺序切

`ChannelName()` 主要是给管理页展示和 channel restriction 用的，不是调度顺序的直接依据。

关键代码：

- `sdk/cliproxy/auth/types.go:397`

### 细节 B：不是按 auth 文件上传顺序切

同优先级 auth 会先按 `ID` 排序，然后：

- `round-robin` 轮询
- `fill-first` 固定拿第一个可用的

关键代码：

- `sdk/cliproxy/auth/selector.go:244`
- `sdk/cliproxy/auth/selector.go:246`
- `sdk/cliproxy/auth/selector.go:305`
- `sdk/cliproxy/auth/selector.go:361`

所以如果你问“是不是按顺序切换”，答案只能说：

- **会切换**
- **但顺序是 priority + selector 决定的，不是渠道名顺序**

## 3.3 请求失败后会不会继续试下一个

会。

`executeMixedOnce()` / `executeCountMixedOnce()` / `executeStreamMixedOnce()` 的结构都是：

1. 选一个 auth
2. 执行
3. 如果失败：
   - 记录结果
   - 如果是请求本身无效，则直接返回
   - 否则继续选下一个没试过的 auth

关键代码：

- `sdk/cliproxy/auth/conductor.go:643`
- `sdk/cliproxy/auth/conductor.go:652`
- `sdk/cliproxy/auth/conductor.go:674`
- `sdk/cliproxy/auth/conductor.go:687`
- `sdk/cliproxy/auth/conductor.go:688`
- `sdk/cliproxy/auth/conductor.go:691`
- `sdk/cliproxy/auth/conductor.go:699`
- `sdk/cliproxy/auth/conductor.go:755`

特别要注意这一行语义：

- `isRequestInvalidError(errExec)` 为真时，不会切 auth，因为它认为换 auth 也救不了。

关键代码：

- `sdk/cliproxy/auth/conductor.go:1592`

---

## 4. 额度耗尽后的自动跳过

## 4.1 429 会发生什么

运行时在 `MarkResult()` 里根据 HTTP 状态码更新 auth/model 状态。

对 `429` 的处理是最关键的一段：

- 设置 `NextRetryAfter`
- 设置 `Quota.Exceeded = true`
- 设置 `Quota.NextRecoverAt`
- 标记 `suspendReason = "quota"`
- 标记需要把 registry 的 model quota 状态也同步出去

关键代码：

- `sdk/cliproxy/auth/conductor.go:1356`
- `sdk/cliproxy/auth/conductor.go:1368`
- `sdk/cliproxy/auth/conductor.go:1369`
- `sdk/cliproxy/auth/conductor.go:1375`
- `sdk/cliproxy/auth/conductor.go:1377`

如果没有 model 级状态，则 auth 级别也会被打上 quota exhausted：

- `sdk/cliproxy/auth/conductor.go:1607`
- `sdk/cliproxy/auth/conductor.go:1631`
- `sdk/cliproxy/auth/conductor.go:1645`

## 4.2 后续调度怎么跳过它

选择器在 `isAuthBlockedForModel()` 里会检查：

- `Disabled`
- `StatusDisabled`
- `state.Unavailable`
- `state.NextRetryAfter`
- `state.Quota.Exceeded`
- `auth.Unavailable`
- `auth.NextRetryAfter`
- `auth.Quota.Exceeded`

如果还在 cooldown 窗口内，就直接视为 blocked。

关键代码：

- `sdk/cliproxy/auth/selector.go:365`
- `sdk/cliproxy/auth/selector.go:381`
- `sdk/cliproxy/auth/selector.go:385`
- `sdk/cliproxy/auth/selector.go:389`
- `sdk/cliproxy/auth/selector.go:397`
- `sdk/cliproxy/auth/selector.go:408`
- `sdk/cliproxy/auth/selector.go:416`

这意味着：

- **codex 某个 auth 一旦因为 429 被打进 cooldown，下一轮选择就会跳过它**

## 4.3 这个跳过是 model 级还是 auth 级

两层都有：

- 优先是 **model 级**
- 聚合后也会影响 **auth 级**

聚合逻辑在 `updateAggregatedAvailability()`：

- 如果所有 model 都不可用，则 auth 也会变 `Unavailable`
- 如果任一 model quota exceeded，会把 auth.Quota 也聚合出来

关键代码：

- `sdk/cliproxy/auth/conductor.go:1444`
- `sdk/cliproxy/auth/conductor.go:1486`
- `sdk/cliproxy/auth/conductor.go:1492`

## 4.4 registry 也会同步 quota 状态

`MarkResult()` 在 429 时不仅更新 auth 内存状态，还会同步 registry：

- `SetModelQuotaExceeded`
- `SuspendClientModel`

成功恢复时会：

- `ClearModelQuotaExceeded`
- `ResumeClientModel`

关键代码：

- `sdk/cliproxy/auth/conductor.go:1401`
- `sdk/cliproxy/auth/conductor.go:1404`
- `sdk/cliproxy/auth/conductor.go:1407`
- `sdk/cliproxy/auth/conductor.go:1409`
- `internal/registry/model_registry.go:590`
- `internal/registry/model_registry.go:604`
- `internal/registry/model_registry.go:618`
- `internal/registry/model_registry.go:649`

---

## 5. 额度恢复检查是否也有

## 5.1 有后台 quota recovery probe

auth manager 在自动 refresh loop 里，除了刷新 token，也会检查 quota recoveries：

- `checkRefreshes()` 最后会调用 `checkQuotaRecoveries()`

关键代码：

- `sdk/cliproxy/auth/conductor.go:2004`
- `sdk/cliproxy/auth/conductor.go:2025`

`checkQuotaRecoveries()` 只会对满足以下条件的 auth 发 probe：

- 当前真的有 quota cooldown
- executor 支持 `QuotaRecoveryProber`

关键代码：

- `sdk/cliproxy/auth/quota_reconcile.go:43`
- `sdk/cliproxy/auth/quota_reconcile.go:52`
- `sdk/cliproxy/auth/quota_reconcile.go:63`

## 5.2 Codex 的 probe 实现是有的

Codex executor 实现了 `ProbeQuotaRecovery()`，直接去打：

- `GET https://chatgpt.com/backend-api/wham/usage`

然后从返回体里解析：

- `allowed`
- `limit_reached`
- `primary_window`
- `secondary_window`
- `reset_at`
- `reset_after_seconds`

关键代码：

- `internal/runtime/executor/codex_executor.go:80`
- `internal/runtime/executor/codex_executor.go:94`
- `internal/runtime/executor/codex_executor.go:119`
- `internal/runtime/executor/codex_executor.go:760`
- `internal/runtime/executor/codex_executor.go:770`
- `internal/runtime/executor/codex_executor.go:776`
- `internal/runtime/executor/codex_executor.go:793`

## 5.3 Probe 后会不会真的改回可用

会。

`ReconcileQuota()` -> `probeQuotaRecovery()` -> `applyQuotaProbeResult()` 这条链路会：

- quota 恢复则清掉 model/auth 的 quota state
- 更新 `NextRecoverAt`
- 恢复 registry 的 quota/suspend 状态

关键代码：

- `sdk/cliproxy/auth/quota_reconcile.go:38`
- `sdk/cliproxy/auth/quota_reconcile.go:123`
- `sdk/cliproxy/auth/quota_reconcile.go:154`
- `sdk/cliproxy/auth/quota_reconcile.go:174`
- `sdk/cliproxy/auth/quota_reconcile.go:181`
- `sdk/cliproxy/auth/quota_reconcile.go:212`
- `sdk/cliproxy/auth/quota_reconcile.go:224`

所以“额度检查逻辑有没有”这个问题，答案是：

- **有**
- 而且不是纯前端展示，它会回写 runtime 状态

---

## 6. 管理页额度显示到底是不是自动更新

## 6.1 后端 `/auth-files` 不直接返回 live quota items

`ListAuthFiles()` 返回的是 `buildAuthFileEntry()` 组出来的对象。

当前会返回这些和状态相关的字段：

- `auth_index`
- `label`
- `status`
- `status_message`
- `disabled`
- `unavailable`
- `last_refresh`
- `next_retry_after`

关键代码：

- `internal/api/handlers/management/auth_files.go:252`
- `internal/api/handlers/management/auth_files.go:273`
- `internal/api/handlers/management/auth_files.go:375`
- `internal/api/handlers/management/auth_files.go:381`
- `internal/api/handlers/management/auth_files.go:382`
- `internal/api/handlers/management/auth_files.go:383`
- `internal/api/handlers/management/auth_files.go:384`
- `internal/api/handlers/management/auth_files.go:385`
- `internal/api/handlers/management/auth_files.go:408`
- `internal/api/handlers/management/auth_files.go:411`

但这里**没有直接把 quota item 列表塞进响应**。

这意味着：

- 管理页看到的具体百分比、重置时间，不是后端 auth 列表接口直接给的。

## 6.2 前端额度显示来自单独探测

`AuthFilesPage` 对当前页 auth 做了单独额度探测：

- `resolveQuotaProvider(file)` 决定是否支持
- `refreshQuota(file, provider)` 真正去拉额度
- 结果存进 `quotaByFileName`

关键代码：

- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:888`
- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:905`
- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:919`
- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:1112`
- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:1213`

探测实际由 `fetchQuota()` 完成：

- Codex 用 `CODEX_USAGE_URL`
- 通过 `authIndex` 指定使用哪一个 auth 去发请求

关键代码：

- `codeProxy/src/modules/quota/quota-fetch.ts:79`
- `codeProxy/src/modules/quota/quota-fetch.ts:83`
- `codeProxy/src/modules/quota/quota-fetch.ts:118`
- `codeProxy/src/modules/quota/quota-fetch.ts:121`
- `codeProxy/src/modules/quota/quota-fetch.ts:124`
- `codeProxy/src/modules/quota/quota-fetch.ts:131`

## 6.3 它是怎么“自动更新”的

当前页有两层自动行为：

### 第一层：页面进入后 warmup

打开 auth-files 页时，会为当前页的支持项自动拉一轮 quota。

关键代码：

- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:1112`
- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:1123`
- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:1151`

### 第二层：定时自动刷新

如果 `quotaAutoRefreshMs > 0`，页面会周期性调用 `refreshCurrentPageQuota()`。

关键代码：

- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:1179`
- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:1213`
- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:1217`

### 第三层：单条手工刷新

每个 auth 行/卡片都有刷新按钮。

关键代码：

- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:2219`
- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:2225`
- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:2696`

## 6.4 拉到额度后会不会同步后端状态

会。

前端每次 quota 探测成功后，会拿该 auth 的 `auth_index` 去调用：

- `POST /quota/reconcile`

关键代码：

- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:908`
- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:911`
- `codeProxy/src/lib/http/apis/quota.ts:3`
- `internal/api/handlers/management/quota.go:30`
- `internal/api/handlers/management/quota.go:49`

这个设计很关键：

- 前端 quota 探测不是纯展示
- 它还会反过来推动后端 runtime 的 quota 恢复/延长判断

## 6.5 但这里有两个边界

### 边界 A：只刷新当前页

当前页面分页大小是 9，quota 自动抓取的对象是 `pageItems`，不是全量 auth。

关键代码：

- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:53`
- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:1103`
- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:1116`
- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:1184`

### 边界 B：列表刷新和额度刷新不是一个按钮语义

页面顶部的刷新按钮 `loadAll()` 只会刷新：

- auth 文件列表
- usage entity stats

关键代码：

- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:978`
- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:984`
- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:986`
- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:2333`

它不会直接同步触发“全量 quota 刷新”；quota 依赖后面的 warmup / 定时轮询 / 单条刷新。

---

## 7. 调用次数统计是否有完整逻辑

## 7.1 前端展示逻辑是有的

`AuthFilesPage` 会取 `/usage/entity-stats`，再按：

- `auth_index`
- 或 `source`

把每个 auth 的成功/失败次数聚合出来。

关键代码：

- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:319`
- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:342`
- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:364`
- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:2065`
- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:2080`
- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:2093`

所以“卡片上/列表上显示已经调用的次数”这条展示逻辑是存在的。

## 7.2 后端统计接口也是有的

后端 `GetEntityUsageStats()` 会查询：

- `source`
- `auth_index`

然后返回给前端。

关键代码：

- `internal/api/handlers/management/usage_logs_handler.go:559`
- `internal/api/handlers/management/usage_logs_handler.go:564`
- `internal/api/handlers/management/usage_logs_handler.go:573`
- `internal/api/handlers/management/usage_logs_handler.go:582`

底层数据来自 `request_logs` 表：

- `QueryEntityStats()` 直接按 `request_logs` 聚合

关键代码：

- `internal/usage/usage_db.go:81`
- `internal/usage/usage_db.go:1124`
- `internal/usage/usage_db.go:1141`

## 7.3 这个统计是否受 `usage-statistics-enabled` 控制

**受。**

`LoggerPlugin.HandleUsage()` 和 `RequestStatistics.Record()` 都先检查 `statisticsEnabled`。

而 `InsertLog()` 也是在 `Record()` 里被调用的。

关键代码：

- `internal/usage/logger_plugin.go:56`
- `internal/usage/logger_plugin.go:57`
- `internal/usage/logger_plugin.go:281`
- `internal/usage/logger_plugin.go:286`
- `internal/usage/logger_plugin.go:342`
- `internal/usage/logger_plugin.go:351`

这意味着：

- `usage-statistics-enabled = false` 时
  - 不仅内存统计不更新
  - 连 `request_logs` 的落库也不会走到
  - 那么前端 `auth-files` 页看到的调用次数也不会继续增长

而且默认值就是 `false`：

- `internal/config/config.go:598`

## 7.4 调用次数是不是实时自动刷新

**不是像 quota 那样独立轮询。**

`usageData` 只在 `loadAll()` 时拉一次：

- 页面初始化
- 用户手工点顶部刷新

关键代码：

- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:978`
- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:1003`
- `codeProxy/src/modules/auth-files/AuthFilesPage.tsx:2333`

当前没有看到针对 `usageData` 的单独定时轮询。

所以现在的行为是：

- **调用次数统计链路有**
- **但不是像 quota 一样自动高频刷新**

---

## 8. 最容易误判的点

## 8.1 `quota-exceeded.switch-project` / `switch-preview-model` 现在看起来没有接到 runtime

这是本次评审最重要的结论之一。

我全局检索后，这两个配置项当前只出现在：

- 配置结构体
- management API
- TUI 配置面板
- 配置 diff

但**没有发现运行时调度/执行代码读取它们来决定切项目或切 preview model**。

关键代码：

- `internal/config/config.go:197`
- `internal/api/handlers/management/quota.go:10`
- `internal/api/server.go:553`
- `internal/tui/config_tab.go:346`
- `internal/watcher/diff/config_diff.go:74`

同时，检索 `SwitchProject` / `SwitchPreviewModel` 在 runtime 范围内没有命中实际消费代码。

所以当前更准确的结论应当是：

- **“配额超出策略配置项存在”**
- **不等于“运行时已实现切项目/切 preview model”**

## 8.2 真正已经实现的是“跳过 cooldown auth”，不是“切项目”

现在实际落地的是：

- 某 auth 429 了
- 标成 quota exhausted
- 调度器后续跳过这个 auth

这和“同一个 auth 内部自动换 project”是两件不同的事。

如果你原本预期的是：

- Codex 同一份 auth 内 quota 用完后自动切 project
- 或自动切 preview model

那么从目前代码看，**这部分并没有证据说明已经接通**。

---

## 9. 对你的问题逐条回答

## 9.1 “请求的渠道异常了，会自动切到别的渠道吧？”

**答：会切，但准确说是切到别的可用 auth/credential。**

不是简单按渠道名顺序硬切，而是：

- 先筛候选
- 再按 priority
- 再按 round-robin 或 fill-first
- 当前 auth 失败就继续试下一个

## 9.2 “按顺序切换？”

**答：不是你直觉里的‘渠道顺序’。**

当前顺序由：

- priority
- selector strategy
- auth ID 排序

共同决定。

## 9.3 “如果对应渠道，比如 codex 的额度用完了，是不是自动跳过该渠道？”

**答：是。**

429 后会把该 auth/model 打进 quota cooldown，选择器后续会跳过。

## 9.4 “如果检查到对应额度用完了，会自动更新对应额度显示吗？”

**答：管理页会自动更新，但靠的是前端主动探测。**

不是后端调度器直接把额度变化推到页面。

## 9.5 “调度器和对应渠道的额度检查逻辑是否都有？”

**答：都有，但不是一个完整闭环。**

已有：

- 调度器失败切换
- 429 后跳过
- quota recovery probe
- 管理页 quota 拉取与自动刷新
- 调用次数统计展示

缺口：

- `switch-project`
- `switch-preview-model`

这两个“配额超出策略”目前看只有配置面，没有 runtime 消费逻辑。

---

## 10. 建议你 review 时重点看什么

如果你要判断“这套系统是不是已经完整支持额度驱动的智能切换”，建议重点看 3 个判断标准：

### 标准 1：429 后能不能不再命中同一 auth

这一点当前是成立的。

### 标准 2：UI 上的 quota 百分比是不是 runtime 真状态

不完全是。

它是前端主动探测出的“当前视图”，然后再反向 reconcile 到后端。

### 标准 3：配置里的 quota strategy 开关是不是已经真正生效

当前结论是：

- **大概率没有**

---

## 11. 最终结论

一句话总结：

**当前系统已经具备“失败后切其它 auth + quota exhausted 后自动跳过 + 管理页主动探测并刷新 quota”的能力，但还不能证明“quota-exceeded.switch-project / switch-preview-model”这套策略已经真正接入运行时。**

如果你要的目标是：

- 429 后自动换其它 auth：**已有**
- codex 某 auth 额度耗尽后自动跳过：**已有**
- 管理页额度显示自动刷新：**已有，但靠前端探测**
- 管理页调用次数统计：**已有，但依赖 usage-statistics-enabled，且不是高频自动刷新**
- 配额超出后自动切项目 / 切 preview model：**目前未发现 runtime 接入证据**

