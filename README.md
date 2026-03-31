# Relay Monitor

OpenAI 兼容中转站监控面板 + 智能路由代理。单二进制部署，浏览器 Dashboard，自动巡检 + 手动检测，模型真伪鉴别，一个 Key 统一对外。

## 功能

- **Dashboard 总览** — 站点卡片网格，健康分/余额/正确率/延迟一目了然，支持搜索过滤（站点名称或 URL）
- **模型视图** — 跨站点聚合，按模型维度查看哪个站最好用，指纹筛选/排序，一键复制配置（Claude Code / Codex CLI / OpenCode）
- **指纹鉴别** — 10 道分层数学/推理题，检测模型是否被降级或替换，结果反馈到路由评分，进度实时显示在 TASK PROGRESS 面板
- **智能路由代理** — 统一 `/v1` 端点，多维评分 + 实时反馈 + 自动 failover + 重试超时递减
- **能力探测** — 自动检测 tool_use / streaming 支持，标注 Claude Code 兼容性（仅限 Claude 模型）
- **余额监控** — 每次检测自动刷新余额，获取失败时 Dashboard 显示提示
- **变更检测** — 每次巡检与上次对比，模型新增/移除/状态变化自动记录
- **SSE 实时推送** — 巡检和指纹检测进度实时展示，断线自动重连
- **站点管理** — Web UI 添加/编辑/删除站点，操作即时同步到数据库和配置文件
- **安全防护** — CSRF Origin/Referer 校验、XSS 转义、Provider 名称和 URL 格式验证

## 快速开始

```bash
# 编译
go build -o relay-monitor .

# 准备配置
cp providers.json.example providers.json
# 编辑 providers.json 填入你的中转站信息

# 启动 Dashboard（默认 :8080）
./relay-monitor
# 打开 http://localhost:8080
```

这项目是 **原生单二进制**，默认就该直接跑，不需要 Docker。除非你的运维体系已经强绑定容器，不然为了它再包一层 Docker 只是多一层缓存、镜像和排障噪音。

## 部署

### Linux 服务器

```bash
# 编译
go build -o relay-monitor .

# 配置监听地址（仅本地，配合 nginx 反代）
# config.toml
listen = "127.0.0.1:8090"
external_url = "https://your-domain.com"    # 面板显示的外网地址

# 用 systemd 管理
sudo systemctl enable --now relay-monitor

# nginx 反代 + Basic Auth 保护 Dashboard，/v1/ 路径走 Bearer token
```

如果你不需要 systemd，直接在宿主机启动也完全可行：

```bash
./relay-monitor
```

项目数据默认落在 `./data/relay-monitor.db`，配置从当前工作目录的 `config.toml` 和 `providers.json` 读取。你用什么目录启动，就在那个目录维护状态。

## 智能路由代理

在 `config.toml` 中启用代理，即可将所有中转站聚合为一个统一入口：

```toml
[proxy]
enabled = true
api_key = "sk-relay-your-key"    # 留空则自动生成
request_timeout = "30s"
stream_first_byte_timeout = "30s"
stream_idle_timeout = "60s"
max_retries = 2
```

启动后，下游应用只需对接一个端点：

```bash
# 查看可用模型
curl http://localhost:8080/v1/models -H "Authorization: Bearer sk-relay-your-key"

# Chat Completions（自动路由到最佳站点）
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-relay-your-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5.2","messages":[{"role":"user","content":"hi"}]}'

# Responses API（同样支持，自动路由）
curl http://localhost:8080/v1/responses \
  -H "Authorization: Bearer sk-relay-your-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5.4","input":"hi"}'
```

### 配置 Codex CLI

在 `~/.codex/config.toml` 中：

```toml
model = "gpt-5.4"
model_provider = "relay"

[model_providers.relay]
name = "Relay Monitor"
base_url = "https://your-domain.com/v1"
wire_api = "responses"
experimental_bearer_token = "sk-relay-your-key"
```

### 配置 Claude Code

