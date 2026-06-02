# Relay Monitor 系统重设计

这不是“把几个页面补全”或者“把几个 bug 修掉”的问题。`relay-monitor` 现在同时承担四个角色：

1. 站点资产台账
2. 模型质量巡检器
3. 智能路由代理
4. 运维解释面板

当前实现把这四件事堆在一个二进制里，但没有定义统一语义，所以用户看到的是一堆彼此冲突的真相：

- 模型页说“它可用”
- Proxy 页说“它会被选”
- 指纹页说“它没测”
- 实际请求又走到了另一个站

这不是展示问题，是系统边界和状态模型没立住。

## 1. 目标重述

这个系统的真正目标不是“展示很多站点和模型”，而是下面这句话：

> 用一个统一入口，把一组不稳定、能力不一致、质量参差不齐的 OpenAI 兼容上游，变成一个可解释、可干预、可恢复的稳定服务。

围绕这个目标，系统必须同时满足四个判断：

1. 对运维者：我能知道某个站现在是否应该被路由。
2. 对使用者：我发一个请求，系统能尽量给我成功响应，而不是沉默超时。
3. 对排障者：我能解释为什么这次请求选了 A 没选 B，为什么失败后没切，或者为什么切了还失败。
4. 对实现者：页面展示、数据库快照、路由决策，必须引用同一套状态定义。

如果这四件事不能同时成立，系统就还没做成。

## 2. 当前实现的硬伤

### 2.1 真相源分裂

当前系统至少有五套“当前状态”：

- `providers.json` 里的站点配置
- 内存里的 `s.providers`
- `providers` 表里的站点状态
- `current_results` 里的当前模型快照
- `proxy.Stats` / `proxy.Breakers` 里的内存运行态

这些状态没有统一生命周期，也没有统一版本号。于是你会得到这种荒唐结果：

- 页面改了权重，proxy 立刻变了，但数据库不一定知道为什么。
- 服务重启后，流量统计和 breaker 全丢，但页面没告诉你这是“冷启动后的新观察窗口”。
- provider 详情页会混合“实时拉模型列表”和“上次快照结果”，把“现在有这个模型”和“上次测过这个模型”混在一起。

### 2.2 页面语义不统一

现在页面里至少有四种概念，但没有被明确区分：

- 可见：模型页能看到
- 可用：巡检曾经成功
- 可路由：proxy 此刻会考虑
- 首选：给定请求形状下 proxy 当前优先选它

这四个词在产品里必须是四个不同状态。现在它们被表格凑在一起显示，用户只能靠猜。

### 2.3 路由学习粒度过粗

当前 proxy 的统计和 breaker 都按 `(provider, model)` 记账。这在路由上是错的。

实际请求至少还有这些维度：

- `endpoint`: `chat` / `responses`
- `stream`
- `tools`
- `tool_call_required`

如果一个 provider 的 `chat` 没问题、`responses + tools + stream` 总挂，当前实现会把这些失败全混进同一个错误率和 breaker 里。结果不是“智能切换”，而是“错误学习污染了别的请求形状”。

这个问题如果不改，系统只会越来越玄学。

### 2.4 “检查任务”被错误建模成一个布尔值

现在服务只有一个 `isChecking`。这意味着：

- 全量巡检、单站巡检、指纹检测、能力探测，本质上是不同作业，却被当成一个互斥锁。
- UI 只能表达“有任务在跑/没在跑”，不能表达“谁在跑、跑到哪、哪些结果已可见、哪些结果还只是临时证据”。
- 后续如果你想加“重跑某个模型的 capability”“只重算某个 provider 的指纹”“后台低优先级刷新余额”，会直接撞墙。

### 2.5 页面交互没有闭环

当前系统更多是在“展示数据”，不是“支持用户完成任务”。几个典型断点：

