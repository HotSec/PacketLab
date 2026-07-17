# PacketLab V3 开发计划

> 状态：V3 ✅ 全部完成（v0.1.0）
> 最后更新：2026-07-17

---

## V2 → V3 状态总览

| 功能 | 状态 | 说明 |
|------|------|------|
| HTTP/HTTPS 代理捕获 | ✅ V2 | goproxy + MITM |
| 批量写入 (WAL) | ✅ V2 | 50条缓冲 / 200ms 刷新 |
| API 地图树形视图 | ✅ V2 | 站点树 + 方法着色 + 备注 |
| 编辑重发 | ✅ V2 | Method/URL/Headers/Body 可编辑 |
| i18n / 亮暗主题 | ✅ V2 | 中英文 / CSS 变量双主题 |
| WebSocket 实时推送 | ✅ V2 | 新请求即时同步 |
| 搜索与过滤 | ✅ V2 | 方法/method/host/错误筛选 |
| 拦截模式 | ✅ V3 已完成 | auto/manual + allow/drop/modify |
| 拦截规则引擎 | ✅ V3 已完成 | 通配匹配 + SQLite 持久化 |
| 拦截前端 UI | ✅ V3 已完成 | 待审入列表 + 编辑放过 |
| **网卡抓包** | ✅ V3 已完成 | gopacket + TCP 重组 + 进程关联 |
| **进程关联** | ✅ V3 已完成 | macOS lsof / Linux proc |

---

## 一、网卡流量捕获（核心 V3 功能）

### 1.1 目标

在现有代理模式之外，增加网卡抓包模式，直接从系统网络接口捕获所有 HTTP 流量。
可选关联请求到发起进程（名称 + PID）。

### 1.2 架构决策

| 决策 | 选择 | 理由 |
|------|------|------|
| 抓包库 | `google/gopacket` + `libpcap` (CGO) | macOS 预装，Linux 一行安装 |
| TCP 重组 | `gopacket/tcpassembly`，限 1000 并发流 | 内置重传/乱序处理 |
| HTTP 解析 | 自实现轻量状态机 | 只提取 Method/URL/Headers/Body |
| 进程关联 (macOS) | 连接建立时 `lsof -i tcp` 按端口查 | 按需查，不遗漏短连接 |
| 进程关联 (Linux) | `/proc/net/tcp` → inode → `/proc/<pid>/fd` → comm | 零外部依赖 |
| HTTPS 处理 | 只记录 CONNECT 握手(SNI)，不解密 | 私钥不在抓包侧 |

### 1.3 新增代码

```
internal/capture/
├── engine.go          # CaptureEngine — 生命周期、统计
├── stream.go          # TCP 流重组 (tcpassembly.StreamPool)
├── http_parse.go      # HTTP 请求/响应状态机提取
├── process.go         # ProcessResolver 接口 + LRU 缓存
├── process_darwin.go  # macOS lsof 实现
└── process_linux.go   # Linux /proc/net 实现
```

### 1.4 数据模型扩展

```go
// CapturedRequest 新增
Process      *ProcessInfo `json:"process,omitempty"`
CaptureMode  string       `json:"capture_mode"`   // "proxy" | "nic"

// 新增
type ProcessInfo struct {
    PID     int    `json:"pid"`
    Name    string `json:"name"`
    Cmdline string `json:"cmdline,omitempty"`
}

// 数据库迁移 — ALTER TABLE 新增
capture_mode TEXT DEFAULT 'proxy'
process_pid  INTEGER DEFAULT 0
process_name TEXT DEFAULT ''
```

### 1.5 新增 CLI

```
--capture                       启用网卡抓包
--capture-iface en0             指定网卡（默认自动检测）
--capture-bpf "..."             BPF 过滤（默认 tcp port 80 or tcp port 443）
--capture-max-streams           最大并发流数（默认 1000，超限 LRU 淘汰，最小 64）
--capture-ring-entries          网卡抓包环形缓冲区条目数（默认 262144，向上取 2 的幂）
--capture-stream-timeout        流空闲超时（分钟，默认 2）
--capture-no-proc               禁用进程关联
--intercept-pending-timeout     拦截器 pending 请求超时（Go duration，默认 15s，范围 1s~10m）
--cleanup-retention-days        自动清理保留天数（默认 7，0=用默认值）
--cleanup-interval              自动清理间隔（Go duration，默认 6h，最小 1m）
```

