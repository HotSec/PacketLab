# Changelog

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