Claude Code 使用 Anthropic API 格式，不能直接对接 OpenAI 兼容 proxy。需要通过协议转换工具（如 [claude-adapter](https://github.com/shantoislamdev/claude-adapter)）桥接，或者直连支持 Anthropic 格式的中转站：

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "https://relay-that-supports-anthropic-format.com",
    "ANTHROPIC_AUTH_TOKEN": "your-key"
  }
}
```

模型视图中的「配置」按钮会自动判断：只有 Claude 模型且支持 tool_use + streaming 时才显示 Claude Code 配置。

### 路由评分

每个 (站点, 模型) 的路由分数由三部分组成：

| 信号 | 权重 | 来源 | 更新时机 |
|------|------|------|----------|
| 延迟 | 40% | 巡检测得的响应时间 | 每次巡检 |
| 健康分 | 30% | 站点正确率 | 每次巡检 |
| 指纹分 | 30% | 模型真伪鉴别得分 | 指纹检测后 |

计算后乘以站点的 **Priority 倍数**（默认 1.0）。设 1.5 = 同等条件下优先，2.0 = 几乎锁定首选。

在此基础上叠加两层实时修正：

- **错误率惩罚**（每次请求实时计算）：10 分钟滑动窗口，分数 × (1 - 错误率)。最少 5 个请求后才生效
- **Circuit Breaker**（每次请求实时检查）：Suspect 状态分数 × 0.5，Open 状态直接跳过；HalfOpen 只允许 1 个探测请求，其余并发请求会直接跳过该站，避免把半死不活的站点再次打穿

### Failover

请求失败时自动切换到下一个候选站点。重试使用递减超时（30s → 15s → 7.5s，最低 5s），避免总等待时间过长。

| 上游返回 | 行为 | 触发 Breaker |
|----------|------|-------------|
| 5xx / 429 | 立即换站 | 是（连续 3 次 → 熔断 60s） |
| 401 / 403 / 404 | 立即换站 | 否（持久性错误，不惩罚） |
| 其他 4xx | 不换站，透传给客户端 | 否 |

### 错误率统计

错误率按 **(站点, 模型)** 粒度独立统计，10 分钟滑动窗口，窗口过期后计数器归零。

**什么算错误**：proxy 转发请求后，上游返回非 200 的响应（5xx、429、401、403、404、其他 4xx）或网络超时/连接失败，都记为一次错误。

**Failover 中的独立计算**：每次 failover 重试都是独立的一次记录。例如请求 `gpt-5.4`，proxy 先试站点 A 返回 503，再试站点 B 返回 200——站点 A 记 1 次请求 + 1 次错误，站点 B 记 1 次请求 + 0 次错误。用户侧这次请求是成功的，但站点 A 的错误率会上升。

**对路由的影响**：错误率参与路由评分，`score × (1 - 错误率)`。需要累计至少 5 次请求后才生效，避免小样本噪声（第一个请求失败不会直接变成 100% 错误率惩罚）。

**Proxy 页面显示**：页面上的错误率 = 该站点在所有模型上的总错误数 ÷ 总请求数。

### Circuit Breaker

针对每个 (站点, 模型) 对独立维护状态：

```
Healthy → (2次失败) → Suspect → (3次失败) → Open → (60s冷却) → HalfOpen
                                                                    |
                                                         成功 → Healthy
                                                         失败 → Open
