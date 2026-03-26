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
- **Circuit Breaker**（每次请求实时检查）：Suspect 状态分数 × 0.5，Open 状态直接跳过，HalfOpen 状态分数 × 0.3

### Failover

请求失败时自动切换到下一个候选站点。重试使用递减超时（30s → 15s → 7.5s，最低 5s），避免总等待时间过长。

| 上游返回 | 行为 | 触发 Breaker |
|----------|------|-------------|
| 5xx / 429 | 立即换站 | 是（连续 3 次 → 熔断 60s） |
| 401 / 403 / 404 | 立即换站 | 否（持久性错误，不惩罚） |
| 其他 4xx | 不换站，透传给客户端 | 否 |

### Circuit Breaker

针对每个 (站点, 模型) 对独立维护状态：

```
Healthy → (2次失败) → Suspect → (3次失败) → Open → (60s冷却) → HalfOpen
                                                                    |
                                                         成功 → Healthy
                                                         失败 → Open
```

### API 格式兼容

同时支持 `/v1/chat/completions` 和 `/v1/responses` 两种端点。路由不做静态格式过滤——大多数 OpenAI 兼容中转站同时支持两种格式，proxy 会将请求原样转发到对应路径。如果某个站不支持 `/v1/responses`，上游返回 404 后自动 failover 到下一个候选，错误率惩罚会逐渐降低该站在此路径上的优先级，实现动态学习。

### 路由调试

查看某个模型的实际路由分数：

```bash
curl http://localhost:8080/api/v1/proxy/stats?debug=gpt-5.4
```

返回按分数排序的候选列表，包含 provider、score、latency_ms、format。编辑站点 Priority 后立即生效，无需等待巡检。

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
