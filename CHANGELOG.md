# Changelog

## v0.1.2 (2026-08-15) — LLM 流量分析重构 + 性能修复

### 🤖 LLM 流量分析重构（完整版）

**国内模型支持**（此前仅覆盖 OpenAI/Anthropic/Gemini 三大厂）：
- 新增内置厂商检测：**DeepSeek / Moonshot(Kimi) / 智谱 GLM / MiniMax / 阿里云 Qwen / xAI(Grok)**，全部按 host 精确/后缀匹配（防伪造 host 绕过）。(`internal/llm/provider.go`)
- 解析协议路由 `ProtocolFor`：国内厂商走 OpenAI 兼容协议，自定义端点统一归一化为 OpenAI 解析。(`internal/llm/parser.go`)
- 定价表以 **models.dev 官方 API 价为基准**（2026-08-15 同步，124 个模型条目）：
  - 修正 5 项过时价格（deepseek-chat/reasoner 已与 v4-flash 同价 $0.14/$0.28、o3-mini $1.10/$4.40、qwen3-max $1.20/$6.00、qwen3.7-plus $0.50/$3.00）
  - 新增 70+ 新世代模型（GPT-5.x 全系、Claude Sonnet/Opus 4.x-5、Gemini 3.x、GLM-4.x 全系、Kimi K2.x preview、Grok 4.x、Qwen3.8 等）
  - 历史模型 key 保留（merge 策略），`scripts/sync_pricing.py` 可随时重新同步
- 新增 `scripts/sync_pricing.py`：从 models.dev 仓库拉取官方价 → 对比 → merge 写入定价表（`--diff` 只报差异 / `--write` 落盘）

**成本/用量统计聚合**：
- 新增 `GET /api/llm/stats`：总量（交换次数、Prompt/Completion/Total tokens、估算成本）+ 按模型分布 + 按厂商分布（成本降序，SQLite `json_extract` 原生聚合）。(`internal/store/llm.go`, `internal/api/server.go`)
- `/api/llm` 列表支持 `provider` / `model` 精确过滤；列表项新增 token 用量与成本字段。
- 请求列表新增 `is_llm` 字段，前端 LLM 检测与后端单一事实来源对齐（移除前端硬编码 host 列表）。

**前端 AI 流量总览**：
- 顶栏新增「🤖 AI总览」按钮：打开全局仪表盘（5 张总览卡片 + 按模型分布表含成本占比条 + 按厂商分布表）。(`cmd/proxy/web/*`)
- 请求列表新增「🤖 AI」筛选 chip，一键只看 LLM 流量；条目加 🤖 徽标。
- provider 徽标新增国内厂商配色；成本显示精度 4 → 6 位小数。

### 🔧 性能修复（08-09 未发布修复补发）

- `HandleClose` O(n) 遍历修复、SSE 缓冲无上限修复等 5 项（`internal/capture/engine.go`, `internal/proxy/proxy.go`）
- `ForEachFull` 缺列、`Clear` 残留、HAR 超时截断等 5 项必须修复（`internal/store/store.go`, `internal/api/server.go`, `cmd/proxy/main.go`）

## v0.1.1 (2026-08-05) — 安全修复（默认鉴权 + CSRF 防护）

修复默认安装无 API 鉴权导致本机任意进程可读取解密流量、恶意网页可跨站重放请求的问题（Security Advisory GHSA-gp8p-c8gg-x422）。

### 🔴 安全修复（P0）

- **默认启用 API 鉴权**：未设置 `--api-token` / `PACKETLAB_API_TOKEN` 时自动生成随机 token，写入 `~/.packetlab/token`（0600）。Web 界面首次访问需输入 token。(`internal/config/config.go`, `cmd/proxy/main.go`)
- **CSRF 防护**：状态变更请求（非 GET/HEAD/OPTIONS）必须携带 `X-Requested-With: XMLHttpRequest` 自定义头，阻止恶意网页跨站调用 `/api/resend`、`/api/intercept/*`、`/api/clear` 等端点。(`internal/api/middleware.go`, `internal/api/server.go`, `cmd/proxy/web/app.js`)
- 新增 `SECURITY.md` 安全政策与漏洞报告渠道。

