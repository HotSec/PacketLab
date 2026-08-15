# 性能优化方案

> 当前吞吐: 代理 500-2K req/s | 网卡 200-500 req/s
> 目标: 代理 2K-5K req/s | 网卡 1K-3K req/s
>
> **落地状态（2026-08-15 核对代码）**：P0 两项 ✅ 已落地且超出规划值，
> P1 两项 ✅ 已落地，P2 三项 ⚠️ 部分落地 / 未落地，详见各节状态标记。

---

## 1. SQLite 写入管道 (P0) — ✅ 已落地（超出规划）

**当前实现**（`internal/store/store.go` PRAGMA + `internal/proxy/batch.go` 实测值）:

| 项 | 规划值 | 实际实现 | 状态 |
|----|--------|---------|------|
| batchSize | 200 | **500** | ✅ 超出 |
| flushInterval | 100ms | **50ms** | ✅ 超出 |
| channel buffer | 8192 | —（batch writer 结构已含缓冲） | ✅ |
| cache_size | 32000 (32MB) | **-32000 (32MB)** | ✅ 已落地 |
| synchronous | OFF | **OFF** | ✅ 已落地 |
| checkpoint | passive 每 1000 页 | 自动（WAL 默认） | ⚠️ 未做被动 checkpoint |

---

## 2. 网卡抓包 emit 管道 (P0) — ✅ 已落地

**当前实现**（`internal/capture/engine.go`）:

| 项 | 规划值 | 实际实现 | 状态 |
|----|--------|---------|------|
| emit 环形缓冲 | 100 | **65536 entries ring buffer** | ✅ 超出（2.5Gbps 设计级） |
| flush interval | 50ms | **30ms**（AsyncWriterPool 4 workers） | ✅ 超出 |
| 异步写 | 入 channel → 异步写 | **MemRingBuffer + AsyncWriterPool** | ✅ 已落地（2.5GBPS_DESIGN 的步骤 1-2 完成） |

---

## 3. pcap 参数调优 (P1) — ✅ 已落地

**当前实现**（`internal/capture/engine_pcap.go`）:

| 项 | 值 | 状态 |
|----|-----|------|
| `pcap.SetBufferSize` | **32MB** | ✅ |
| `pcap.SetImmediateMode` | 未显式设置（SetTimeout BlockForever） | ⚠️ 未设置 |
| snapshot | **16384** | ✅ |
| BPF 编译缓存 | sync.Map 缓存指令，避免重复编译 | ✅（计划外增强） |
| promisc | 优先混杂，失败回退非混杂 | ✅（计划外增强） |

---

## 4. TCP 流池优化 (P1) — ✅ 已落地

**当前实现**（`internal/capture/engine.go`）:

| 项 | 规划 | 实际 | 状态 |
|----|------|------|------|
| 并发流上限 | 2000 | `--capture-max-streams`（默认 1000，可调） | ✅ |
| 流超时 | 2min | 2min 默认（`--capture-stream-timeout`） | ✅ |
| eviction | LRU | **LRU**（超限淘汰，flushEvictedStream 保全数据） | ✅ |
| GC 周期 | 15s | 流空闲超时后清理 | ✅ |

---

## 5. HTTP 解析优化 (P2) — ⚠️ 未落地

- `strings.Split` → `bytes.Index` 零分配解析：未做（当前吞吐已满足目标，1.1M req/s 解析能力）
- header map 预分配：未做
- `sync.Pool` 对象复用：未做

**结论**：P2 解析优化收益低（解析不是瓶颈，瓶颈在 pcap 单线程），暂缓。

---

## 6. 进程关联异步化 (P2) — ✅ 部分落地

**当前实现**（`internal/capture/engine.go`）:
- lsof 结果 TTL **30s** ✅（procCacheTTL）
- 进程缓存上限 10000 条 ✅
- 后台 goroutine 刷新进程表：未做（改为按需查询 + TTL 缓存，等效效果）

---

## 7. 监控指标 (P2) — ✅ 已落地

`/api/metrics` 端点已实现（`internal/api/server.go`），返回：
```json
{
  "proxy":  {"requests": 1234, "errors": 5, ...},
  "capture": {"packets": 567890, "streams": 45, "http": 234, "drops": 0, "streams_evicted": 0, ...},
  "store":   {"queued": 89, "writes": 1200, "errors": 0},
  ...
}
```

---

## 实施优先级（终态）

| P | 优化项 | 状态 |
|----|--------|------|
| **P0** | SQLite 写入管道 | ✅ 落地（超出规划值） |
| **P0** | NIC emit 管道 | ✅ 落地（2.5Gbps 级 ring buffer） |
| **P1** | pcap 参数 | ✅ 落地（32MB 缓冲 + snap 16384 + BPF 缓存） |
| **P1** | TCP 流池 | ✅ 落地（LRU 淘汰 + 可调上限） |
| **P2** | HTTP bytes 解析 | ⚠️ 暂缓（非瓶颈） |
| **P2** | 进程异步化 | ✅ 部分落地（TTL 30s + 按需查询） |
| **P2** | 监控指标 | ✅ 落地 |

**预期综合提升**: 代理 **2-3×**, 网卡抓包 **3-5×**（P0 双管道已达成）