1. 用户新增 provider 后，不知道它现在是“已保存但未验证”，还是“验证失败但可继续观察”，还是“已经可以进路由”。
2. 用户在模型页看到某 provider 可用，但不知道为什么 proxy 不选它。
3. 用户看到请求失败，不知道是没有候选、候选都失败、流开始后协议异常、还是 breaker 开了。
4. 用户调整权重后，没有一个地方能验证“这次改动对 `gpt-5.4 responses + tools` 的实际选路影响是什么”。

### 2.6 安全面和产品目标冲突

这个系统自称“一个 Key 统一对外”，但当前模型页仍然围绕“直接暴露上游 provider 配置片段”设计。这个方向本身就和产品目标冲突：

- 如果 relay 是统一入口，下游配置默认就应该复制 relay 的 endpoint 和 relay key，而不是 provider 原始 key。
- 直接把 provider key 送到浏览器，本质上是在拆自己的统一代理价值。

这块不只是安全风险，还是产品定位摇摆。

## 3. 正确的产品模型

系统应该围绕六个核心对象设计，而不是围绕页面设计。

### 3.1 Provider

一个上游站点的静态配置和运营属性。

关键字段：

- `id`
- `name`
- `base_url`
- `declared_format`
- `api_key_ref`
- `access_token_ref`
- `priority_override`
- `operator_state`: `enabled | disabled | quarantined`
- `note`

### 3.2 Provider Snapshot

某次探测后得到的 provider 级状态。

关键字段：

- `provider_id`
- `snapshot_at`
- `connectivity_status`
- `models_fetch_status`
- `models_found`
- `health_score`
- `last_error`
- `balance`
- `platform`
- `source_run_id`

### 3.3 Model Availability

某个 provider 上某个模型当前是否可用，这是路由的底表。

关键字段：

- `provider_id`
- `model`
- `vendor`
- `availability_status`: `ok | degraded | failed | unknown`
- `correct`
- `latency_ms`
- `evidence_run_id`
- `evidence_at`

### 3.4 Capability Evidence

某个模型在某 provider 上，针对不同请求形状的能力证据。

关键字段：

- `provider_id`
- `model`
- `endpoint`
- `streaming`
- `tools`
- `tool_call_required`
- `verdict`: `supported | unsupported | unknown`
- `probe_kind`: `smoke | exact | inferred`
- `tested_at`
- `raw_evidence`

注意：这里不能再只放几个布尔值。能力判断本质上是“对特定请求形状的证据”，不是永恒事实。

### 3.5 Routing Decision

proxy 在收到一个请求时做出的决策解释对象。

关键字段：

- `trace_id`
- `model`
- `request_shape`
- `candidate_set`
- `selected_provider_id`
- `selected_reason`
- `failover_count`
- `final_status`
- `client_visible_error`

### 3.6 Job Run

所有后台任务都必须纳入同一套作业模型。

关键字段：

- `run_id`
- `run_type`: `full_check | quick_check | single_check | fingerprint | capability_refresh | balance_refresh`
- `scope`
- `status`: `queued | running | partial | completed | failed | cancelled`
- `progress_total`
- `progress_done`
- `started_at`
- `ended_at`
- `summary`

## 4. 交互设计

下面不是“页面长什么样”，而是“用户怎么完成任务”。

### 4.1 任务一：新增一个 Provider，并知道它何时可路由

#### 用户目标

我新增一个站点后，系统必须明确告诉我：

1. 站点是否保存成功
2. 认证是否通过
3. 模型列表是否拿到
4. 哪些核心模型完成了 smoke check
5. 是否已进入路由
6. 如果没进入，是因为哪一步卡住

#### 推荐交互

新增页提交后，不要直接“保存然后后台随缘跑”。应该进入一个明确的 onboarding 面板：

1. `配置校验中`
2. `认证校验中`
3. `模型索引中`
4. `核心模型冒烟检测中`
5. `能力探测中`
6. `已进入路由 / 已隔离等待人工处理`