### 影响

所有 v0.1.0 及更早版本默认配置下 API 无鉴权。升级到 v0.1.1 后首次访问 Web 界面需从 `~/.packetlab/token` 获取 token。

# Changelog

## v0.1.0 (2026-07-17) — 稳定性、安全、V3 完整交付

v0.0.2 → v0.1.0 是稳定性延续版本，无破坏性变更，可直接替换二进制升级。

### 🔴 Bug 修复（数据正确性 P0）

- **代理转发请求体被截断**：`io.LimitReader` 截断后替换 `req.Body` 转发，10MB POST 只到 2MB。改为 ReadAll 全量转发 + 截断存储，设置 `req.GetBody`。(`internal/proxy/proxy.go`)
- **代理转发响应体被截断**：响应侧同理，ReadAll 全量转发，存储侧截断。(`internal/proxy/proxy.go`)
- **SaveBatch 部分失败时 ids 误用**：tx.Rollback 后仍返回 ids，调用方触发 onSave 推送无效 ID。失败时返回 nil ids，调用方不调用 onSave。(`internal/store/store.go`, `internal/proxy/batch.go`)
- **shouldMITM 端口后缀匹配失效**：`host:443` 永远不匹配 `.wns.windows.com`。先 `net.SplitHostPort` 去端口再匹配。(`internal/proxy/proxy.go`)
- **manual 模式 readBody 返回空字符串**：`NopCloser` 无 `GetBody`，readBody 走 false 分支返回 `""`。OnRequest 设置 `req.GetBody`，readBody 优先使用。(`internal/proxy/interceptor.go`, `internal/proxy/proxy.go`)

### 🟠 Bug 修复（稳定性与安全 P1）

- **MemRingBuffer 数据竞争**：lock-free 设计在多生产者场景存在 race。改用 `sync.Mutex` + `sync.Cond`。(`internal/capture/memring.go`)
- **HandleClose / FlushOlderThan 锁顺序相反**：跨 worker 并发死锁。统一锁顺序为 `assembler.mu → streamPool.mu → TCPStream.mu`。(`internal/capture/engine.go`)
- **Interceptor logCh goroutine 泄漏**：`for range it.logCh` 永远阻塞。新增 `Stop()` 用 `sync.Once` 保护，先 `close(logCh)` 再 `wg.Wait()`。(`internal/proxy/interceptor.go`, `cmd/proxy/main.go`)
- **ListHosts 全表扫描两次**：合并为单次 GROUP BY + 强制分页 + 5 分钟 TTL 缓存。(`internal/store/store.go`)
- **WebSocket 空 Origin 被允许**：`isAllowedOrigin("")` 返回 true。改为 false，新增 `--api-allow-origins` CLI。(`internal/api/middleware.go`)
- **handleWebSocket Upgrade 失败重复写**：gorilla/websocket Upgrade 失败时已写 HTTP 响应，不应再 `http.Error`。(`internal/api/server.go`)
- **AsyncWriterPool.flushAll 无重试退避**：SaveBatch 失败立即重试，雪崩式重试。新增 50/100/200ms 指数退避 + stopCh 早退。(`internal/capture/memring.go`)
- **emitNonBlocking HTTPFound 统计不一致**：ring buffer drop 或 save 失败导致漏计。入口立即计数，bulkEmit 不再重复。(`internal/capture/engine.go`)
- **packetLoop / Stop 并发安全**：Stop 期间 packetLoop 可能 panic。改为 defer 清理 + workerChs 快照 + 锁释放后阻塞操作。(`internal/capture/engine_pcap.go`, `internal/capture/engine.go`)

### 🟢 V3 功能补齐

