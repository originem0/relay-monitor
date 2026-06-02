# Relay Monitor 设计补全

本文档是 `system-redesign.md` 的落地补全。system-redesign 做了精准的诊断和方向设定，但很多地方点到为止。本文档把那些"需要你去补全"的部分展开到可以直接指导实现的程度。

结构：先补交互闭环（用户视角），再补数据模型（实现视角），最后补接口和迁移策略。

---

## 一、交互设计补全

system-redesign 定义了四个核心任务。下面逐个补全完整的用户行为路径、状态变化、反馈机制、异常分支。

### 1.1 新增 Provider：从"保存"到"可路由"的完整旅程

**当前问题**：保存即入库，入库即可路由。用户不知道新站点处于什么阶段。

**完整交互流**：

```
用户点击 "Add Provider"
  ↓
填写表单（name, base_url, api_key, format）
  ↓ 点击 "Validate"
preflight 请求（不落库）
  ↓ SSE 推送每步结果
┌─────────────────────────────────────────────┐
│  ✓ URL 格式有效                              │
│  ✓ TLS 连接成功                              │
│  ✓ API Key 认证通过                          │
│  ✓ /models 返回 126 个模型                   │
│  ⚠ /responses 未验证（将在 onboarding 中探测）│
└─────────────────────────────────────────────┘
  ↓ 用户点击 "Save & Onboard"
落库，operator_state = quarantined，创建 onboarding job
  ↓ 跳转到 Provider 详情页，onboarding 面板实时刷新
┌─────────────────────────────────────────────┐
│  Onboarding Progress                         │
│  ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━ 60%          │
│                                              │
│  [✓] 配置已保存                              │
│  [✓] 认证已验证                              │
│  [✓] 模型索引完成 (126 models)               │
│  [→] 核心模型冒烟测试 (3/6)                  │
│  [ ] 能力探测                                │
│  [ ] 路由资格评估                            │
│                                              │
│  实时日志：                                  │
│  gpt-5.4      OK   2.3s  correct            │
│  gpt-4.1-mini OK   1.1s  correct            │
│  claude-4-...  OK   3.5s  correct            │
│  o4-mini      testing...                     │
└─────────────────────────────────────────────┘
  ↓ onboarding job 完成
┌─────────────────────────────────────────────┐
│  Onboarding Complete                         │
│                                              │
│  Smoke: 5/6 passed                          │
│  Capabilities: chat ✓, responses ✓,         │
│    streaming ✓, tools ✓                     │
│                                              │
│  Recommendation: Ready for routing           │
│                                              │
│  [Enable Routing]  [Keep Quarantined]        │
└─────────────────────────────────────────────┘
```

**关键设计决策**：

1. preflight 不落库。用户可以反复测试不同 URL/Key 组合而不产生垃圾数据。
2. 保存后默认 quarantined。必须通过 onboarding 或手动操作才能进入路由。
3. onboarding 是一个真正的 job_run，有进度、可取消、结果可追溯。
4. onboarding 完成后给出明确建议（ready/not ready），并提供二选一操作。

**异常分支**：

| 异常 | 用户看到的 | 系统行为 |
|---|---|---|
| preflight 时 URL 不通 | "Connection failed: dial tcp: i/o timeout" | 不允许保存，除非用户勾选 "Save anyway" |
| preflight 时 auth 失败 | "Authentication failed: 401 Unauthorized" | 同上 |
| onboarding 中某模型超时 | 该模型标记 `smoke_failed`，不阻塞整体 | 继续测试其他模型 |
| onboarding 中全部模型失败 | 建议 "Keep Quarantined"，显示失败汇总 | `operator_state` 保持 quarantined |
| onboarding 被用户取消 | 已完成的结果保留，未完成的标记 unknown | job 状态变 cancelled |
| 保存时 name 重复 | 表单错误提示 "Provider name already exists" | 不落库 |

**SSE 事件协议**：

```
event: job_progress
data: {"run_id":"abc","step":"smoke","provider_id":12,"progress":3,"total":6,"detail":"gpt-4.1-mini OK 1.1s"}

event: job_step_complete
data: {"run_id":"abc","step":"smoke","passed":5,"failed":1}

event: job_complete
data: {"run_id":"abc","status":"completed","summary":{"smoke_pass":5,"smoke_fail":1,"capabilities_probed":4}}
```

### 1.2 Dashboard：回答四个运维问题

**当前问题**：Dashboard 是 provider 卡片墙，所有信息平铺，没有层次。

**设计原则**：Dashboard 是入口，不是详情。5 秒内回答"系统是否正常"，30 秒内定位到异常 provider。

**布局**（从上到下）：

