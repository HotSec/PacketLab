# PacketLab V3 开发计划 (v2 — 已审查修复)

## 概览

两个核心新功能：
1. **网卡流量捕获** — 直接从网络接口抓包，解析 HTTP 请求，关联进程信息
2. **代理拦截模式** — 自动放过 / 手动审批 双模式

---

## 功能 1：网卡流量捕获 + 进程关联

> 📋 详细实施方案见 [docs/NETWORK_CAPTURE.md](docs/NETWORK_CAPTURE.md)

### 架构决策

| 决策项 | 选择 | 理由 |
|--------|------|------|
| 抓包库 | `google/gopacket` + `pcap` (CGO) | macOS 预装 libpcap，Linux `apt install libpcap-dev` |
| TCP 重组 | `gopacket/tcpassembly`，限制 1000 并发流 | 内置重组，`StreamPool` 控制内存 |
| HTTP 解析 | 自实现轻量解析器 | 只需 Method/URL/Headers/Body |
| 进程关联 | **连接建立时立即查询** `lsof -i tcp -n -P -p <PID>` | ~~不支持轮询~~ 按需查，不遗漏短连接 |
| 进程缓存 | `sync.Map[addr]ProcInfo`，30s TTL | 避免频繁 fork lsof |
| 降级模式 | 进程关联失败 → 留空，不影响抓包 | 非关键功能 |

### 新增模块

```
internal/capture/
  engine.go         # CaptureEngine — 主入口，管理抓包生命周期
  tcp_stream.go     # TCP 流重组工厂 (tcpassembly.StreamPool)
  http_extract.go   # HTTP 请求/响应从 TCP 流中提取
  process.go        # ProcessResolver 接口 + 缓存
  process_darwin.go # macOS lsof 实现 (OnDemand)
  process_linux.go  # Linux /proc/net 实现 (OnDemand)
```

### 数据模型扩展

```go
// models.go 新增
type ProcessInfo struct {
    PID     int    `json:"pid"`
    Name    string `json:"name"`
    Cmdline string `json:"cmdline,omitempty"`
}

// CapturedRequest 新增字段
Process      *ProcessInfo `json:"process,omitempty"`
CaptureMode  string       `json:"capture_mode"` // "proxy" | "nic"
Interface    string       `json:"interface,omitempty"`
```

### 进程关联流程（已修复）

```
新连接到达(srcIP:srcPort, dstIP:dstPort)
  │
  ├─ ProcessCache.Get(addr) → 命中? 直接返回
  │
  └─ 未命中:
       ├─ macOS: lsof -i tcp -n -P -sTCP:ESTABLISHED | grep <port>
       ├─ Linux: 读 /proc/net/tcp → inode → /proc/*/fd/* → comm
       └─ 结果存入 ProcessCache (TTL 30s)
```

### 新增 CLI 参数

```
--capture              启用网卡抓包
--capture-iface en0    指定网卡（默认自动检测活跃网卡）
--capture-bpf "..."    BPF 过滤（默认 tcp port 80 or tcp port 443）
```

### 关键技术点

1. **HTTPS 限制**：网卡抓包只看 CONNECT 握手 (SNI hostname)，不解密。记录标注 `(encrypted)`。
2. **按需查进程**：连接建立时立即查询，不依赖轮询。短连接不会遗漏。
3. **BPF 内核过滤**：用户态仅处理 HTTP 端口数据，降低 CPU 开销。
4. **流池限制**：最多 1000 并发 TCP 流，超出丢弃最老的 idle 流。

### 新增 API 端点

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/api/capture/status` | 抓包状态、网卡名、已捕获数 |
| `GET` | `/api/capture/interfaces` | 可用网卡列表 (ifconfig) |
| `POST` | `/api/capture/start` | 启动 `{interface:"en0", bpf:"..."}` |
| `POST` | `/api/capture/stop` | 停止抓包 |

---


## 功能 2：代理拦截模式（自动放过 / 手动审批<放过|丢弃|修改后放过>）

### 架构决策

| 决策项 | 选择 | 理由 |
|--------|------|------|
| 拦截实现 | **channel 阻塞等待**，携带 Action 决定 | Handler 内用 `select` 阻塞，API 写入 channel 唤醒 |
| 手动操作 | **Allow / Drop / Modify&Allow** | 放过 / 丢弃 / 修改后放过 |
| 状态机 | Pending → Allowed / Dropped / Modified / Timeout | 四终态 |
| 超时策略 | **15s 后自动放过**（可配） | 浏览器 30s 留余量 |
| 持久化 | 规则存 SQLite `intercept_rules` 表 | 重启不丢失 |
| 模式持久化 | SQLite `settings` 表 | 重启记忆 |

### 新增模块

```
internal/proxy/
  interceptor.go  # Interceptor — 拦截控制器
  rules.go        # RuleEngine — 规则匹配
