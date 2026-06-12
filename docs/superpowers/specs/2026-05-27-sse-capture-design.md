# SSE 流式捕获设计

## 问题

当前 PacketLab 无法正确捕获 SSE (Server-Sent Events) 形式的 HTTP 接口：

- **代理模式**：`OnResponse` 中 `io.ReadAll(resp.Body)` 阻塞等待 SSE 流结束，导致请求永远不被记录，代理卡住
- **网卡抓包模式**：`tryExtractHTTP` 等待完整 HTTP 响应（Content-Length 或 chunked 终止符），SSE 流永远不结束，请求不被 emit

## 方案：流式读取 + 实时追加事件

### 1. 代理模式改造

`internal/proxy/proxy.go` 的 `OnResponse` handler：

1. 检测 `resp.Header.Get("Content-Type")` 是否包含 `text/event-stream`
2. 如果是 SSE：
   - 立即记录请求（响应头 + 空 ResBody），入队 batchWriter 写入 DB
   - 用 `io.TeeReader` 包装 `resp.Body`：一份数据写入 `io.PipeWriter` 转发给客户端，另一份供 Scanner 读取
   - 启动 goroutine 用 `bufio.Scanner` 逐行扫描 SSE 事件
   - 每识别到一个完整 SSE 事件（空行分隔）：
     - 追加到 `captured.ResBody`
     - 调用 `store.UpdateResBody` 更新 DB
     - 通过 WebSocket 推送 `update_request` 事件
   - 连接关闭时停止 goroutine
3. 如果不是 SSE：保持现有逻辑

### 2. 网卡抓包模式改造

`internal/capture/engine.go` 的 `tryExtractHTTP`：

1. 解析响应头后，检测 Content-Type 是否包含 `text/event-stream`
2. 如果是 SSE：
   - 立即 emit 记录（只有响应头，ResBody=""）
   - 标记 `stream.sseStream = true`
   - 后续 `Feed` 收到的服务端数据走 `tryExtractSSEEvent`：
     - 从 `serverBuf` 中识别完整 SSE 事件（空行分隔）
     - 每识别到一个事件，追加到已 emit 的记录
     - 调用 `store.UpdateResBody` 更新 DB
     - 通过 WebSocket 推送 `update_request`
3. 如果不是 SSE：保持现有逻辑

### 3. 数据模型变更

`CapturedRequest` 新增字段：

```go
IsSSE     bool `json:"is_sse,omitempty"`     // 标记为 SSE 流
SSEEvents int  `json:"sse_events,omitempty"` // 已收到的事件数
```

### 4. WebSocket 推送扩展

新增 `update_request` 消息类型：

```json
{"type": "new_request", "data": {...}}
{"type": "update_request", "data": {"id": 123, "res_body": "...", "sse_events": 5, "size_bytes": 1234}}
```

### 5. Store 层扩展

```go
func (s *Store) UpdateResBody(id int64, resBody string, sseEvents int) error
```

### 6. 前端改造

`app.js` 处理 `update_request` 事件：实时更新列表项的 ResBody，如果正在查看该请求详情则刷新。

### 7. SSE 事件识别

```go
func isSSEResponse(headerData []byte) bool
func findSSEEventEnd(data []byte) int  // 找到双换行分隔的完整事件
```

### 8. 内存限制

- 代理模式：SSE 流 ResBody 限制 **2MB**（`DefaultMaxSSEResBodyKB = 2048`），普通响应保持 64KB
- 网卡抓包模式：`truncateBuffer` 阈值从 256KB 调到 **2MB**
- 超过限制时截断旧数据，保留最新事件

### 9. 代理模式 TeeReader 数据流

```
resp.Body ──TeeReader──> PipeWriter ──> 客户端（正常转发）
                 │
                 └──> Scanner ──> 识别 SSE 事件 ──> 更新 DB + WebSocket
```

关键：TeeReader 确保代理正常转发 SSE 数据的同时捕获内容，不阻塞客户端。

### 10. 涉及文件

| 文件 | 变更 |
|------|------|
| `internal/proxy/proxy.go` | OnResponse SSE 检测 + 流式读取 |
| `internal/capture/engine.go` | tryExtractHTTP SSE 检测 + tryExtractSSEEvent |
| `internal/models/models.go` | 新增 IsSSE, SSEEvents 字段 |
| `internal/api/ws.go` | 新增 update_request 广播 |
| `internal/store/store.go` | 新增 UpdateResBody 方法 |
| `internal/store/migrations.go` | 新增迁移：is_sse, sse_events 列 |
| `cmd/proxy/web/app.js` | 处理 update_request 事件 |
| `internal/config/config.go` | 新增 DefaultMaxSSEResBodyKB |