```
┌─────────────────────────────────────────────────────────────┐
│  System Health                                     [Check ▾] │
│                                                              │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐       │
│  │ 8        │ │ 6        │ │ 142      │ │ 99.2%    │       │
│  │ Providers│ │ Routable │ │ Models   │ │ Success  │       │
│  │ Total    │ │          │ │ Routable │ │ (10 min) │       │
│  └──────────┘ └──────────┘ └──────────┘ └──────────┘       │
│                                                              │
│  ┌─ Alerts (2) ──────────────────────────────────────────┐  │
│  │ ⚠ provider-c: breaker open on gpt-5.4 (3 failures)   │  │
│  │ ⚠ provider-e: quarantined — onboarding failed         │  │
│  └───────────────────────────────────────────────────────┘  │
│                                                              │
│  ┌─ Active Jobs ─────────────────────────────────────────┐  │
│  │ full_check  running  ━━━━━━━━━━━━░░░░ 70%  3m ago     │  │
│  └───────────────────────────────────────────────────────┘  │
│                                                              │
│  Providers                               [Search] [Filter ▾]│
│  ┌──────────┬────────┬────────┬───────┬───────┬──────────┐  │
│  │ Name     │ State  │ Route  │ Check │ Reqs  │ Errors   │  │
│  ├──────────┼────────┼────────┼───────┼───────┼──────────┤  │
│  │ alpha    │ ✓ ok   │ active │ 12m   │ 847   │ 0.3%     │  │
│  │ beta     │ ✓ ok   │ active │ 12m   │ 523   │ 1.2%     │  │
│  │ gamma    │ ⚠ degr │ cooling│ 12m   │ 31    │ 12.3%    │  │
│  │ delta    │ ✗ down │ —      │ 12m   │ 0     │ —        │  │
│  │ epsilon  │ 🔒 quar│ —      │ never │ 0     │ —        │  │
│  └──────────┴────────┴────────┴───────┴───────┴──────────┘  │
└─────────────────────────────────────────────────────────────┘
```

**状态聚合规则**（三层合一显示）：

Provider 表里的 State 列是配置态+连通态的聚合：
- `ok` = enabled + reachable + 最近巡检通过
- `degraded` = enabled + reachable + 最近巡检部分失败
- `down` = enabled + 最近巡检全部失败或不可达
- `disabled` = 用户手动禁用
- `quarantined` = 系统或用户隔离

Route 列是路由态：
- `active` = 正在参与路由，有候选模型
- `eligible` = 满足条件但近期未被选中（所有模型都有更好候选）
- `cooling` = 有模型处于 breaker cooldown
- `—` = 不参与路由（disabled/quarantined/down）

**Alerts 生成规则**：

Alert 不是手写的，是从以下条件自动聚合：
1. 任何 provider 有 breaker 处于 open 状态 → alert
2. 任何 provider 10 分钟错误率 > 20% → alert
3. 任何 provider operator_state = quarantined → alert
4. 最近一次 full_check 失败 → alert
5. 最近一次 full_check 超过 `check_interval × 1.5` 未运行 → alert

### 1.3 路由解释器：从"为什么不选它"到可操作答案

**当前问题**：只有 `/api/v1/proxy/debug` 返回 JSON，没有面向人的解释。

**交互流**：

```
用户进入 Routing Lab
  ↓
选择/输入参数：
  Model:    [gpt-5.4        ▾]  （从路由表模型列表下拉）
  Endpoint: [responses      ▾]  （chat / responses）
  Stream:   [✓]
  Tools:    [✓]
  Required: [✓]
  ↓ 点击 "Explain"
  ↓
┌─────────────────────────────────────────────────────────────┐
│  Routing Explanation for gpt-5.4                             │
│  Shape: responses + stream + tools + required                │
│                                                              │
│  ┌─ Selected ────────────────────────────────────────────┐  │
│  │ ★ alpha    score: 0.82    reason: highest rank-0 score│  │
│  │   static: 0.75  priority: ×1.2  fp: 0.9  error: -2%  │  │
│  │   capabilities: responses ✓  stream ✓  tools ✓        │  │
│  │   breaker: healthy                                     │  │
│  └───────────────────────────────────────────────────────┘  │
│                                                              │
│  ┌─ Other Candidates (2) ────────────────────────────────┐  │
│  │ beta     score: 0.68  rank: 0                         │  │
│  │   capabilities: responses ✓  stream ✓  tools ✓        │  │
│  │   breaker: healthy                                     │  │
│  │                                                        │  │
│  │ gamma    score: 0.31  rank: 1                         │  │
│  │   capabilities: responses ✓  stream ✓  tools ⚠ unknown│  │
│  │   breaker: suspect (2 failures in 5 min window)        │  │
│  └───────────────────────────────────────────────────────┘  │
│                                                              │
│  ┌─ Filtered Out (3) ────────────────────────────────────┐  │
│  │ delta    reason: provider down (last check failed)     │  │
│  │ epsilon  reason: quarantined by operator               │  │
│  │ zeta     reason: responses tool_call unsupported       │  │
│  │          evidence: probed 2h ago, returned 400         │  │
│  └───────────────────────────────────────────────────────┘  │
│                                                              │
│  90% chance: alpha    10% chance: beta (probe)              │
└─────────────────────────────────────────────────────────────┘
```

**关键补全：过滤原因分类**

system-redesign 说"过滤原因必须可读"，但没给完整分类。以下是实现所需的完整 FilterReason 枚举：

```go
type FilterReason struct {
    Code    string // 机器可读
    Message string // 人可读
}

// Provider 级过滤（在候选生成阶段就被排除）
"provider_disabled"       → "Provider disabled by operator"
"provider_quarantined"    → "Provider quarantined: {note}"
"provider_down"           → "Provider unreachable since {last_check}"
"provider_auth_failed"    → "Authentication failed at last check"
"model_not_found"         → "Model not in provider's model list"
"smoke_failed"            → "Smoke check failed: {error}"
"smoke_not_run"           → "No smoke check result (unknown quality)"

// 格式级过滤（候选存在但格式不匹配）
"format_mismatch"         → "Chat request to responses-only provider"

// 能力级过滤（候选存在但能力不支持）
"capability_unsupported"  → "{endpoint} {capability} unsupported (probed {age} ago)"
"capability_unknown"      → "{endpoint} {capability} not yet probed"  // 不过滤，降级

// 运行时过滤
"breaker_open"            → "Circuit breaker open ({failures} failures, cooldown {remaining}s)"
```