- **`--capture-max-streams` CLI + LRU 淘汰**：默认 1000，超限 LRU 淘汰 + flushEvictedStream 数据保全，新增 `streams_evicted` 指标。(`internal/capture/engine.go`, `internal/config/config.go`)
- **`--capture-ring-entries` CLI**：环形缓冲区条目数参数化（默认 262144，向上取 2 的幂）。(`internal/capture/memring.go`, `internal/config/config.go`)
- **拦截规则前端 UI**：顶栏盾牌按钮打开模态面板，支持增/删/改/启用切换 + 拦截日志查看。(`cmd/proxy/web/*`)
- **待审请求 Headers 批量编辑器**：resend 标签 kv-editor-row，支持新增/删除/编辑 header 行。(`cmd/proxy/web/*`)
- **`--intercept-pending-timeout` CLI**：原 15s 硬编码，改为 Go duration 格式可配（1s~10m）。NewInterceptor 签名改 `time.Duration`。(`internal/proxy/interceptor.go`, `internal/config/config.go`)
- **`--cleanup-retention-days` / `--cleanup-interval` CLI**：暴露 retention 与 interval 为 CLI，首次启动写入 settings。(`cmd/proxy/main.go`, `internal/config/config.go`)

### 🟢 LLM 增强（阶段三）

- **LLM AI 对话视图**：捕获到 LLM API 调用时自动显示 🤖 AI对话 tab，渲染对话消息（system/user/assistant 角色）+ provider badge（OpenAI/Anthropic/Gemini）+ token usage + 复制提示词按钮。(`cmd/proxy/web/*`)
- **自定义 OpenAI 兼容端点识别**：新增 `RegisterCustomEndpoint`/`UnregisterCustomEndpoint`/`ListCustomEndpoints` API，支持 host 完全/后缀匹配 + path 前缀匹配。可识别 DeepSeek/Moonshot/本地 vLLM 等 OpenAI 兼容端点。(`internal/llm/custom_endpoints.go`)
- **Tool/Function Calling 提取**：解析请求 `tools` 定义（OpenAI/Anthropic/Gemini 三家）+ 响应 `tool_calls`/`tool_use`/`functionCall`，支持 OpenAI 流式 tool_calls 增量拼接（index 索引）和 Anthropic 流式 `content_block_start`/`input_json_delta` 拼接。(`internal/llm/parser.go`)
- **LLM 成本估算**：内置 19 个模型定价表（OpenAI 9 + Anthropic 5 + Gemini 4），按 token 用量估算 USD 成本，最长前缀匹配（`gpt-4o-mini` 优先于 `gpt-4o`），大小写不敏感，未命中返回 0。前端显示 `Cost $x.xxxx`。(`internal/llm/pricing.go`)

### 🔵 工程化

- **CI 流水线**：新增 `.github/workflows/ci.yml`（push + PR 触发，lint + test -race + build + 覆盖率上报 codecov）。
- **golangci-lint 规则**：新增 `.golangci.yml`（errcheck/govet/staticcheck/unused/misspell/gofmt/goimports）。
- **release.yml 增强**：新增 Docker image 推送 ghcr（多平台 build → Docker buildx）。
- **测试覆盖**：新增 6 个测试函数覆盖 `mitm.go`（CA 生成/加载）、`batch.go`（ID 回填）、`ws.go`（多客户端广播/缓冲满/Stop 关闭）盲区。

### 🟣 重构

- **MemRingBuffer 容量参数化**：`--capture-ring-entries` CLI，向上取 2 的幂次。
- **handleListRequests limit clamp**：`limit > 200` 时 clamp 到 200（而非重置为 50）。
- **NewInterceptor 签名现代化**：`timeoutSec int` → `timeout time.Duration`。
- **startAutoCleanup 参数化**：接受 `retentionDays + interval` 而非硬编码。

### 迁移

无新增 schema（v0.0.2 已到 v21）。

---

## v0.0.2 (2026-06-14) — 稳定性、功能完善与性能优化