```

### 拦截核心实现

```go
// 三种操作结果
type InterceptResult struct {
    Action      string            // "allow" | "drop" | "modify"
    Method      string            // modify 时的新方法
    URL         string            // modify 时的新 URL
    NewHeaders  map[string]string // modify 时的新请求头
    NewBody     string            // modify 时的新请求体
}

type pendingReq struct {
    req    *http.Request
    result chan InterceptResult
    timer  *time.Timer
}

// OnRequest handler 中
if mode == "manual" {
    ch := make(chan InterceptResult, 1)
    interceptor.enqueue(req, ch)
    select {
    case r := <-ch:
        switch r.Action {
        case "allow":
            // 转发原始请求
        case "drop":
            return nil, goproxy.NewResponse(req, "text/plain", 403, "Blocked by PacketLab")
        case "modify":
            newReq, _ := http.NewRequest(r.Method, r.URL, strings.NewReader(r.NewBody))
            for k, v := range r.NewHeaders { newReq.Header.Set(k, v) }
            return newReq, nil
        }
    case <-time.After(15*time.Second):
        // 超时自动放过
    }
}
```

### 手动操作三种方式

| 操作 | 前端按钮 | 后端行为 |
|------|---------|---------|
| **放过 (Allow)** | 绿色「放行」 | 原始请求直接转发到目标服务器 |
| **丢弃 (Drop)** | 红色「丢弃」 | 返回 403，不发送到服务器 |
| **修改后放过 (Modify)** | 蓝色「修改并发送」 | 弹出编辑面板 → 修改后构造新请求转发 |

### 拦截流程

```
浏览器 → 代理 → OnRequest
                   │
                   ├─ mode=auto ──► RuleEngine(host, path, method)
                   │                   │
                   │              ┌────┴────┐
                   │           match    no match
                   │              │         │
                   │           规则动作   放行
                   │
                   └─ mode=manual ──► pendingReq{ch, 15s timer}
                                          │
                                     WS notify 前端
                                          │
                               ┌──────────┼──────────┐
                               │          │          │
                            Allow       Drop     Modify
                               │          │          │
                            转发请求  返回 403  弹出编辑器
                                                   │
                                              修改 Method/URL
                                              修改 Headers/Body
                                                   │
                                               转发新请求
```

### 数据模型

```go
// InterceptResult — 用户操作
type InterceptResult struct {
    RequestID  string            `json:"request_id"`
    Action     string            `json:"action"`    // "allow" | "drop" | "modify"
    Method     string            `json:"method,omitempty"`
    URL        string            `json:"url,omitempty"`
    NewHeaders map[string]string `json:"new_headers,omitempty"`
    NewBody    string            `json:"new_body,omitempty"`
}

// PendingRequest — 待审请求（内存）
type PendingRequest struct {
    ID        string            `json:"id"`
    Method    string            `json:"method"`
    URL       string            `json:"url"`
    Host      string            `json:"host"`
    Path      string            `json:"path"`
    Headers   map[string]string `json:"headers"`
    Body      string            `json:"body"`
    Timestamp time.Time         `json:"timestamp"`
    Age       float64           `json:"age_sec"`
}

// InterceptRule — 持久化规则
type InterceptRule struct {
    ID        int64     `json:"id"`
    Pattern   string    `json:"pattern"` // "example.com" | "*.example.com/api/*"
    Action    string    `json:"action"`  // "allow" | "block"
    Enabled   bool      `json:"enabled"`
    CreatedAt time.Time `json:"created_at"`
}
```

### 新增 API 端点

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/api/intercept/mode` | `{"mode":"auto"}` |
| `POST` | `/api/intercept/mode` | 切换 `{"mode":"manual"}` |
| `GET` | `/api/intercept/pending` | `[{id,method,url,host,path,headers,body,age_sec}]` |
| `POST` | `/api/intercept/action` | `{"request_id":"x","action":"allow\|drop\|modify","method":"...","url":"...","new_headers":{},"new_body":"..."}` |
| `GET` | `/api/intercept/rules` | 规则列表 |
| `POST` | `/api/intercept/rules` | 添加 `{"pattern":"...","action":"allow\|block"}` |
| `PUT` | `/api/intercept/rules/:id` | 更新 `{"enabled":false}` |
| `DELETE` | `/api/intercept/rules/:id` | 删除 |

