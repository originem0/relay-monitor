# Relay Monitor

OpenAI 兼容中转站监控面板 + 智能路由代理。单二进制部署，浏览器 Dashboard，自动巡检 + 手动检测，模型真伪鉴别，一个 Key 统一对外。

## 功能

- **Dashboard 总览** — 站点卡片网格，健康分/余额/正确率/延迟一目了然
- **模型视图** — 跨站点聚合，按模型维度查看哪个站最好用，一键复制 Claude Code / Cursor / OpenCode 配置
- **指纹鉴别** — 10 道分层数学/推理题，检测模型是否被降级或替换
- **智能路由代理** — 对外暴露统一 `/v1` 端点，自动路由到质量最好的站点，故障自动切换
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

# 发送请求（自动路由到最佳站点）
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-relay-your-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5.2","messages":[{"role":"user","content":"hi"}]}'

# Streaming 也支持
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-relay-your-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v3.2","messages":[{"role":"user","content":"hi"}],"stream":true}'
```

### 路由逻辑

- 只有检测正确（`correct=true`）的模型才进入路由表
- 按延迟和健康分加权评分，从 top 3 中加权随机选择
- 请求失败自动 failover 到下一个站点（最多 2 次重试）
- Circuit breaker：连续 3 次失败的 (站点, 模型) 对自动移出路由，60s 后探测恢复
- 支持 `/v1/chat/completions` 和 `/v1/responses` 两种格式
- Dashboard `/proxy` 页面查看可用模型目录、流量统计、站点路由状态

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
- GPT-5+ 和 codex 模型自动优先使用 responses API
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
  proxy/                             # 智能路由代理、Circuit Breaker、遥测
  notifier/                          # 系统通知（平台相关）

web/
  static/{htmx.min.js, style.css}    # 前端资源
  templates/                         # Go HTML 模板
```