#### 用户可见反馈

- 顶部状态条：`已保存，但未入路由`
- 分步结果：
  - Base URL 有效
  - API Key 认证成功
  - 获取到 126 个模型
  - 核心 smoke 成功 5/6
  - `responses + tools + stream` 未验证
- CTA：
  - `保存为隔离站点`
  - `立即加入路由`
  - `查看失败详情`

#### 后端动作

1. 保存 provider 静态配置
2. 创建 `job_run`
3. 逐步写入 provider snapshot 和 model availability
4. 如果 smoke 不通过，默认 `operator_state = quarantined`
5. 只有满足最小可用条件时才进 proxy routing set

这一步的关键是：**“已保存”不等于“已可路由”**。当前实现把这两件事混了。

### 4.2 任务二：看懂当前系统健康度

Dashboard 不该只是 provider 卡片墙。它应该先回答四个运维问题：

1. 现在有多少 provider 可配置
2. 其中多少 provider 可路由
3. 当前 proxy 成功率如何
4. 哪些 provider 正在拖累系统

#### Dashboard 推荐布局

1. 顶部摘要
   - 总 provider 数
   - 可路由 provider 数
   - 可路由模型数
   - 最近 10 分钟 proxy 成功率
   - 待处理异常数

2. 运行面板
   - 当前运行中的 job
   - 最近完成的 job
   - 失败的 job

3. 风险列表
   - 被隔离 provider
   - breaker 打开的 provider-model
   - 最近错误率暴涨的 provider

4. Provider 表
   - 名称
   - 状态
   - 路由状态
   - 最后权威快照时间
   - 最近 10 分钟请求数
   - 最近 10 分钟错误率
   - 手动权重
   - 操作入口

#### 这里必须新增的语义标签

- `Configured`: 已保存配置
- `Verified`: 已完成基本连通性验证
- `Routable`: 已满足进入路由条件
- `Preferred`: 当前某些关键模型的首选
- `Quarantined`: 人工或系统隔离

### 4.3 任务三：用户问“为什么这个模型没走到我预期的站”

这是系统最关键的交互。必须给一个“路由解释器”。

#### 路由解释页必须回答

给定：

- 模型：`gpt-5.4`
- endpoint：`responses`
- `stream=true`
- `tools=true`
- `tool_call_required=true`

系统应展示：

1. 参与候选的 provider 列表
2. 被过滤掉的 provider 列表
3. 每个 provider 的过滤原因
4. 剩余 provider 的分数构成
5. 最终选中者
6. 如果 failover，失败链条是什么

#### 每个候选至少显示这些字段

- `static_score`
- `priority_override`
- `fingerprint_score`
- `live_error_penalty`
- `breaker_state`
- `request_shape_match`
- `capability_evidence_age`
- `last_3_failures`
- `final_rank`

#### 过滤原因必须可读

不是“rank=3”。而是：

- `filtered: responses tool_call unsupported`
- `degraded: streaming capability unknown`
- `degraded: breaker half-open`
- `filtered: provider quarantined`

用户不该读源码猜路由。

### 4.4 任务四：请求失败后做归因

当前 proxy 页更多是统计页，不是事故页。真正需要的是“请求轨迹”。

#### 用户目标

OpenClaw / Codex / 任意客户端报错后，我能拿一个 `trace_id` 或时间窗口，看到：

1. 请求打到了哪个模型
2. 初始候选是谁
3. 为何失败
4. 有没有切
5. 切了几次
6. 最终错误是上游错误还是 proxy 自己拦截的协议错误

#### 请求轨迹对象

`request_trace` 至少应包含：

- `trace_id`
- `received_at`
- `client_model`
- `request_shape`
- `attempts[]`
- `final_provider`
- `final_status`
- `response_started`
- `client_error_message`

`attempt` 至少应包含：