**分数构成的展示**：

当前 router.go 的分数公式是 `0.4×latency + 0.3×health + 0.3×fingerprint`，加上 priority 乘数和 error penalty。路由解释器需要把每一项拆开展示：

```json
{
  "score_breakdown": {
    "latency_score": 0.85,    // 1.0 - clamp(latencyMs/30000, 0, 1)
    "health_score": 0.92,     // provider health / 100
    "fingerprint_score": 0.9, // clamp(totalScore/10, 0, 1), with verdict penalty
    "base_score": 0.89,       // 0.4*latency + 0.3*health + 0.3*fingerprint
    "priority_multiplier": 1.2,
    "after_priority": 1.068,
    "error_penalty": -0.02,   // -(errorRate * score)
    "breaker_penalty": 0,     // 0 / ×0.5 / ×0.3
    "final_score": 1.048
  }
}
```

### 1.4 请求轨迹：从"失败了"到"为什么失败"

**当前问题**：proxy 失败只打日志，用户无法在 UI 上做归因。

**交互流**：

```
用户进入 Proxy 页面
  ↓
看到请求列表（默认最近 100 条，按时间倒序）
  ┌──────────┬──────────┬──────────┬────────┬────────┬──────┐
  │ Time     │ Model    │ Provider │ Status │ Latency│ Tries│
  ├──────────┼──────────┼──────────┼────────┼────────┼──────┤
  │ 14:23:01 │ gpt-5.4  │ alpha    │ ✓ 200  │ 2.3s   │ 1    │
  │ 14:22:58 │ gpt-5.4  │ beta→alp │ ✓ 200  │ 5.1s   │ 2    │
  │ 14:22:45 │ o4-mini  │ —        │ ✗ 502  │ 12.4s  │ 3    │
  │ 14:22:30 │ gpt-5.4  │ gamma    │ ✗ done │ 8.2s   │ 1    │
  └──────────┴──────────┴──────────┴────────┴────────┴──────┘
  ↓ 用户点击某行展开
┌─────────────────────────────────────────────────────────────┐
│  Request Trace: tr_a1b2c3                                    │
│  Time: 2026-04-01 14:22:58    Model: gpt-5.4                │
│  Shape: responses + stream + tools                           │
│  Final: ✓ 200 via alpha (attempt 2)                          │
│                                                              │
│  Attempt 1: beta                                             │
│    Score: 0.82  Breaker: healthy                             │
│    → 500 Internal Server Error  (1.8s)                       │
│    → Classified: transient_5xx → breaker +1 → failover       │
│                                                              │
│  Attempt 2: alpha                                            │
│    Score: 0.75  Breaker: healthy                             │
│    → 200 OK  (3.3s)                                          │
│    → Stream started, completed normally                      │
│                                                              │
│  Candidates at request time:                                 │
│    beta (0.82), alpha (0.75), gamma (0.31 suspect)           │
│  Filtered: delta (down), epsilon (quarantined)               │
└─────────────────────────────────────────────────────────────┘
```

**请求分类体系**（proxy.go 里的 forwardResult 需要扩展）：

当前代码只有 `forwardOK / forwardRetry / forwardRetryMild / forwardDone` 四种。需要扩展为带语义的失败分类，用于轨迹记录和 UI 展示：

```go
type FailureClass string

const (
    FailureNone              FailureClass = ""                  // 成功
    FailureTimeout           FailureClass = "timeout"           // 连接或首字节超时
    FailureUpstream5xx       FailureClass = "upstream_5xx"      // 上游 500/502/503
    FailureRateLimit         FailureClass = "rate_limit"        // 429
    FailureAuthFailed        FailureClass = "auth_failed"       // 401
    FailureQuotaExhausted    FailureClass = "quota_exhausted"   // 403
    FailureModelGone         FailureClass = "model_gone"        // 404
    FailureBodyTooLarge      FailureClass = "body_too_large"    // 413
    FailureClientError       FailureClass = "client_error"      // other 4xx
    FailureProtocolError     FailureClass = "protocol_error"    // 响应体格式不对
    FailureStreamInterrupted FailureClass = "stream_interrupted" // 流中断
    FailureStreamIdle        FailureClass = "stream_idle"       // 流超时无数据
    FailureToolCallMissing   FailureClass = "tool_call_missing" // required tool_call 缺失
)
```

**存储策略**：

请求轨迹是高频写入。设计约束：
- 只存最近 N 条（默认 10000）或最近 M 小时（默认 24h），取先到者
- 用 ring buffer 语义：新记录覆盖最旧记录（SQLite 实现用 `DELETE WHERE id <= (SELECT MAX(id) - N)`）
- 每次写入不走事务，用 WAL 模式的 append 性能
- 轨迹查询支持按 model / provider / status / 时间范围过滤

### 1.5 运行中心：把后台任务变成可观测的一等公民

**当前问题**：`isChecking` 是一个布尔值，SSE 只推文本消息。

**交互流**：

