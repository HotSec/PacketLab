# 网卡流量捕获 — 详细实施方案

> 状态：待开发 | 依赖：google/gopacket (CGO) | 复杂度：高

---

## 1. 目标

不依赖浏览器代理设置，直接从系统网卡抓取所有 HTTP 流量（进程无关），
并尽可能将每个 HTTP 请求关联到发起它的进程（名称 + PID）。

## 2. 核心决策

| 决策 | 选择 | 理由 |
|------|------|------|
| 抓包库 | `google/gopacket` + `libpcap` (CGO) | Go 生态最成熟，BPF 内核过滤 |
| 依赖安装 | macOS 预装；Linux `apt install libpcap-dev` | 一条命令即可 |
| TCP 重组 | `gopacket/tcpassembly`，限 1000 并发流 | 内置重传/乱序处理 |
| HTTP 解析 | 自实现有限状态机 | 只提取 Method/URL/Headers/Body，不解析完整语义 |
| 进程关联 | 新连接建立时立即查询 + 30s 缓存 | 按需查，不遗漏短连接 |
| HTTPS | 只记录 CONNECT 握手 `(encrypted)` | 无法解密（私钥不在抓包侧） |

## 3. 新增模块

```
internal/capture/
├── engine.go          # CaptureEngine — 生命周期、开始/停止、统计
├── stream.go          # TCP 流重组工厂 (tcpassembly.StreamPool)
├── http_parse.go      # HTTP 请求/响应解析（状态机，从重组流中提取）
├── process.go         # ProcessResolver 接口 + LRU 缓存
├── process_darwin.go  # macOS: lsof 实现
└── process_linux.go   # Linux: /proc/net 实现
```

### 3.1 engine.go — CaptureEngine

```go
type CaptureEngine struct {
    handle       *pcap.Handle
    assembler    *tcpassembly.Assembler
    streamPool   *tcpStreamPool
    procResolver ProcessResolver
    writer       *BatchWriter           // 复用现有批量写入
    iface        string
    bpf          string
    running      atomic.Bool
    stats        CaptureStats
}

type CaptureStats struct {
    PacketsReceived int64
    StreamsCreated  int64
    HTTPExtracted   int64
    ProcessHits     int64
}
```

**Start 流程：**
```
1. pcap.OpenLive(iface, 65536, true, pcap.BlockForever)
2. handle.SetBPFFilter(bpf)
3. assembler = tcpassembly.NewAssembler(tcpStreamPool)
4. go packetLoop()   — 循环读包 → assembler.AssembleWithContext()
5. go reassemblyGC() — 每 30s 清理过期流
```

**Stop 流程：**
```
1. running.Store(false)
2. assembler.FlushAll()
3. handle.Close()
```

### 3.2 stream.go — TCP 流重组

```go
// tcpStreamPool 实现 tcpassembly.StreamPool
// 每个新 TCP 连接创建一个 tcpStream
type tcpStreamPool struct {
    resolver  ProcessResolver
    writer    *BatchWriter
    maxStreams int // 默认 1000
}

type tcpStream struct {
    net, transport gopacket.Flow
    buf            bytes.Buffer
    start          time.Time
    proc           *ProcessInfo
}
```

**重组策略：**
- `Reassembled`: 数据到达 → 追加到 buf → 尝试提取 HTTP
- `ReassemblyComplete`: 流结束 → 最后一次提取 → 清理
- 流超时：30 分钟无新数据自动清理

### 3.3 http_parse.go — HTTP 提取

从重组后的 TCP 流（双向）提取 HTTP 请求和响应：

```go
type HTTPExtractor struct{}

// 状态机：
//   IDLE → 读取请求行 (START_LINE)
//   START_LINE → 读取请求头 (HEADERS)
//   HEADERS → Content-Length / chunked → 读取请求体 (BODY)
//   BODY → IDLE / START_LINE (keep-alive)

// 输出 CapturedRequest，字段映射：
func extractHTTP(src net.Addr, data []byte, proc *ProcessInfo) *CapturedRequest {
    // Method   ← 请求行第一个 token
    // URL      ← src + host header + path
    // Host     ← Host header
    // Path     ← 请求行 path
    // Headers  ← 解析到的 headers
    // Body     ← 请求体
    // Protocol ← HTTP/1.1
    // IsHTTPS  ← port == 443
    // Process  ← proc
}
```

**HTTPS 识别：**
- 端口 443 → `CapturedRequest{IsHTTPS: true, ReqBody: "(encrypted)"}`
- ClientHello 解析 SNI → 填入 Host 字段

### 3.4 process.go — 进程关联

```go
type ProcessResolver interface {
    Resolve(srcIP net.IP, srcPort uint16) (*ProcessInfo, error)
}

type ProcessInfo struct {
    PID     int    `json:"pid"`
    Name    string `json:"name"`
    Cmdline string `json:"cmdline,omitempty"`
}

// LRU 缓存：key = "ip:port"
type processCache struct {
    mu    sync.RWMutex
    items map[string]*cacheEntry
    // 30s TTL
}
```