- `provider_id`
- `attempt_index`
- `selected_score`
- `match_rank`
- `breaker_state_before`
- `upstream_status_code`
- `failure_class`
- `latency_ms`
- `wrote_to_client`

没有这层，所谓“智能切换”就永远只能靠日志猜。

## 5. 页面与信息架构重组

### 5.1 Dashboard

定位：系统概览 + 风险入口，不承担深度排障。

必须保留：

- 全局运行状态
- 风险摘要
- provider 列表

不该塞进 Dashboard 的内容：

- 模型明细
- 指纹细节
- 路由细节

### 5.2 Provider 详情页

定位：单站点运维面板。

应该拆成四块：

1. `站点概况`
   - 基础配置
   - 认证状态
   - 平台识别
   - 余额
   - 手动权重
   - 路由状态

2. `当前模型快照`
   - 这是权威快照，不要掺 live fetch

3. `能力证据`
   - chat / responses
   - stream / tools / required tool call
   - 测试时间
   - 证据来源

4. `近期请求表现`
   - 最近 10 分钟命中数
   - 错误率
   - breaker 开启的模型

当前 provider 页把“实时拉模型列表”混入快照，这是坏设计。运维页要看的是“可追溯快照”，不是“此刻临时扫到什么”。

### 5.3 模型页

定位：按模型看覆盖和质量，不负责解释实时路由。

这里必须明确三层视角：

1. `Coverage`: 哪些 provider 声称/经验证提供这个模型
2. `Quality`: 哪些 provider 的这个模型质量更好
3. `Routing`: 对特定请求形状，当前谁会被选

现在的模型页把 1 和 2 混了，还拿一些 capability 字段硬凑 3，这是半吊子。

#### 模型页的推荐操作

- 查看覆盖
- 切换“只看可路由”
- 进入“路由解释器”
- 查看指纹历史

不要再让模型页承担直接复制 provider 原始 key 的职责。默认动作应该是“复制 relay 配置”。

### 5.4 指纹页

定位：模型真实性证据库，不是单独的孤岛页面。

应该支持：

- 按 provider 看
- 按 model 看
- 按 vendor 看
- 看到为什么这次没测到

当前“未抽样/未运行”两个词太粗。至少要分成：

- `未进入当前采样策略`
- `已排队未执行`
- `执行失败`
- `已有过期结果`
- `当前结果有效`

### 5.5 新增页面：运行中心

这是当前系统最缺的一页。

作用：

- 展示所有 `job_run`
- 区分全量巡检、单站巡检、指纹、能力探测
- 展示每个 job 的范围、进度、耗时、失败项
- 支持取消和重试

没有运行中心，系统所有后台任务都只能靠一条 SSE 文本消息糊弄。

### 5.6 新增页面：路由实验室

这个页面专门解决“为什么不选它”。

输入：

- model
- endpoint
- stream
- tools
- tool_call_required

输出：

- 过滤前总候选
- 过滤后候选
- 每个候选的解释
- 如果现在真实发请求，大概率会选谁

这个页面应服务真实运维动作，而不是只给 debug 接口返回一堆 JSON。

## 6. 状态机设计

### 6.1 Provider 状态机

拆成三层，不要再用一个 `status` 字段乱装。

#### 配置态

- `enabled`
- `disabled`
- `quarantined`

#### 连通态

- `unknown`
- `auth_failed`
- `reachable`
- `models_fetch_failed`
- `degraded`

#### 路由态

- `not_eligible`
- `eligible`
- `cooling_down`
- `active`

页面最终展示的是这三层聚合后的 badge，而不是数据库里某个偷懒字段。

### 6.2 Model Availability 状态机

- `unknown`
- `indexed_only`
- `smoke_ok`
- `smoke_failed`
- `verified`
- `stale`

只有 `verified` 才允许进入关键请求的优先候选。

### 6.3 Job 状态机

- `queued`
- `running`
- `partial`
- `completed`
- `failed`
- `cancelled`

