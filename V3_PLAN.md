# PacketLab V3 开发计划

## 概览

两个核心新功能：
1. **网卡流量捕获** — 直接从网络接口抓包，解析 HTTP 请求，关联进程信息
2. **代理拦截模式** — 自动放过 / 手动审批 双模式

---

## 功能 1：网卡流量捕获 + 进程关联

### 架构决策

| 决策项 | 选择 | 理由 |
|--------|------|------|
| 抓包库 | `google/gopacket` + `libpcap` | Go 生态最成熟的包捕获库，支持 BPF 过滤 |
| TCP 重组 | `gopacket/tcpassembly` | 内置 TCP 流重组，处理重传/乱序 |
| HTTP 解析 | 自实现轻量解析器 | 只需 Method/URL/Headers/Body，无需完整 HTTP 语义 |
| 进程关联 (macOS) | `lsof -i tcp -n -P` 轮询 + 缓存 | 无需 root 守护进程，用户级可运行 |
| 进程关联 (Linux) | `/proc/net/tcp` + `/proc/<pid>/fd` | 内核暴露，零外部依赖 |
| 进程关联 (Windows) | `netstat -ano` + Windows API | 仅 WSL/非优先支持 |

### 新增模块

```
internal/capture/
  engine.go         # CaptureEngine — 主入口，管理抓包生命周期
  tcp_stream.go     # TCP 流重组工厂 (tcpassembly)
  http_extract.go   # HTTP 请求/响应从 TCP 流中提取
  process.go        # 进程信息解析 + IP:端口 → PID 映射缓存
  process_darwin.go # macOS lsof 实现
  process_linux.go  # Linux /proc 实现
```

### 数据模型扩展

```go
// models.go 新增
type ProcessInfo struct {
    PID     int    `json:"pid"`
    Name    string `json:"name"`
    Cmdline string `json:"cmdline"`
}

// CapturedRequest 新增字段
Process      *ProcessInfo `json:"process,omitempty"`
CaptureMode  string       `json:"capture_mode"` // "proxy" | "nic"
Interface    string       `json:"interface,omitempty"`  // 网卡名
```

### 进程关联流程

```
┌─────────────┐    定期轮询(5s)    ┌──────────────┐
│  lsof /     │ ◄─────────────── │  ProcessCache │
│  /proc/net  │     PID, Name     │  IP:Port→Proc │
└─────────────┘                   └──────┬───────┘
                                         │ 查询
┌─────────────┐    TCP 流              ┌─▼──────────┐
│  网卡       │ ────► gopacket ──────► │ HTTP 提取   │
│  (en0/eth0) │    重组+解析           │ + 关联进程   │
└─────────────┘                        └──────┬──────┘
                                              │
                                         ┌────▼─────┐
                                         │ BatchWriter│
                                         │  + WS 广播 │
                                         └──────────┘
```

### 实现阶段

| 阶段 | 内容 | 产出 |
|------|------|------|
| P1 | gopacket 抓包 + BPF 过滤 `tcp port 80 or tcp port 443` | `engine.go` + `--capture` 参数 |
| P2 | TCP 流重组 + HTTP 请求/响应提取 | `tcp_stream.go` + `http_extract.go` |
| P3 | macOS 进程关联 (lsof) | `process_darwin.go` |
| P4 | Linux 进程关联 (/proc) | `process_linux.go` |
| P5 | 前端显示进程列 + 网卡选择器 | `index.html` 扩展 |

### 新建 CLI 参数

```
--capture            启用网卡抓包
--capture-iface eth0 指定网卡（默认自动检测）
--capture-bpf "tcp port 80 or tcp port 443"  BPF 过滤表达式
```

### 关键技术点

1. **HTTPS 限制**：网卡抓包只能看到 CONNECT 握手 (SNI hostname)，无法解密 HTTPS 内容。协议列标注 `(encrypted)`。
2. **进程关联时效**：5 秒轮询窗口可能错过短连接。使用连接开始时立即查询 + 定期刷新策略。
3. **性能**：BPF 在内核层过滤，用户态只处理 HTTP 端口流量。每连接 goroutine 处理 TCP 流。

### 新增 API 端点

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/api/capture/status` | 抓包状态（运行中/停止/网卡） |
| `GET` | `/api/capture/interfaces` | 可用网卡列表 |
| `POST` | `/api/capture/start` | 启动抓包 `{interface:"en0"}` |
| `POST` | `/api/capture/stop` | 停止抓包 |

---

## 功能 2：代理拦截模式（自动/手动）

### 架构决策

| 决策项 | 选择 | 理由 |
|--------|------|------|
| 拦截实现 | goproxy OnRequest 返回 nil 暂停 | Handler 内阻塞等待用户决定 |
| 状态机 | Pending → Allowed/Blocked | 简单二态 + 超时 |
| 超时策略 | 手动模式 30s 后自动放过 | 避免浏览器卡死 |
| 前端通知 | WebSocket `intercept_request` 消息 | 复用现有 WS 通道 |
| 规则引擎 | Host/Path/Method 白名单 + 黑名单 | 自动模式下的精确控制 |

### 新增模块

```
internal/proxy/
  interceptor.go  # Interceptor — 拦截控制器
  rules.go        # RuleEngine — 规则匹配
