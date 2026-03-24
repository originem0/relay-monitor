# Relay Monitor

OpenAI 兼容中转站监控面板 + 智能路由代理。单二进制部署，浏览器 Dashboard，自动巡检 + 手动检测，模型真伪鉴别，一个 Key 统一对外。

## 功能

- **Dashboard 总览** — 站点卡片网格，健康分/余额/正确率/延迟一目了然
- **模型视图** — 跨站点聚合，按模型维度查看哪个站最好用，指纹筛选/排序，一键复制配置
- **指纹鉴别** — 10 道分层数学/推理题，检测模型是否被降级或替换，结果反馈到路由评分
- **智能路由代理** — 统一 `/v1` 端点，多维评分 + 实时反馈 + 自动 failover
- **能力探测** — 自动检测 tool_use / streaming 支持，标注 Claude Code 兼容性
- **余额监控** — 读取 new-api 面板余额，Dashboard 直接显示
- **变更检测** — 每次巡检与上次对比，模型新增/移除/状态变化自动记录
- **SSE 实时推送** — 巡检进度实时展示，任务面板显示每个模型测试结果
- **站点管理** — Web UI 添加/编辑/删除站点

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

### 路由评分

每个 (站点, 模型) 的路由分数由三部分组成：

| 信号 | 权重 | 来源 | 更新时机 |
|------|------|------|----------|
| 延迟 | 40% | 巡检测得的响应时间 | 每次巡检 |
| 健康分 | 30% | 站点正确率 | 每次巡检 |
| 指纹分 | 30% | 模型真伪鉴别得分 | 指纹检测后 |

计算后乘以站点的 **Priority 倍数**（默认 1.0）。设 1.5 = 同等条件下优先，2.0 = 几乎锁定首选。Priority 只是乘数，不改变质量判断——一个基础分差的站即使 priority 拉满也打不过真正好的站。

在站点详情页「编辑站点信息」中设置，保存后下次巡检或手动触发检测后生效。

在此基础上叠加两层实时修正：

- **错误率惩罚**（每次请求实时计算）：10 分钟滑动窗口内，统计该 (站点, 模型) 的请求总数和失败次数，错误率 r = 失败数 / 总数，分数 × (1-r)。最少 5 个请求后才生效，避免小样本噪声。窗口过期后计数器归零，历史错误不会永远拖累评分
- **Circuit Breaker**（每次请求实时检查）：Suspect 状态分数 × 0.5，Open 状态直接跳过，HalfOpen 状态分数 × 0.3

### Proxy 页面的错误率

Proxy 页面显示的错误率 = 代理转发到上游后收到 5xx / 超时 / 403 的次数 ÷ 该 (站点, 模型) 的总请求数。Failover 重试中每次尝试独立计算——第一个站 500 重试第二个站成功，第一个站记一次错误，第二个站记一次成功。

### 选择策略

90% 概率选分数最高的站点，10% 随机探测其他 top 3 候选（保持对备选站点的健康感知）。

### Failover

请求失败时自动切换到下一个候选站点，不同错误码的处理：

| 上游返回 | 行为 | 触发 Breaker |
|----------|------|-------------|
| 5xx | 立即换站 | 是（连续 3 次 → 熔断 60s） |
| 429 | 立即换站 | 是 |
| 403 | 立即换站 | 否（余额不足是持久状态） |
| 其他 4xx | 不换站，透传给客户端 | 否 |

最多重试 `max_retries` 次（默认 2），所有候选耗尽返回 502。

### Circuit Breaker

针对每个 (站点, 模型) 对独立维护状态：

```
Healthy → (2次失败) → Suspect → (3次失败) → Open → (60s冷却) → HalfOpen
                                                                    ↓
                                                         成功 → Healthy
                                                         失败 → Open
```

Breaker 状态在每次请求时实时检查，不依赖巡检周期。站点恢复后立即可用。

### API 格式兼容

同时支持 `/v1/chat/completions` 和 `/v1/responses` 两种端点。大多数 OpenAI 兼容中转站同时支持两种格式，proxy 会自动将请求路由到正确路径。

## 配置

### providers.json

```json
[
  {
    "name": "站点名称",
    "base_url": "https://example.com/v1",
    "api_key": "sk-xxx",
    "access_token": "面板令牌（可选，用于余额查询）",
    "api_format": "chat 或 responses"
  }
]
```

- `api_format` 省略默认 `chat`（`/v1/chat/completions`）
- `access_token` 是 new-api 面板登录令牌，用于查询余额

### config.toml

```toml
listen = ":8080"               # 监听地址
check_interval = "8h"          # 自动巡检间隔
retention_days = 7             # 事件保留天数
max_concurrency = 16           # 并发检测站点数
request_interval = "2s"        # 站内模型间隔
ssl_verify = false             # SSL 证书验证
external_url = ""              # 外网访问地址（Proxy 页面显示用）

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
- SQLite（modernc.org/sqlite，纯 Go，无 CGO）
- HTMX + Go templates（无 Node.js 构建）
- SSE（Server-Sent Events，实时推送）

## 项目结构

```
main.go                              # 入口
config.toml                          # 应用配置
providers.json                       # 站点配置

internal/
  config/config.go                   # 配置加载
  provider/                          # 厂商识别、旗舰选择、跳过规则
  checker/                           # HTTP 客户端、Chat、指纹、能力探测、余额、变更检测
  store/                             # SQLite 存储
  scheduler/                         # 自适应定时调度
  server/                            # HTTP 路由、SSE、页面渲染
  proxy/                             # 智能路由、Circuit Breaker、Stats、Failover
  notifier/                          # 系统通知（平台相关）

web/
  static/{htmx.min.js, style.css}    # 前端资源
  templates/                         # Go HTML 模板
```