### 6.4 Request Trace 状态机

- `received`
- `candidate_selected`
- `attempting`
- `failed_over`
- `streaming`
- `completed`
- `failed`

## 7. 接口设计

下面的接口不是“建议有空加一下”，而是支撑上面交互的最小集。

### 7.1 Provider 管理

#### `POST /api/v1/providers/preflight`

用途：新增 provider 前先做配置校验，不直接落库。

请求：

```json
{
  "name": "uniproxy",
  "base_url": "https://api.example.com/v1",
  "api_key": "sk-xxx",
  "declared_format": "chat"
}
```

响应：

```json
{
  "ok": true,
  "checks": {
    "url": "ok",
    "auth": "ok",
    "models": "ok"
  },
  "models_found": 126,
  "warnings": [
    "responses support not yet verified"
  ]
}
```

#### `POST /api/v1/providers`

用途：落库并创建 onboarding job。

响应必须带：

- `provider_id`
- `run_id`
- `initial_state`

#### `PATCH /api/v1/providers/{id}`

允许修改：

- `priority_override`
- `operator_state`
- `note`

这里不要继续用表单 POST 拼字段，应该明确字段级更新。

### 7.2 作业系统

#### `POST /api/v1/jobs`

请求：

```json
{
  "type": "full_check",
  "scope": {
    "provider_ids": [12, 18]
  }
}
```

#### `GET /api/v1/jobs`

返回运行中、最近完成、失败任务。

#### `GET /api/v1/jobs/{run_id}`

返回：

- 总进度
- 每个 provider 子任务
- 错误详情
- 产出摘要

### 7.3 模型与能力

#### `GET /api/v1/models`

查询参数：

- `scope=all|routable|verified`
- `vendor`
- `model`

每个模型项返回三组字段：

- `coverage`
- `quality`
- `routing_summary`

#### `GET /api/v1/models/{model}/providers`

每个 provider 返回：

- 当前 availability
- capability evidence
- fingerprint summary
- routing eligibility

### 7.4 路由解释

#### `POST /api/v1/routing/explain`

请求：

```json
{
  "model": "gpt-5.4",
  "endpoint": "responses",
  "stream": true,
  "tools": true,
  "tool_call_required": true
}
```

响应：

```json
{
  "request_shape": {
    "endpoint": "responses",
    "stream": true,
    "tools": true,
    "tool_call_required": true
  },
  "eligible_candidates": [],
  "filtered_candidates": [],
  "selected_candidate": null
}
```

### 7.5 请求轨迹

#### `GET /api/v1/proxy/requests`

支持按时间、模型、provider、结果过滤。

#### `GET /api/v1/proxy/requests/{trace_id}`

返回完整 failover 链。

这两个接口会直接改变排障效率，不是锦上添花。

## 8. 数据库设计

当前 schema 可以复用一部分，但必须重组。

### 8.1 保留并调整

#### `providers`

保留，但应新增：

- `operator_state`
- `priority_override`
- `note`
- `declared_format`
- `last_verified_at`

#### `job_runs`

替代当前 `check_runs`，支持所有后台任务。

关键字段：

- `id`
- `type`
- `scope_json`
- `status`
- `progress_total`
- `progress_done`
- `summary`
- `started_at`
- `ended_at`

#### `provider_model_current`

替代 `current_results` 的语义化表名。

#### `provider_model_history`

替代 `check_results` 的历史语义。

### 8.2 必须新增

#### `capability_evidence`

不能只是一行几个布尔值。建议主键：

- `provider_id`
- `model`
- `endpoint`
- `stream`
- `tools`
- `tool_call_required`

关键字段：

- `verdict`
- `evidence_type`
- `tested_at`
- `raw_excerpt`

#### `fingerprint_current`

存当前生效结果，避免每次都从历史里 `MAX(id)` 推断。

#### `routing_overrides`

用于存：

- 权重覆盖
- 黑名单
- 白名单
- 固定 provider