```
┌─────────────────────────────────────────────────────────────┐
│  Job Center                                                  │
│                                                              │
│  ┌─ Running (1) ─────────────────────────────────────────┐  │
│  │ #42 full_check  all providers                          │  │
│  │ ━━━━━━━━━━━━━━━━━━━━━░░░░░░ 75%  (6/8 providers)      │  │
│  │ Started: 3m ago    ETA: ~1m                            │  │
│  │                                            [Cancel]    │  │
│  └───────────────────────────────────────────────────────┘  │
│                                                              │
│  ┌─ Recent (5) ──────────────────────────────────────────┐  │
│  │ #41 full_check      completed  12m ago   8/8  all ok   │  │
│  │ #40 single_check    completed  1h ago    alpha  ok     │  │
│  │ #39 fingerprint      completed  2h ago   sampled 12    │  │
│  │ #38 capability       completed  2h ago   probed 6      │  │
│  │ #37 onboarding      completed  5h ago    epsilon       │  │
│  └───────────────────────────────────────────────────────┘  │
│                                                              │
│  [Trigger Full Check]  [Trigger Fingerprint]                │
└─────────────────────────────────────────────────────────────┘
```

**Job 类型完整列表**：

| type | scope | 触发方式 | 说明 |
|---|---|---|---|
| `full_check` | all or provider_ids[] | 定时/手动 | 全量巡检 |
| `single_check` | provider_id | 手动（provider 详情页） | 单站巡检 |
| `onboarding` | provider_id | 新增 provider 后自动 | 新站上线流程 |
| `fingerprint` | sampled (provider_id, model)[] | 定时/手动 | 模型指纹检测 |
| `capability_refresh` | (provider_id, model)[] | 巡检后自动 | 能力探测刷新 |
| `balance_refresh` | provider_ids[] | 定时 | 余额查询 |

**Job 并发规则**：

当前的 `isChecking` 互斥锁太粗。改为：
- 同类型 job 互斥（不能同时跑两个 full_check）
- 不同类型 job 可并发（full_check 和 fingerprint 可以同时跑）
- onboarding 和 single_check 只锁定对应 provider，不影响其他 provider 的 job
- 如果 full_check 正在跑，拒绝对同一 provider 的 single_check（已在 full_check 范围内）

---

## 二、状态机补全

system-redesign 定义了状态枚举，但没说转换条件。

### 2.1 Provider 状态转换

**配置态**：

```
                    ┌──────────┐
       add provider │quarantin.│ ← 默认新增状态
                    └────┬─────┘
                         │ operator action: enable
                         ▼
                    ┌──────────┐
                    │ enabled  │ ←→ operator action: disable → [disabled]
                    └──────────┘
                         ▲
                         │ operator action: enable
                    ┌──────────┐
                    │ disabled │
                    └──────────┘
```

触发 quarantined 的自动条件（不仅是手动）：
- onboarding smoke 通过率 < 50%
- 连续 2 次 full_check 全部模型失败
- 3 次 full_check 内 health 持续 < 20

从 quarantined 恢复需要手动操作（不自动恢复，避免垃圾站反复震荡）。

**连通态**（每次巡检后更新）：

```
unknown → reachable         （首次巡检成功）
unknown → auth_failed       （首次巡检 401/403）
reachable → degraded        （巡检部分失败，correct/total < 0.8）
reachable → models_fetch_failed （/models 返回错误）
degraded → reachable        （巡检恢复到 > 0.8）
any → unknown               （超过 check_interval × 2 未巡检）
```

**路由态**（实时计算，不持久化）：

```go
func computeRoutingState(provider) RoutingState {
    if provider.OperatorState != "enabled" {
        return NotEligible
    }
    if provider.ConnectivityState == "auth_failed" ||
       provider.ConnectivityState == "models_fetch_failed" ||
       provider.ConnectivityState == "unknown" {
        return NotEligible
    }

    routableModels := countModelsInRoutingTable(provider.ID)
    if routableModels == 0 {
        return NotEligible
    }

    openBreakers := countOpenBreakers(provider.ID)
    if openBreakers > 0 && openBreakers == routableModels {
        return CoolingDown // 所有模型都被熔断
    }
    if openBreakers > 0 {
        return CoolingDown // 部分模型被熔断
    }

    // 检查最近 10 分钟是否有请求被路由到此 provider
    if recentRequests(provider.ID, 10*time.Minute) > 0 {
        return Active
    }
    return Eligible
}
```

### 2.2 Model Availability 状态转换

```
unknown → indexed_only       （/models 看到，但未测试）
indexed_only → smoke_ok      （smoke check correct=true）
indexed_only → smoke_failed  （smoke check correct=false 或 status=error）
smoke_ok → verified          （capability probe 全部完成且通过）
smoke_ok → stale             （超过 check_interval × 2 未重新验证）
verified → stale             （同上）
smoke_failed → smoke_ok      （重新巡检通过）
stale → smoke_ok             （重新巡检通过）
stale → smoke_failed         （重新巡检失败）
```

只有 `smoke_ok` 和 `verified` 进入路由表。`verified` 在排序中获得 rank 优势。

---

## 三、数据模型补全

### 3.1 完整 DDL

以下是新 schema，包含从旧 schema 的迁移路径。