### WebSocket 消息

```json
{"type":"intercept_request","data":{"id":"req_abc","method":"POST","url":"https://api.example.com/v1/orders","host":"api.example.com","path":"/v1/orders","headers":{"Content-Type":"application/json"},"body":"{\"qty\":1}"}}
{"type":"intercept_resolved","data":{"request_id":"req_abc","action":"modify"}}
{"type":"intercept_timeout","data":{"request_id":"req_abc"}}
```

### 前端扩展

1. 顶部工具栏「模式切换」：自动 ↔ 手动
2. 手动模式下右侧面板顶部出现「待审队列」区域
3. 待审项显示 Method 徽章 + URL + Host + 倒计时进度条
4. 三个按钮：**允许**（绿）/ **丢弃**（红）/ **修改并发送**（蓝）
5. 点击「修改并发送」→ 展开编辑面板（Method/URL/Headers/Body 可编辑）
6. 15s 超时自动放过并移除

### 数据库迁移

```sql
CREATE TABLE IF NOT EXISTS intercept_rules (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    pattern TEXT NOT NULL,
    action  TEXT NOT NULL DEFAULT 'allow',  -- allow | block
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

INSERT OR IGNORE INTO settings (key, value) VALUES ('intercept_mode', 'auto');
```

---
## V3 项目结构总览

```
traffic-capture-tool/
├── cmd/proxy/main.go
├── internal/
│   ├── proxy/              # 代理核心
│   │   ├── proxy.go
│   │   ├── mitm.go
│   │   ├── batch.go
│   │   ├── interceptor.go  # [new] 拦截控制器 (channel 阻塞)
│   │   └── rules.go        # [new] 规则引擎 (通配匹配)
│   ├── capture/            # [new] 网卡抓包
│   │   ├── engine.go
│   │   ├── tcp_stream.go   # TCP 流重组 (tcpassembly)
│   │   ├── http_extract.go
│   │   ├── process.go      # ProcessResolver 接口 + 缓存
│   │   ├── process_darwin.go
│   │   └── process_linux.go
│   ├── api/
│   │   ├── server.go
│   │   └── ws.go
│   ├── store/
│   │   └── store.go        # + intercept_rules + settings 表
│   └── models/
│       └── models.go       # + ProcessInfo, InterceptRule, PendingRequest
├── cmd/proxy/web/index.html
├── go.mod / go.sum
├── README.md
└── V3_PLAN.md
```

## 实施优先级

| 优先级 | 功能 | 依赖 | 预估 |
|--------|------|------|------|
| P0 | 拦截控制器 (channel阻塞) | 无 | 200行 |
| P0 | `settings` 表 + 模式持久化 | store.go migration | 30行 |
| P1 | 规则引擎 + `intercept_rules` 表 | store.go CRUD | 150行 |
| P1 | 拦截 API 端点 | server.go 新增路由 | 100行 |
| P2 | WebSocket 推送待审请求 | ws.go 新增消息类型 | 50行 |
| P2 | 前端拦截 UI (待审面板) | index.html | 200行 |
| P3 | gopacket 引擎 + TCP 重组 | go get gopacket | 400行 |
| P4 | HTTP 流提取 | 自实现解析器 | 200行 |
| P5 | 进程关联 (macOS + Linux) | lsof | 150行 |

## 与 V2 相比的关键修复

| 问题 | V2 原方案 | V2 修复后 |
|------|----------|----------|
| 拦截实现 | `return nil → 拒绝` | channel 阻塞 + 超时放过 |
| 超时 | 30s | 15s (浏览器 30s 留余量) |
| 进程关联 | 5s 轮询错过短连接 | 连接时即时查询 + 30s 缓存 |
| 规则持久化 | 未提及 | SQLite `intercept_rules` 表 |
| 模式持久化 | 未提及 | SQLite `settings` 表 |
| 规则匹配 | 未定义 | 精确 → 通配 → 前缀 优先级 |
