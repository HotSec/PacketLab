# SSE 流式捕获设计

## 问题

当前 PacketLab 无法正确捕获 SSE (Server-Sent Events) 形式的 HTTP 接口：

- **代理模式**：`OnResponse` 中 `io.ReadAll(resp.Body)` 阻塞等待 SSE 流结束，导致请求永远不被记录，代理卡住
- **网卡抓包模式**：`tryExtractHTTP` 等待完整 HTTP 响应（Content-Length 或 chunked 终止符），SSE 流永远不结束，请求不被 emit

## 方案：流式读取 + 定时增量更新

### 1. 代理模式改造

`internal/proxy/proxy.go` 的 `OnResponse` handler：

1. 检测 `resp.Header.Get("Content-Type")` 是否包含 `text/event-stream`
2. 如果是 SSE：
   - 同步保存初始记录（响应头 + 空 ResBody），拿到真实 ID
   - 用 `io.Pipe` 替代 `resp.Body`：goroutine 写入 pipe，客户端读取 pipe
   - goroutine 用 `bufio.Scanner` 逐行扫描 SSE 数据
   - 每 500ms 定时调用 `store.UpdateResBody` 更新 DB
   - 通过 WebSocket 推送 `update_request`（仅元数据）
   - scanner 循环结束后检查 `scanner.Err()` 并做最终更新
3. 如果不是 SSE：保持现有逻辑

### 2. 网卡抓包模式改造

`internal/capture/engine.go` 的 `tryExtractHTTP`：

1. 解析响应头后，检测 Content-Type 是否包含 `text/event-stream`
2. 如果是 SSE：
   - 同步保存初始记录（响应头 + 初始 body），拿到真实 ID
   - 标记 `stream.ssePending = true`
   - 后续 `Feed` 收到的服务端数据追加到 `sseBuf`
   - 每 4KB 增量调用 `flushSSEEvents` 更新 DB + WebSocket（仅元数据）
   - 连接关闭时 `HandleClose` 做最终 flush
3. 如果不是 SSE：保持现有逻辑

### 3. 数据模型变更

`CapturedRequest` 新增字段：

```go
IsSSE     bool   `json:"is_sse,omitempty"`     // 标记为 SSE 流
SSEEvents string `json:"sse_events,omitempty"`  // SSE 事件完整文本
```

### 4. WebSocket 推送扩展

新增 `update_request` 消息类型（仅推送元数据，body 由前端按需请求 API）：

```json
{"type": "new_request", "data": {...}}
{"type": "update_request", "data": {"id": 123, "status_code": 200, "duration_ms": 1500, "size_bytes": 4096}}
```

### 5. Store 层扩展

```go
func (s *Store) UpdateResBody(id int64, resBody string, sseEvents string, sizeBytes int64) error
```

### 6. 前端改造

`app.js` 处理 `update_request` 事件：
- 实时更新列表项的 size/duration/status_code
- 使用 `requestElCache` Map 做 O(1) DOM 查找
- `renderRequestList` 重新渲染时清空缓存

### 7. SSE 检测

```go
func isSSEResponse(resp *http.Response) bool  // 代理模式：检测 Content-Type
func isSSEHeader(headerData []byte) bool      // 网卡模式：检测 HTTP 响应头
```

### 8. 内存限制

| 限制项 | 值 |
|--------|-----|
| 代理模式 ReqBody 最大 | 2048 KB (2MB) |
| 代理模式 ResBody 最大 | 4096 KB (4MB) |
| 网卡模式 streamBufMax | 8 MB |
| 网卡模式 truncateBuffer keepSize | 1 MB |
| SSE 事件缓冲区上限 | 4 MB（超限截断 + Warn 日志） |

### 9. 代理模式 Pipe 数据流

```
resp.Body ──> Scanner ──> PipeWriter ──> 客户端（正常转发）
                  │
                  └──> 定时(500ms)更新 DB + WebSocket 推送
```

关键：`io.Pipe` 确保代理正常转发 SSE 数据的同时捕获内容，不阻塞客户端。

### 10. 分流路由

`cmd/proxy/main.go` 的 `onCapture` 回调根据 `IsSSE` 字段分流：

- SSE 更新 → `BroadcastUpdate` → `updateCh` → `update_request` 消息
- 普通请求 → `BroadcastCapture` → `broadcastCh` → `new_request` 消息

### 11. 涉及文件

| 文件 | 变更 |
|------|------|
| `internal/proxy/proxy.go` | OnResponse SSE 检测 + handleSSEResponse 流式处理 |
| `internal/capture/engine.go` | tryExtractHTTP SSE 检测 + flushSSEEvents + Feed SSE 追踪 |
| `internal/models/models.go` | 新增 IsSSE, SSEEvents 字段 |
| `internal/api/ws.go` | 新增 updateCh + BroadcastUpdate + update_request 广播 |
| `internal/api/server.go` | 新增 BroadcastUpdate 方法 |
| `internal/store/store.go` | 新增 UpdateResBody 方法 + v17/v18 迁移 |
| `cmd/proxy/web/app.js` | 处理 update_request 事件 + requestElCache |
| `internal/config/config.go` | DefaultMaxReqBodyKB/DefaultMaxResBodyKB 调大 |
| `cmd/proxy/main.go` | onCapture 分流 SSE/普通请求 |
| `internal/capture/e2e_test.go` | testHub 新增 BroadcastUpdate |