```sql
-- ==========================================
-- providers: 扩展，不重建
-- ==========================================
-- 新增列（ALTER TABLE 方式迁移）
ALTER TABLE providers ADD COLUMN operator_state TEXT NOT NULL DEFAULT 'enabled';
ALTER TABLE providers ADD COLUMN priority_override REAL NOT NULL DEFAULT 1.0;
ALTER TABLE providers ADD COLUMN note TEXT NOT NULL DEFAULT '';
ALTER TABLE providers ADD COLUMN declared_format TEXT NOT NULL DEFAULT 'chat';
ALTER TABLE providers ADD COLUMN last_verified_at DATETIME;

-- 迁移: 从 api_format 填充 declared_format
-- UPDATE providers SET declared_format = api_format WHERE api_format != '';
-- 旧 status/health 字段保留但不再作为路由依据，逐步废弃

-- ==========================================
-- job_runs: 替代 check_runs
-- ==========================================
CREATE TABLE IF NOT EXISTS job_runs (
    id              TEXT PRIMARY KEY,            -- UUID
    type            TEXT NOT NULL,               -- full_check | single_check | onboarding | fingerprint | capability_refresh | balance_refresh
    scope_json      TEXT NOT NULL DEFAULT '{}',  -- {"provider_ids":[12,18]} 或 {"provider_id":12} 或 {"sampled":[...]}
    status          TEXT NOT NULL DEFAULT 'queued', -- queued | running | partial | completed | failed | cancelled
    trigger_type    TEXT NOT NULL DEFAULT 'manual', -- manual | scheduled | onboarding | auto
    progress_total  INTEGER NOT NULL DEFAULT 0,
    progress_done   INTEGER NOT NULL DEFAULT 0,
    summary_json    TEXT,                        -- 完成后的结构化摘要
    error_message   TEXT,
    started_at      DATETIME,
    ended_at        DATETIME,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_job_runs_status ON job_runs(status, created_at);
CREATE INDEX IF NOT EXISTS idx_job_runs_type ON job_runs(type, created_at);

-- 迁移：INSERT INTO job_runs SELECT id, 'full_check', '{}', status, trigger_type, ...  FROM check_runs;

-- ==========================================
-- check_results: 保留不变（历史数据）
-- ==========================================
-- 只改 run_id 的语义：现在指向 job_runs.id

-- ==========================================
-- current_results: 保留不变（语义不变，是权威快照）
-- ==========================================

-- ==========================================
-- capability_evidence: 替代 capabilities
-- ==========================================
CREATE TABLE IF NOT EXISTS capability_evidence (
    provider_id     INTEGER NOT NULL REFERENCES providers(id),
    model           TEXT NOT NULL,
    endpoint        TEXT NOT NULL,          -- 'chat' | 'responses'
    capability      TEXT NOT NULL,          -- 'basic' | 'streaming' | 'tool_use' | 'tool_call_required'
    verdict         TEXT NOT NULL,          -- 'supported' | 'unsupported' | 'unknown' | 'error'
    probe_kind      TEXT NOT NULL DEFAULT 'smoke', -- 'smoke' | 'exact' | 'inferred'
    evidence_detail TEXT,                   -- 错误信息或原始摘要
    tested_at       DATETIME NOT NULL,
    run_id          TEXT REFERENCES job_runs(id),
    PRIMARY KEY (provider_id, model, endpoint, capability)
);
CREATE INDEX IF NOT EXISTS idx_capev_provider ON capability_evidence(provider_id);

-- 迁移：从 capabilities 表拆分
-- 每行 capabilities 拆成最多 7 行 capability_evidence:
--   (chat, streaming), (chat, tool_use),
--   (responses, basic), (responses, streaming), (responses, tool_use)

-- ==========================================
-- fingerprint_current: 当前生效的指纹结果
-- ==========================================
CREATE TABLE IF NOT EXISTS fingerprint_current (
    provider_id     INTEGER NOT NULL REFERENCES providers(id),
    model           TEXT NOT NULL,
    vendor          TEXT NOT NULL,
    total_score     INTEGER NOT NULL,
    expected_min    INTEGER,
    verdict         TEXT NOT NULL,
    self_id_verdict TEXT,
    tested_at       DATETIME NOT NULL,
    run_id          TEXT REFERENCES job_runs(id),
    PRIMARY KEY (provider_id, model)
);

-- 迁移：INSERT INTO fingerprint_current
--   SELECT DISTINCT ON (provider_id, model) ... FROM fingerprint_results ORDER BY checked_at DESC;
-- SQLite 没有 DISTINCT ON，用子查询实现

-- ==========================================
-- proxy_request_traces: 请求轨迹
-- ==========================================
CREATE TABLE IF NOT EXISTS proxy_request_traces (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    trace_id        TEXT NOT NULL UNIQUE,    -- "tr_" + nanoid
    received_at     DATETIME NOT NULL,
    model           TEXT NOT NULL,
    endpoint        TEXT NOT NULL,           -- 'chat' | 'responses'
    stream          BOOLEAN NOT NULL DEFAULT 0,
    has_tools       BOOLEAN NOT NULL DEFAULT 0,
    tool_call_required BOOLEAN NOT NULL DEFAULT 0,
    candidate_count INTEGER NOT NULL,
    filtered_count  INTEGER NOT NULL,
    attempt_count   INTEGER NOT NULL,
    final_provider_id INTEGER,
    final_status    TEXT NOT NULL,           -- 'ok' | 'failed' | 'partial' (stream interrupted)
    final_http_code INTEGER,
    total_latency_ms INTEGER NOT NULL,
    client_error    TEXT
);
CREATE INDEX IF NOT EXISTS idx_traces_time ON proxy_request_traces(received_at);
CREATE INDEX IF NOT EXISTS idx_traces_model ON proxy_request_traces(model, received_at);
CREATE INDEX IF NOT EXISTS idx_traces_status ON proxy_request_traces(final_status, received_at);

-- ==========================================
-- proxy_attempts: 每次 failover attempt
-- ==========================================
CREATE TABLE IF NOT EXISTS proxy_attempts (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    trace_id        TEXT NOT NULL REFERENCES proxy_request_traces(trace_id),
    attempt_index   INTEGER NOT NULL,
    provider_id     INTEGER NOT NULL,
    provider_name   TEXT NOT NULL,
    score_at_selection REAL,
    match_rank      INTEGER,
    breaker_state   TEXT,                   -- 'healthy' | 'suspect' | 'half_open'
    upstream_status INTEGER,                -- HTTP status code
    failure_class   TEXT,                   -- FailureClass 枚举值
    latency_ms      INTEGER NOT NULL,
    wrote_to_client BOOLEAN NOT NULL DEFAULT 0  -- 是否已经开始向客户端写入
);
CREATE INDEX IF NOT EXISTS idx_attempts_trace ON proxy_attempts(trace_id);
CREATE INDEX IF NOT EXISTS idx_attempts_provider ON proxy_attempts(provider_id, trace_id);

-- ==========================================
-- routing_overrides: 手动路由干预
-- ==========================================
CREATE TABLE IF NOT EXISTS routing_overrides (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    model           TEXT,                   -- NULL 表示全局
    provider_id     INTEGER REFERENCES providers(id),
    override_type   TEXT NOT NULL,          -- 'pin' | 'block' | 'weight'
    value           TEXT,                   -- pin: provider_id; block: "true"; weight: "1.5"
    reason          TEXT,
    created_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(model, provider_id, override_type)
);
```