### 1.6 新增 API

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/api/capture/status` | 运行状态 + 统计（含 `packets`、`http`、`streams_evicted`、`ring_usage`、`ring_dropped`） |
| `GET` | `/api/capture/interfaces` | 可用网卡列表 |
| `POST` | `/api/capture/start` | 启动抓包 |
| `POST` | `/api/capture/stop` | 停止抓包 |

### 1.7 前端扩展

- 顶部栏新增「抓包」按钮（启用/停止）
- 网卡选择下拉框
- 请求列表「来源」列：PROXY / NIC + 进程名
- 请求详情进程信息行

### 1.8 实施步骤

| 步 | 内容 | 文件 | 预估 |
|----|------|------|------|
| 1 | 引入 gopacket + 数据库迁移 | go.mod, store.go | 30 行 |
| 2 | 模型扩展 | models.go | 15 行 |
| 3 | CaptureEngine + TCP 流重组 | engine.go, stream.go | 200 行 |
| 4 | HTTP 状态机提取 | http_parse.go | 120 行 |
| 5 | 进程关联 macOS | process_darwin.go | 60 行 |
| 6 | 进程关联 Linux | process_linux.go | 50 行 |
| 7 | 配置 + API 端点 | config.go, server.go | 120 行 |
| 8 | 前端 UI | app.js, style.css | 80 行 |

> 📋 详细设计见 [docs/NETWORK_CAPTURE.md](docs/NETWORK_CAPTURE.md)

---

## 二、拦截模式优化（V3 已实现，待完善）

| 项 | 当前状态 | 待完善 |
|----|---------|--------|
| 自动/手动切换 | ✅ | — |
| 待审入请求列表 | ✅ 黄色边框 + PENDING | — |
| Allow/Drop | ✅ | — |
| 修改后放过 | ✅ | Headers 批量编辑（resend 标签 kv-editor-row） |
| 规则引擎 | ✅ | 通配匹配 + method 维度匹配 |
| 规则管理 UI | ✅ | 拦截规则面板（增/删/改/启用切换 + 拦截日志查看） |
| 拦截日志 | ✅ | allow/drop/modify 操作记录到数据库 |

---

## 三、其他改进

### 3.1 响应体格式化 ✅

- JSON 响应体自动美化（`JSON.parse` + `JSON.stringify(obj, null, 2)`）
- 请求体同步美化

### 3.2 导出功能 ✅

- ✅ 单请求导出为 curl 命令（"copy as curl" 按钮）
- ✅ 批量导出为 HAR 文件
- ✅ 复制为 fetch / python-requests 格式

### 3.3 其他已完成

- ✅ 请求列表虚拟滚动（>1000 条时）
- ✅ 数据库定期清理（保留最近 N 天，`--cleanup-retention-days` / `--cleanup-interval` CLI）
- ✅ BPF 编译缓存

---

## 实施优先级

| P | 功能 | 依赖 | 复杂度 |
|----|------|------|--------|
| P0 | 网卡抓包引擎 (gopacket) | go get gopacket | ✅ |
| P1 | HTTP 提取 + 数据库存储 | engine 完成 | ✅ |
| P2 | 进程关联 | engine 完成 | ✅ |
| P3 | 前端抓包 UI | API 完成 | ✅ |
| P4 | 拦截规则管理 UI | — | ✅ |
| P5 | 导出 (curl/HAR) | — | ✅ curl + HAR |
| P6 | 响应体格式化 | — | ✅ |

---

## V3 完成总结（v0.1.0, 2026-07-17）

V3 计划全部交付：

- ✅ 网卡抓包引擎（gopacket + TCP 重组 + IPv6 + 4 worker 分流）
- ✅ 进程关联（macOS lsof / Linux proc / Windows netstat）
- ✅ 拦截规则前端 UI（V3-2A：增/删/改/启用切换 + 拦截日志查看）
- ✅ 待审请求 Headers 批量编辑器（V3-2B：resend 标签 kv-editor-row）
- ✅ `--capture-max-streams` CLI + LRU 淘汰（V3-1，默认 1000，最小 64）
- ✅ `--capture-ring-entries` CLI（环形缓冲区条目数，默认 262144）
- ✅ `--intercept-pending-timeout` CLI（V3-3，Go duration 格式，默认 15s，范围 1s~10m）
- ✅ `--cleanup-retention-days` / `--cleanup-interval` CLI（V3-4，首次启动写入 settings）
- ✅ 批量导出 HAR
- ✅ 复制为 fetch / python-requests
- ✅ 响应体 JSON 格式化
- ✅ 请求列表虚拟滚动
- ✅ 数据库定期清理
- ✅ BPF 编译缓存

### v0.1.0 稳定性增强（PR-2 + PR-3）

PR-2 修复 5 个 P1 稳定性 bug：
- MemRingBuffer 数据竞争（sync.Mutex + sync.Cond 替代 lock-free atomic）
- 锁顺序反转（统一 assembler.mu → streamPool.mu → TCPStream.mu）
- Interceptor goroutine 泄漏（sync.Once + sync.WaitGroup + sync.RWMutex）
- ListHosts 全表扫描（SQLite 查询缓存 + 分页 clamp）
- WebSocket Origin 校验（CORS 白名单 + CheckOrigin 闭包化）

PR-3 V3 功能补齐：
- `--capture-max-streams` CLI + Assembler LRU 淘汰 + flushEvictedStream 数据保全
- `--intercept-pending-timeout` CLI（NewInterceptor 改 time.Duration）
- `--cleanup-retention-days` / `--cleanup-interval` CLI
- 拦截规则管理面板 + Headers 批量编辑器（已在计划前实现，验收确认完整）