#### `proxy_request_traces`

记录每个真实代理请求。

#### `proxy_attempts`

记录每次 failover attempt。

### 8.3 可以暂缓

#### `provider_secrets`

如果短期仍然用 `providers.json` 持有密钥，可以暂缓单独建表。但要把这个决定写清楚：

- Phase 1：文件保存 secrets，DB 保存元数据
- Phase 2：迁到 DB 或外部 secret store

不要继续保持现在这种“内存切片 + JSON 文件 + DB 读一半”的半吊子状态。

## 9. 数据流设计

### 9.1 新增 Provider

`表单 -> preflight -> 保存 provider -> 创建 onboarding job -> 产出 snapshot/capability/current -> routing table rebuild -> UI 订阅 job 进度`

### 9.2 全量巡检

`创建 full_check job -> provider 并发执行 -> 写 history -> 汇总 authoritative snapshot -> 更新 current -> 刷新 routing evidence -> publish SSE job update`

### 9.3 指纹检测

`创建 fingerprint job -> 选样 -> 写 fingerprint_history -> 更新 fingerprint_current -> 触发 routing score rebuild`

### 9.4 实时代理请求

`client request -> normalize request shape -> routing explain object -> attempt 1 -> 失败分类 -> stats / breaker / trace 写入 -> failover -> final response`

这里最关键的一条原则：

> 真实代理请求的观测证据，必须回流到“路由解释”和“事故排查”页面，而不是只打日志。

## 10. 实现边界与重构建议

### Phase 1：先统一语义，不先卷 UI

先做：

1. 把后台作业统一成 `job_runs`
2. 把 provider 当前态、模型当前态、capability 当前态明确分层
3. 把 proxy stats / breaker 的 key 扩展到请求形状
4. 做 `routing/explain` 接口

不先做这个，UI 再漂亮也只是帮用户更快地看见矛盾。

### Phase 2：补观测闭环

做：

1. 请求轨迹
2. 运行中心
3. provider 页改成只看权威快照
4. 模型页增加 scope 切换

### Phase 3：再做高级策略

例如：

- 厂商级 fallback
- 优先走低成本路由
- 按客户端类型定制路由策略
- 基于历史成功率的自适应权重

这些都必须建立在“前两阶段已经把状态定义做干净”的前提上。

## 11. 对当前代码的落地审查

下面是不留情面的结论。

### 11.1 现在最该先改的，不是样式，不是表格，是状态模型

如果继续在当前结构上追加功能，你只会得到更多“页面看着齐全，但解释不了”的假完成。

### 11.2 Proxy 不是缺一个算法，而是缺一个证据层

现在的路由逻辑已经有分数、错误率、breaker、能力过滤，但缺的是：

- 请求形状维度
- 失败链条记录
- 对外解释对象

没有这三层，再好的算法都只能靠日志考古。

### 11.3 你的 UI 现在是在暴露内部实现，不是在服务用户任务

用户需要的是：

- “为什么没选它”
- “这个站还能不能用”
- “我刚改的权重有没有生效”

不是：

- 一堆离散页面
- 一堆布尔字段
- 一堆要靠 README 理解的语义差别

### 11.4 这个系统是能做成的，但必须收敛边界

能复用的：

- 单二进制架构
- SQLite
- 当前 checker / proxy 基础能力
- HTMX + template 的轻前端方案

必须重构的：

- 作业系统
- 当前态与历史态建模
- proxy telemetry key
- route explain / request trace
- provider 和 model 页面语义

## 12. 下一步的正确顺序

不是继续东一块西一块补。

正确顺序应该是：

1. 重构数据语义和作业系统
2. 让 proxy 决策和 UI 引用同一组证据
3. 做路由解释和请求轨迹
4. 再重做 Dashboard / Provider / Models 三个页面

如果顺序反过来，后面改一次状态模型，前面所有页面都得重写。