### 3.2 关键 Go 结构体

```go
// RequestShape 是路由的最小请求特征，用于 Stats/Breaker key 扩展
type RequestShape struct {
    Endpoint        string // "chat" | "responses"
    Stream          bool
    Tools           bool
    ToolCallRequired bool
}

func (rs RequestShape) Key() string {
    // 用于 map key 和数据库存储
    return fmt.Sprintf("%s:s%t:t%t:r%t", rs.Endpoint, rs.Stream, rs.Tools, rs.ToolCallRequired)
}

// RoutingExplanation 是路由解释器的返回结构
type RoutingExplanation struct {
    Model           string                `json:"model"`
    Shape           RequestShape          `json:"request_shape"`
    Eligible        []ExplainedCandidate  `json:"eligible_candidates"`
    Filtered        []FilteredCandidate   `json:"filtered_candidates"`
    Selected        *ExplainedCandidate   `json:"selected_candidate"`
    SelectionMethod string                `json:"selection_method"` // "weighted_top3" | "only_candidate" | "none"
}

type ExplainedCandidate struct {
    ProviderID      int64          `json:"provider_id"`
    ProviderName    string         `json:"provider_name"`
    MatchRank       int            `json:"match_rank"`
    ScoreBreakdown  ScoreBreakdown `json:"score_breakdown"`
    BreakerState    string         `json:"breaker_state"`
    CapabilityMatch map[string]string `json:"capability_match"` // capability → "supported"|"unknown"|"unsupported"
    RecentErrors    int            `json:"recent_errors"`
    RecentRequests  int            `json:"recent_requests"`
}

type ScoreBreakdown struct {
    LatencyScore      float64 `json:"latency_score"`
    HealthScore       float64 `json:"health_score"`
    FingerprintScore  float64 `json:"fingerprint_score"`
    BaseScore         float64 `json:"base_score"`
    PriorityMultiplier float64 `json:"priority_multiplier"`
    ErrorPenalty      float64 `json:"error_penalty"`
    BreakerPenalty    float64 `json:"breaker_penalty"`
    FinalScore        float64 `json:"final_score"`
}

type FilteredCandidate struct {
    ProviderID   int64  `json:"provider_id"`
    ProviderName string `json:"provider_name"`
    ReasonCode   string `json:"reason_code"`
    ReasonDetail string `json:"reason_detail"`
}

// RequestTrace 是一次代理请求的完整轨迹
type RequestTrace struct {
    TraceID         string          `json:"trace_id"`
    ReceivedAt      time.Time       `json:"received_at"`
    Model           string          `json:"model"`
    Shape           RequestShape    `json:"request_shape"`
    CandidateCount  int             `json:"candidate_count"`
    FilteredCount   int             `json:"filtered_count"`
    Attempts        []AttemptRecord `json:"attempts"`
    FinalProviderID *int64          `json:"final_provider_id"`
    FinalStatus     string          `json:"final_status"`
    FinalHTTPCode   *int            `json:"final_http_code"`
    TotalLatencyMs  int64           `json:"total_latency_ms"`
    ClientError     string          `json:"client_error,omitempty"`
}

type AttemptRecord struct {
    ProviderID      int64   `json:"provider_id"`
    ProviderName    string  `json:"provider_name"`
    AttemptIndex    int     `json:"attempt_index"`
    Score           float64 `json:"score"`
    MatchRank       int     `json:"match_rank"`
    BreakerState    string  `json:"breaker_state"`
    UpstreamStatus  *int    `json:"upstream_status"`
    FailureClass    string  `json:"failure_class"`
    LatencyMs       int64   `json:"latency_ms"`
    WroteToClient   bool    `json:"wrote_to_client"`
}
```

---

## 四、接口补全

system-redesign 列了接口骨架，这里补全请求/响应细节、错误码、分页策略。

### 4.1 Provider 管理

**`POST /api/v1/providers/preflight`**

不落库。执行三步验证：URL 连通 → Auth → Models fetch。

响应补全（system-redesign 只给了成功例）：

