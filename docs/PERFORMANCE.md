# 性能优化方案

> 当前吞吐: 代理 500-2K req/s | 网卡 200-500 req/s
> 目标: 代理 2K-5K req/s | 网卡 1K-3K req/s

---

## 1. SQLite 写入管道 (P0)

**当前**: WAL + NORMAL + batch 50/200ms = 250 req/s burst

| 项 | 当前值 | 优化值 | 预期提升 |
|----|--------|--------|---------|
| batchSize | 50 | **200** | 4× batch 吞吐 |
| flushInterval | 200ms | **100ms** | 2× 刷新频率 |
| channel buffer | 2048 | **8192** | 4× 缓冲容量 |
| cache_size | 8000 (8MB) | **32000 (32MB)** | 4× 页缓存 |
| synchronous | NORMAL | **OFF** (NIC模式可接受) | 2× 写入 |
| checkpoint | 自动 | **passive 每 1000 页** | 减少 WAL 膨胀 |

**代码改动**: `batch.go` 常量 + `store.go` PRAGMA

---

## 2. 网卡抓包 emit 管道 (P0)

**当前**: emitNonBlocking 20/200ms = 100 req/s burst

| 项 | 当前值 | 优化值 | 预期提升 |
|----|--------|--------|---------|
| emit batch | 20 | **100** | 5× |
| flush interval | 200ms | **50ms** | 4× |
| 单条 emit | Save 直接写 | **入 channel → 异步写** | 解耦阻塞 |

---

## 3. pcap 参数调优 (P1)

**当前**: snapshot=65536, immediate=false(BlockForever)

| 项 | 值 | 效果 |
|----|-----|------|
| `pcap.SetBufferSize` | **32MB** | 内核缓冲，降低丢包 |
| `pcap.SetImmediateMode` | **true** | 即时交付，减少延迟 |
| snapshot | 65536→**16384** | 只需 TCP header+少量 payload |
| BPF | `tcp port 80 or 443` | 可加 `and len>0` 过滤空包 |

**代码改动**: `capture/engine.go` Start()

---

## 4. TCP 流池优化 (P1)

| 项 | 当前 | 优化 | 效果 |
|----|------|------|------|
| 并发流上限 | 1000 | **2000** | 2× |
| GC 周期 | 30s | **15s** | 更快释放内存 |
| 流超时 | 5min | **2min** | 短连接快速回收 |
| eviction | FIFO | **LRU** | 活跃连接不丢 |

---

## 5. HTTP 解析优化 (P2)

| 项 | 优化 | 效果 |
|----|------|------|
| `strings.Split` → `bytes.Index` | 直接在 []byte 上操作 | 零分配 |
| header map 预分配 | `make(map[string]string, 16)` | 减少 rehash |
| `parseHTTPRequest` 复用 | 对象池 sync.Pool | 减少 GC |

---

## 6. 进程关联异步化 (P2)

**当前**: 同步 lsof, 阻塞 HTTP 解析线程

**优化**:
- 解析时不查进程, 批量 emit 前一次性补充
- lsof 结果 TTL 从永久改 30s
- 后台 goroutine 每 30s 刷新进程表

---

## 7. 监控指标 (P2)

新增 `/api/metrics` 端点:
```json
{
  "proxy":  {"requests": 1234, "errors": 5, "active": 3, "queue_depth": 12},
  "capture": {"packets": 567890, "streams": 45, "http": 234, "drops": 0},
  "store":   {"queued": 89, "writes": 1200, "errors": 0},
  "memory":  {"alloc": "156MB", "sys": "200MB"}
}
```

---

## 实施优先级

| P | 优化项 | 改动量 | 风险 | 收益 |
|----|--------|--------|------|------|
| **P0** | SQLite 写入管道 | 2 文件, 10 行 | 低 | 4-8× |
| **P0** | NIC emit 管道 | 1 文件, 5 行 | 低 | 5-10× |
| **P1** | pcap 参数 | 1 文件, 5 行 | 低 | 2× |
| **P1** | TCP 流池 | 1 文件, 10 行 | 中 | 1.5× |
| **P2** | HTTP bytes 解析 | 1 文件, 50 行 | 中 | 2× |
| **P2** | 进程异步化 | 2 文件, 30 行 | 中 | 1.5× |
| **P2** | 监控指标 | 2 文件, 40 行 | 低 | dev |

**预期综合提升**: 代理 **2-3×**, 网卡抓包 **3-5×**