### 3.5 process_darwin.go — macOS 实现

```go
func (r *darwinResolver) Resolve(srcIP net.IP, srcPort uint16) (*ProcessInfo, error) {
    // lsof -i tcp -n -P -sTCP:ESTABLISHED
    // 解析输出，按 "ip:port" 匹配
    // 格式：COMMAND   PID  USER  FD  TYPE  ...  NAME
    //       Chrome    1234 m5   42u IPv4 ...  localhost:54321->example.com:443
    // 
    // 从 NAME 字段解析 local:port → 匹配 srcPort
    // 提取 COMMAND + PID
}
```

### 3.6 process_linux.go — Linux 实现

```go
func (r *linuxResolver) Resolve(srcIP net.IP, srcPort uint16) (*ProcessInfo, error) {
    // 1. 读取 /proc/net/tcp → 找到 inode (按 local_address:port 匹配)
    // 2. 遍历 /proc/[0-9]*/fd/* → 找到 inode 匹配的 socket → PID
    // 3. 读取 /proc/<pid>/comm → 进程名
    // 4. 读取 /proc/<pid>/cmdline → 命令行
}
```

## 4. 数据模型扩展

```go
// CapturedRequest 新增字段
Process      *ProcessInfo `json:"process,omitempty"`
CaptureMode  string       `json:"capture_mode"`   // "proxy" | "nic"
Interface    string       `json:"interface,omitempty"`

// ProcessInfo 新增
type ProcessInfo struct {
    PID     int    `json:"pid"`
    Name    string `json:"name"`
    Cmdline string `json:"cmdline,omitempty"` // 不持久化，仅前端显示
}

// 数据库迁移（store.go）
ALTER TABLE requests ADD COLUMN capture_mode TEXT DEFAULT 'proxy';
ALTER TABLE requests ADD COLUMN interface TEXT DEFAULT '';
ALTER TABLE requests ADD COLUMN process_pid INTEGER DEFAULT 0;
ALTER TABLE requests ADD COLUMN process_name TEXT DEFAULT '';
```

## 5. CLI 参数

```bash
--capture                  # 启用网卡抓包
--capture-iface en0        # 指定网卡（默认: 自动检测第一个活跃网卡）
--capture-bpf "tcp port 80 or tcp port 443"  # BPF 过滤（默认）
--capture-max-streams 500  # 最大并发 TCP 流数（默认 1000）
--capture-no-proc          # 禁用进程关联
```

## 6. API 端点

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/api/capture/status` | `{"running":true,"iface":"en0","stats":{...}}` |
| `GET` | `/api/capture/interfaces` | `["en0","lo0","utun0"]` |
| `POST` | `/api/capture/start` | `{"interface":"en0","bpf":"tcp port 80 or tcp port 443"}` |
| `POST` | `/api/capture/stop` | `{"status":"stopped"}` |

## 7. 前端扩展

1. 顶部栏新增「抓包」按钮（启用/停止网卡抓包）
2. 启用后显示当前网卡名 + 已捕获 HTTP 请求数
3. 请求列表中新增「来源」列：代理 / 网卡 + 进程名
4. 请求详情中添加进程信息行（PID + Name）

## 8. 实施步骤（按优先级）

| 步骤 | 内容 | 产出文件 | 预估行数 |
|------|------|---------|---------|
| 1 | 引入 gopacket 依赖 | go.mod | 1 行 |
| 2 | 数据库迁移 4 字段 | store.go | 30 行 |
| 3 | 模型扩展 | models.go | 15 行 |
| 4 | CaptureEngine 架构 | engine.go | 120 行 |
| 5 | TCP 流重组 + HTTP 提取 | stream.go, http_parse.go | 250 行 |
| 6 | macOS 进程关联 | process_darwin.go | 60 行 |
| 7 | Linux 进程关联 | process_linux.go | 50 行 |
| 8 | API 端点 + 配置集成 | server.go, main.go, config.go | 120 行 |
| 9 | 前端 UI | index.html / app.js | 100 行 |

## 9. 边界与限制

| 场景 | 行为 |
|------|------|
| 无 root 权限 | macOS 预装 libpcap 有 SUID；Linux 需 sudo 或 `setcap cap_net_raw+ep` |
| lsof 失败 | `process_name = ""`，不阻塞抓包 |
| TCP 流超过 1000 | 丢弃最老的 idle 流 (LRU) |
| HTTPS 流量 | 记录 CONNECT，`req_body = "(encrypted)"`，不解析内容 |
| 分片/重传 | tcpassembly 自动处理 |
| 进程退出 | 缓存中的 ProcessInfo 保留（连接可能仍在） |
| 内存上限 | 每流 ≤ 2MB buffer，总 ≤ 1000 × 2MB = 2GB（可配置） |

## 10. 编译要求

```bash
# macOS
go build -o packetlab ./cmd/proxy/  # libpcap 预装

# Linux
sudo apt install libpcap-dev
go build -o packetlab ./cmd/proxy/

# 交叉编译（暂不支持，需 CGO）
# CGO_ENABLED=1 环境必须
```