```

### 拦截流程

```
浏览器 → 代理 → OnRequest
                   │
                   ├─ 模式=auto ──► RuleEngine.Match(req)
                   │                   │
                   │              ┌────┴────┐
                   │           match?    no match?
                   │              │          │
                   │          转发请求   放行(自动)
                   │
                   └─ 模式=manual ──► 推入待审队列 → WS通知前端
                                            │
                                      用户点击 Allow/Block
                                            │
                                     ┌──────┴──────┐
                                  Allow          Block
                                     │              │
                                 转发请求      返回 403
```

### 数据模型

```go
// PendingRequest 待审批请求
type PendingRequest struct {
    ID        string            `json:"id"`
    Method    string            `json:"method"`
    URL       string            `json:"url"`
    Host      string            `json:"host"`
    Headers   map[string]string `json:"headers"`
    Timestamp time.Time         `json:"timestamp"`
}

// InterceptAction 用户操作
type InterceptAction struct {
    RequestID string `json:"request_id"`
    Action    string `json:"action"` // "allow" | "block"
}

// InterceptRule 规则
type InterceptRule struct {
    ID      int64  `json:"id"`
    Pattern string `json:"pattern"` // host:port 或 url 匹配模式
    Type    string `json:"type"`    // "allow" | "block"
    Enabled bool   `json:"enabled"`
}
```

### 新增 API 端点

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/api/intercept/mode` | 当前模式 (`auto`/`manual`) |
| `POST` | `/api/intercept/mode` | 切换模式 `{mode:"manual"}` |
| `GET` | `/api/intercept/pending` | 待审批请求列表 |
| `POST` | `/api/intercept/action` | 审批操作 `{request_id, action}` |
| `GET` | `/api/intercept/rules` | 规则列表 |
| `POST` | `/api/intercept/rules` | 添加规则 |
| `DELETE` | `/api/intercept/rules/:id` | 删除规则 |

### WebSocket 消息

```json
// 新待审请求
{"type": "intercept_request", "data": {"id":"...", "method":"GET", "url":"...", "host":"..."}}

// 请求已过期
{"type": "intercept_timeout", "data": {"request_id":"..."}}
```

### 前端扩展

1. 顶部工具栏增加「模式切换」按钮：自动 ↔ 手动
2. 手动模式下弹出「待审队列」区域（底部或侧边栏）
3. 每个待审请求显示 Method/URL/Host + Allow/Block 按钮
4. 规则管理面板（可折叠）

---

## V3 项目结构总览

```
traffic-capture-tool/
├── cmd/proxy/main.go
├── internal/
│   ├── proxy/           # 代理核心 (v2)
│   │   ├── proxy.go
│   │   ├── mitm.go
│   │   ├── batch.go
│   │   ├── interceptor.go    # [new] 拦截控制器
│   │   └── rules.go          # [new] 规则引擎
│   ├── capture/              # [new] 网卡抓包模块
│   │   ├── engine.go
│   │   ├── tcp_stream.go
│   │   ├── http_extract.go
│   │   ├── process.go
│   │   ├── process_darwin.go
│   │   └── process_linux.go
│   ├── api/             # REST + WebSocket
│   │   ├── server.go
│   │   └── ws.go
│   ├── store/           # SQLite
│   │   └── store.go
│   └── models/
│       └── models.go
├── cmd/proxy/web/index.html
├── go.mod
├── go.sum
└── README.md
```

## 实施优先级

| 优先级 | 功能 | 原因 |
|--------|------|------|
| P0 | 拦截模式（自动/手动） | 纯内存逻辑，不改动现有架构 |
| P1 | 拦截规则引擎 | 自动模式核心，白名单/黑名单 |
| P2 | 前端拦截 UI | 待审队列 + 操作按钮 |
| P3 | gopacket 网卡抓包 | 需要 libpcap 依赖 |
| P4 | TCP 流重组 + HTTP 提取 | 复杂但核心 |
| P5 | 进程关联 (macOS) | lsof 轮询实现 |
| P6 | 进程关联 (Linux) | /proc 实现 |

## 风险与限制

| 风险 | 缓解 |
|------|------|
| gopacket 需要 libpcap C 依赖 | 使用 CGO_ENABLED=1 编译；提供 Homebrew 安装指引 |
| TCP 重组内存开销 | 限制并发流数 + 流超时清理 |
| 进程关联准确性 | 仅关联 IP:Port 映射，不保证 100% 准确 |
| 手动拦截阻塞浏览器 | 30s 超时自动放过，可配置 |