```json
// 部分失败
{
  "ok": false,
  "checks": {
    "url": "ok",
    "auth": "failed",
    "models": "skipped"
  },
  "error": "401 Unauthorized: invalid API key",
  "warnings": []
}

// URL 不通
{
  "ok": false,
  "checks": {
    "url": "failed",
    "auth": "skipped",
    "models": "skipped"
  },
  "error": "dial tcp: connection refused",
  "warnings": []
}
```

**`POST /api/v1/providers`**

完整请求：
```json
{
  "name": "uniproxy",
  "base_url": "https://api.example.com/v1",
  "api_key": "sk-xxx",
  "access_token": "optional-for-balance-query",
  "declared_format": "chat",
  "priority_override": 1.0,
  "note": "Main production relay",
  "skip_onboarding": false
}
```

完整响应：
```json
{
  "provider": {
    "id": 12,
    "name": "uniproxy",
    "operator_state": "quarantined",
    "created_at": "2026-04-01T14:00:00Z"
  },
  "onboarding_run_id": "run_abc123",
  "message": "Provider saved. Onboarding check started."
}
```

错误码：
- 400: name 为空 / base_url 格式错误
- 409: name 已存在

**`PATCH /api/v1/providers/{id}`**

允许修改的字段（部分更新）：
```json
{
  "operator_state": "enabled",     // enabled | disabled | quarantined
  "priority_override": 1.5,
  "note": "updated note"
}
```

不允许修改 name / base_url / api_key（这些是标识性字段，改了等于换站）。如果需要改这些，删除重建。

### 4.2 作业系统

**`GET /api/v1/jobs`**

查询参数：
- `status`: 逗号分隔，默认 "running,completed,failed"
- `type`: 逗号分隔，可选
- `limit`: 默认 20，最大 100
- `offset`: 分页偏移

响应：
```json
{
  "jobs": [
    {
      "id": "run_abc123",
      "type": "full_check",
      "status": "running",
      "trigger_type": "scheduled",
      "progress": {"done": 6, "total": 8},
      "started_at": "2026-04-01T14:00:00Z",
      "ended_at": null
    }
  ],
  "total": 42
}
```

**`DELETE /api/v1/jobs/{run_id}`**

取消正在运行的 job。只有 `queued` 和 `running` 状态可以取消。

### 4.3 路由解释

**`POST /api/v1/routing/explain`**

这是路由实验室的后端。system-redesign 给了请求和空响应，这里补全真实响应：

```json
{
  "model": "gpt-5.4",
  "request_shape": {
    "endpoint": "responses",
    "stream": true,
    "tools": true,
    "tool_call_required": true
  },
  "eligible_candidates": [
    {
      "provider_id": 1,
      "provider_name": "alpha",
      "match_rank": 0,
      "score_breakdown": {
        "latency_score": 0.85,
        "health_score": 0.92,
        "fingerprint_score": 0.90,
        "base_score": 0.89,
        "priority_multiplier": 1.2,
        "error_penalty": -0.02,
        "breaker_penalty": 0,
        "final_score": 1.048
      },
      "breaker_state": "healthy",
      "capability_match": {
        "responses_basic": "supported",
        "responses_streaming": "supported",
        "responses_tool_use": "supported"
      },
      "recent_errors": 1,
      "recent_requests": 234
    }
  ],
  "filtered_candidates": [
    {
      "provider_id": 4,
      "provider_name": "delta",
      "reason_code": "provider_down",
      "reason_detail": "Provider unreachable since 2026-04-01T12:00:00Z"
    },
    {
      "provider_id": 6,
      "provider_name": "zeta",
      "reason_code": "capability_unsupported",
      "reason_detail": "responses tool_use unsupported (probed 2h ago, returned 400)"
    }
  ],
  "selected_candidate": {
    "provider_id": 1,
    "provider_name": "alpha"
  },
  "selection_method": "weighted_top3"
}
```

### 4.4 请求轨迹

**`GET /api/v1/proxy/traces`**

查询参数：
- `model`: 过滤模型
- `provider_id`: 过滤 provider
- `status`: ok | failed | partial
- `since`: ISO 时间戳
- `until`: ISO 时间戳
- `limit`: 默认 50，最大 200
- `offset`: 分页偏移

**`GET /api/v1/proxy/traces/{trace_id}`**

返回完整 RequestTrace 结构（包含 attempts 数组）。

### 4.5 SSE 事件协议补全

当前 SSE 只推文本消息。重构为结构化事件：

```
-- Job 生命周期
event: job:created
data: {"run_id":"run_abc","type":"full_check","scope":{}}

event: job:progress
data: {"run_id":"run_abc","done":3,"total":8,"current_provider":"alpha","detail":"gpt-5.4 OK 2.3s"}

event: job:completed
data: {"run_id":"run_abc","status":"completed","summary":{"providers":8,"ok":7,"correct":142}}

event: job:failed
data: {"run_id":"run_abc","status":"failed","error":"context cancelled"}

-- Provider 状态变化
event: provider:state_changed
data: {"provider_id":12,"name":"alpha","field":"operator_state","old":"quarantined","new":"enabled"}

-- 路由表变化
event: routing:table_rebuilt
data: {"models":142,"providers":6,"trigger":"check_completed","run_id":"run_abc"}

-- Breaker 状态变化（运维重要信息）
event: breaker:state_changed
data: {"provider_id":3,"provider_name":"gamma","model":"gpt-5.4","old":"healthy","new":"open","failures":3}

-- Alert 生成
event: alert:created
data: {"id":1,"severity":"warning","message":"gamma: breaker open on gpt-5.4","provider_id":3}
```

---

## 五、请求形状维度扩展