### 🔴 Bug 修复（数据正确性）

- **Resend 重试 body 丢失**：重试 / 5xx 重试时复用已消费的 body，POST/PUT 重发在网络抖动下请求体静默丢失。改为每次重试用 `bytes.NewReader(bodyBytes)` 重建请求。(`internal/api/service.go`)
- **dbRO 只读连接长期闲置 + 读路径加锁**：打开的只读 DB 连接从未被使用，所有读查询仍走写连接并持有 `s.mu.RLock()`。新增 `readDB()` 辅助，所有读方法（List/Get/ListFull/Stats/GetAPINotes/GetAPIMap/ListHosts/ListInterceptLogs/GetSetting/ListRules）改走 dbRO 并去掉读锁，真正利用 WAL 一写多读。(`internal/store/store.go`)
- **网卡抓包完全不支持 IPv6**：`Assembler.Assemble` / `flowHash` 只解析 IPv4 层，IPv6 流量被丢弃。新增 `layers.LayerTypeIPv6` 分支与 `foldIPv6` 哈希折叠。(`internal/capture/engine.go`)
- **SSE 增量推送内容缺失**：`update_request` WebSocket 消息只推送 id/status/size，前端无法实时看到 SSE 事件。补全 `res_body`/`sse_events` 字段并在查看中请求时实时刷新响应体。(`internal/api/ws.go`, `cmd/proxy/web/app.js`)

### 🟢 功能完善

- **数据库定期清理**：补全 `Store.Cleanup` + `/api/maintenance/cleanup` 端点 + 后台每 6 小时自动清理（`retention_days` 由 settings 控制）。(`internal/store`, `internal/api`, `cmd/proxy/main.go`)
- **进程缓存淘汰**：`procCache` 只增不减导致长跑内存泄漏。新增 TTL（30s）+ 容量上限（10000）整体重建淘汰，并提取 `resolveProcessCached` 统一 unix/windows。(`internal/capture/engine.go`, `proc_unix.go`, `proc_windows.go`)
- **请求列表虚拟滚动**：超过 200 条时启用 windowing（50px/项 + 8 overscan + rAF 节流），万条记录流畅滚动。(`cmd/proxy/web/app.js`)
- **拦截规则 method 维度匹配**：规则支持限定 HTTP 方法（逗号分隔多个），大小写不敏感。含 DB 迁移 v19、`matchRule` 重构、测试覆盖。(`internal/store`, `internal/proxy/interceptor.go`)
- **复制为 fetch / python-requests**：在 `copy as curl` 旁新增 `fetch`、`python` 一键复制。(`cmd/proxy/web/app.js`, `index.html`)
- **拦截日志按 host/pattern 过滤**：`ListInterceptLogs` 新增 host/pattern LIKE 模糊匹配参数 + 前端防抖过滤输入框。(`internal/store`, `internal/api`, `cmd/proxy/web/app.js`)
- **请求收藏（starred）**：DB 迁移 v20/v21 + `SetStarred`/`ListStarred` + `/api/starred` 端点 + 列表项星标按钮 + 顶栏「仅显示收藏」筛选。(`internal/store`, `internal/api`, `cmd/proxy/web/*`)

### 🟡 性能与配置

- **BPF 编译缓存**：包级 `sync.Map` 缓存 `pcap.BPFInstruction`，启停抓包不重复编译。(`internal/capture/engine_pcap.go`)
- **pcap 内核缓冲调优**：用 `InactiveHandle` 配置 32MB buffer + 16384 snaplen，降低高吞吐丢包。(`internal/capture/engine_pcap.go`)
- **抓包流超时参数化**：新增 `--capture-stream-timeout` flag（默认 2 分钟），GC 周期取其 1/8。(`internal/config`, `internal/capture`, `cmd/proxy/main.go`)
- **代理/网卡截断阈值统一**：网卡侧 `truncateBuffer` 与 SSE 缓冲区上限改用 `config.MaxResBodyKB`（默认 4MB），与代理侧一致。(`internal/capture/engine.go`)
- **CORS Origin 白名单修正**：`isAllowedOrigin` 允许 localhost/回环任意端口 + https 本地源（修复 `http://localhost:9090` 被拒的回归）。(`internal/api/middleware.go`)