```

`HalfOpen` 不是“降权继续放流量”，而是单探测闸门。只有一个请求能进去验证恢复情况，其他请求直接选下一个候选。

### API 格式兼容

同时支持 `/v1/chat/completions` 和 `/v1/responses` 两种端点。路由不做静态格式过滤——大多数 OpenAI 兼容中转站同时支持两种格式，proxy 会将请求原样转发到对应路径。如果某个站不支持 `/v1/responses`，上游返回 404 后自动 failover 到下一个候选，错误率惩罚会逐渐降低该站在此路径上的优先级，实现动态学习。

### Responses 可靠性

`/v1/responses` 不再只是“上游回什么就原样透传什么”。

- 非流式返回会先校验结构；缺少关键字段的假 200 响应会被判为失败并自动切到下一个站点
- 流式返回会先看首个事件；如果首帧就是畸形数据，proxy 会直接 failover，不把垃圾流量转给下游
- 如果流已经开始发送，后续才发现上游不满足必要约束（例如该请求要求 tool call，但流里根本没有），proxy 会返回协议错误，而不是继续把错误 payload 包装成“成功”
- `responses` 能力会按热模型优先补测，`gpt-5.4 / gpt-5 / gpt-4.1 / o1/o3/o4 / codex` 这一类不会再被冷门模型挤掉探测名额

这块的目的很直接：别再让下游收到一个看着像成功、实际 `output[0].type` 都取不出来的假响应。

### 巡检与手动刷新

- 服务启动后会立刻跑一次全量 warmup；这期间 `checking=true` 是正常现象，不是卡死
- `POST /api/v1/check/trigger/{name}` 的单站手动检查现在不只写 `check_results`，还会立刻刷新 capability，并重建 proxy 路由表
- capability 探测结果按 chat / responses 分开存储，带独立时间戳；短暂探测失败不会把旧的已知能力硬覆盖成 `false`

### 当前快照 vs 历史

这项目现在明确区分两层数据，不再把“历史记录”和“当前真相”混在一起：

- `check_results` 是 **追加式历史**，保留每次巡检/手动检查的原始结果
- `current_results` 是 **当前权威快照**，只代表 proxy 和模型页现在应该依据什么状态决策

只有 **完整的全量巡检** 才会替换 `current_results` 并更新 provider 当前状态。`quick`、抽样、被中断的 full run，只写历史，不会拿半截数据污染当前路由。

如果某个 provider 的全量巡检在模型列表阶段就失败，当前快照会被清空，并把 provider 状态更新为 `error/down`；不会再出现“数据库里明明全挂了，proxy 还继续拿旧模型路由”的鬼情况。

### `/models` 和 `/proxy` 的区别

这两个页面故意不是一个语义：

- `/proxy` 只看 **当前可路由子集**。这里出现的 provider / model，代表 proxy 此刻真的可能选它
- `/models` 看的是 **当前 provider 快照**，包括失败项、未抽到指纹的项、不可路由但仍在当前快照里的项

所以“模型页能看到某个 provider”不等于“proxy 还会选它”。要看真实选路，先看 `/proxy`，再看 `/api/v1/proxy/stats?debug=...`

### 指纹抽样策略

指纹检测不是全量扫完整个模型表。当前策略是：

- 先按厂商分组
- 每个 provider 对每个厂商最多测前 3 个最强候选
- 结果写回模型页和路由评分

这意味着模型页里出现“未抽样”是正常行为，不代表站点没测，只代表当前模型没进入该轮指纹目标。这样做是为了把预算留给最新、最值得怀疑、最影响路由的模型，而不是把请求浪费在一堆边缘小模型上。

### 路由调试

查看某个模型的静态候选分数：

```bash
curl http://localhost:8080/api/v1/proxy/stats?debug=gpt-5.4
```

这只返回 `static_scores`，不包含请求形状约束，不能直接等同于真实 `/chat/completions` 或 `/responses` 的最终选路。

查看某类请求的真实候选顺序：

```bash
curl "http://localhost:8080/api/v1/proxy/stats?debug=gpt-5.4&format=responses&stream=true&tools=true&tool_call=true"
```

返回 `request_candidates` 和 `static_scores` 两部分。`request_candidates` 使用和真实 proxy 一致的能力过滤、breaker、错误率惩罚和 match rank；`static_scores` 只是底表快照。编辑站点 Priority 后立即生效，无需等待巡检。

如果你要看 `/responses` 真正会不会优先走某个站，别再只盯着 `static_scores`。那只是底表排序，不是最终选路。

## 配置

### providers.json

```json
[
  {
    "name": "站点名称",
    "base_url": "https://example.com/v1",
    "api_key": "sk-xxx",
    "access_token": "面板令牌（可选，用于余额查询）",
    "api_format": "chat 或 responses",
    "priority": 1.5,
    "pinned": true,
    "note": "备注信息"
  }
]
```

- `api_format` 省略默认 `chat`（`/v1/chat/completions`）
- `access_token` 是 new-api 面板登录令牌，用于查询余额
- `priority` 路由倍数，默认 1.0，大于 1 则优先路由
- `name` 不能包含 `< > " ' \ `` 等特殊字符

### config.toml

```toml
listen = ":8080"               # 监听地址
check_interval = "8h"          # 自动巡检间隔
retention_days = 7             # 事件保留天数
max_concurrency = 16           # 并发检测站点数
request_interval = "2s"        # 站内模型间隔
ssl_verify = false             # SSL 证书验证
external_url = ""              # 外网访问地址（Proxy 页面显示用）
balance_threshold = 5.0        # 余额告警阈值

[proxy]
enabled = false                # 启用智能路由代理
api_key = ""                   # 代理 API Key（留空自动生成）
request_timeout = "30s"        # 非流式请求超时
stream_first_byte_timeout = "30s"  # 流式首字节超时
stream_idle_timeout = "60s"    # 流式空闲超时
max_retries = 2                # 最大重试次数
```

## CLI 模式

```bash
relay-monitor                    # 启动 Dashboard（默认）
relay-monitor --all              # CLI 全量测试
relay-monitor --list             # 列出所有站点的模型
relay-monitor --add              # 交互式添加站点
relay-monitor --remove           # 交互式删除站点
relay-monitor "站点名"            # 测试指定站点
```

## 技术栈

- Go 1.22+（单二进制，无外部依赖）
- SQLite（modernc.org/sqlite，纯 Go，无 CGO，FK 约束启用）
- HTMX + Go templates（无 Node.js 构建）
- SSE（Server-Sent Events，实时推送，自动重连）

## 项目结构

```
main.go                              # 入口
config.toml                          # 应用配置
providers.json                       # 站点配置

internal/
  config/config.go                   # 配置加载
  provider/                          # 厂商识别、旗舰选择、跳过规则
  checker/                           # HTTP 客户端、Chat、指纹、能力探测、余额、变更检测
  store/                             # SQLite 存储（含事务性级联删除）
  scheduler/                         # 自适应定时调度
  server/                            # HTTP 路由、SSE、页面渲染、CSRF 防护
  proxy/                             # 智能路由、Circuit Breaker、Stats、Failover
  notifier/                          # 系统通知（平台相关）

web/
  static/{htmx.min.js, style.css}    # 前端资源
  templates/                         # Go HTML 模板
```