这是 system-redesign §2.3 指出的核心问题：Stats 和 Breaker 按 (provider, model) 记账粒度太粗。

### 5.1 扩展方案

**不是**把 Stats 和 Breaker 的 key 全部换成 `(provider, model, shape)`。这会导致：
- Key 空间爆炸（6 个 provider × 100 个 model × ~8 种 shape = 4800 个 entry）
- 每个 entry 的样本量太小，统计不稳定

**正确做法**：两级记账。

```
Level 1: (provider, model)        — 用于 breaker 和粗粒度错误率
Level 2: (provider, model, shape) — 用于能力过滤和精细化降级
```

Breaker 仍然按 `(provider, model)` 维度。因为一个模型如果连续 3 次失败（不管什么 shape），说明这个 provider 上这个模型本身有问题。

Stats 同时记两级。路由解释器展示 Level 2 数据，Dashboard 展示 Level 1 聚合。

能力过滤按 `(provider, model, endpoint, capability)` 做硬过滤。这是 capability_evidence 表的职责，不是 Stats 的职责。

### 5.2 对 router.go 的改动

当前 `applyRequestRequirements` 用 `matchRank` 做软降级。改为：

1. 有明确证据 `unsupported` → 硬过滤（不进候选）
2. 有明确证据 `supported` → rank 0（最优）
3. 无证据 `unknown` → rank +N（降级但保留）
4. 证据过期（> 48h）→ 视为 unknown

这个逻辑当前代码已基本正确，需要的改动是：
- 把 `*bool` 换成从 `capability_evidence` 表查出的 `verdict`
- 增加证据年龄判断
- 把 `matchRank` 的含义文档化

---

## 六、迁移策略

### 6.1 数据库迁移

分三步，每步可独立发布：

**Step 1：扩展 providers 表 + 创建 job_runs 表**
- `ALTER TABLE providers ADD COLUMN ...`（5 个新列）
- `CREATE TABLE job_runs`
- 迁移 check_runs → job_runs
- 旧代码继续写 check_runs（双写过渡期），新代码读 job_runs

**Step 2：创建 capability_evidence + fingerprint_current**
- `CREATE TABLE capability_evidence`
- `CREATE TABLE fingerprint_current`
- 迁移脚本从 capabilities / fingerprint_results 填充
- 旧表保留，查询切到新表

**Step 3：创建 proxy_request_traces + proxy_attempts + routing_overrides**
- 纯新增表，无迁移
- proxy.go 开始写入轨迹

### 6.2 代码迁移

**不要**一次重构所有代码。按依赖顺序：

1. 先做 `store/` 层：新增表、新增 query 方法，旧方法标记 deprecated
2. 然后做 `proxy/`：RequestTrace 记录、FailureClass、routing explain
3. 然后做 `checker/` → `server/`：job_runs 统一、SSE 事件协议
4. 最后做前端页面

这个顺序的原因是：数据层改了之后，上层代码可以逐个模块切换。如果先改 UI，UI 还是引用旧的数据查询，等 store 改了 UI 又得跟着改。

---

## 七、落地审查

### 7.1 这个设计的工作量评估

以当前代码规模（9300 行）为基准：

| 模块 | 新增/修改行数估算 | 依赖 |
|---|---|---|
| store: schema migration + queries | ~400 新增 | 无 |
| proxy: RequestTrace + FailureClass | ~300 修改 | store |
| proxy: routing explain | ~200 新增 | 无 |
| checker/server: job_runs 统一 | ~500 修改 | store |
| server: SSE 事件协议 | ~200 修改 | job_runs |
| server: 新 API endpoints | ~400 新增 | store + proxy |
| web: Dashboard 重做 | ~300 修改 | API |
| web: Provider 详情页重做 | ~200 修改 | API |
| web: Routing Lab 页面 | ~200 新增 | routing explain API |
| web: Job Center 页面 | ~200 新增 | jobs API |
| web: Proxy 轨迹页面 | ~200 新增 | traces API |

总计约 3000-3500 行代码改动。分 Phase 1/2/3 执行，每个 Phase 可独立发布。

### 7.2 最大风险点

1. **check_runs → job_runs 迁移**：这是最大的破坏面。当前 server.go 的 `RunCheckAndStore` 深度耦合 check_runs 写入和 SSE 推送。重构这个方法时需要保证：旧的巡检流程不中断，新的 job 系统能接管。建议先双写，再切换。

2. **proxy 请求轨迹的写入性能**：每个代理请求都写 SQLite。在高并发场景（100+ req/s）下可能成为瓶颈。解决方案：用内存 buffer + 定时 batch flush（每 1s 或 buffer 满 100 条时 flush）。

3. **capability_evidence 表的查询模式变化**：当前路由表重建时用 `GetAllCapabilities()` 一次拉所有能力数据到内存。新的 capability_evidence 行数会是旧 capabilities 的 5-7 倍。需要确保查询效率，考虑按 provider_id 分批加载。

### 7.3 这个设计里故意没做的事

- **多用户/多租户**：当前是单用户系统，不需要。
- **Provider secrets 迁到 DB**：system-redesign 说了 Phase 2 做，现在不做。
- **自适应权重**：system-redesign §10 Phase 3 的内容，先把证据层做好再说。
- **请求级的实时 SSE 推送**：只推 job 级事件和 breaker 状态变化，不推每个代理请求。代理请求的观测走轨迹查询，不走 SSE。
- **历史趋势图**：Dashboard 只展示当前状态和最近窗口的数值。如果后续需要趋势，再加 time-series 聚合表。