### 🔵 工程化

- **Dockerfile**：多阶段构建（golang:1.25-bookworm → debian-slim），含 libpcap 运行库，数据卷持久化。

### 迁移

- `ALTER TABLE intercept_rules ADD COLUMN method` (v19)
- `ALTER TABLE requests ADD COLUMN starred` (v20)
- `CREATE INDEX idx_requests_starred` (v21)

---

## v0.0.1 (2026-05-22) — First Tagged Release

> PacketLab 首个正式版本 — MITM 代理、API 地图、拦截编辑重发，即开即用。

### Highlights

| 模块 | 核心能力 |
|------|----------|
| **代理捕获** | HTTP/HTTPS 全流量记录（Method、URL、Headers、Body、耗时、大小、状态码） |
| **HTTPS MITM** | 自签名 CA 证书解密，一键安装引导，可配置 MITM 排除列表 |
| **拦截模式** | 自动放过 / 手动审批（Allow / Drop / Modify），待审批黄色标记 |
| **编辑重发** | 修改 Method/URL/Headers/Body 后通过代理重新发送 |
| **API 地图** | 按站点树形展示全部端点，方法颜色区分，状态码标记，添加备注，右键菜单 |
| **实时推送** | WebSocket 将新请求即时同步到前端界面 |
| **批量写入** | WAL 模式 SQLite，50 条缓冲区 200ms 批量刷新 |
| **搜索过滤** | 按方法/URL/状态码/Host 搜索；错误筛选；搜索式 host 下拉 |
| **i18n** | 中文 / English 界面切换 |
| **明暗主题** | CSS 变量双主题，localStorage 持久化，平滑过渡 |
| **交互细节** | 交错入场动画、Tab 滑动指示器、可拖拽分隔面板 |

### Commits (20)

```
a3d1b66 docs: 网卡抓包详细实施方案
ae7eeaf docs: 开源项目文档
991ea68 feat: API地图点击用host+路径搜索 + 列表动画柔和化
ab26c74 fix: API地图多层展开时子路径被父级遮挡
e38c855 fix: 7项审查修复 — 去重/日志/初始化/过滤/错误处理
44b5d60 fix: 拦截修改后放行不生效
a8604b2 fix: 请求记录不写入SQLite — 捕获代码在拦截器分支后永不执行
e7d66dd fix: 修复 SQLite 打开时 DSN 参数导致磁盘 I/O 错误
44ee409 feat: 不再记录 CONNECT 隧道请求
b2940a9 fix: 最终优化 — 4项修复
2bbafce fix: 端到端审查 — 修复3个问题
60c702e fix: API地图显示网站全部URL — host带端口导致数据分裂
661837d fix: API地图显示 / 节点 + 根节点自身方法
952aa49 fix: API地图根节点 leaf 不显示 — renderTreeNode 只遍历children
20d84b2 fix: API地图支持显示 / 路径的 CONNECT 请求
78aeb8c fix: API地图树形结构children数据不全 — 两个关键bug
96844a9 fix: API地图 host 带端口(:443)时无数据
deadbfb fix: 修复API地图树形视图不显示
45db165 fix: 修复API地图数据不显示 + 移除加载更多站点
fd41aa8 fix: 修复点击请求记录无响应 - ID类型不匹配
```

### Quick Start

```bash
# 克隆 & 构建
git clone https://github.com/user/packetlab.git
cd packetlab
go mod download
go build -o packetlab ./cmd/proxy
./packetlab

# 代理默认端口 :8080，Web UI 默认 :8081
# 安装 CA 证书: ~/.packetlab/certs/ca.crt
```

