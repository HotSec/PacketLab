# 1Gbps HTTP 流量接入方案

## 量化分析

```
1Gbps = 125 MB/s
平均 HTTP 请求+响应: 10KB → 12,500 req/s
峰值: 20KB → 6,250 req/s (大量 body)
```

### 当前瓶颈对照

| 组件 | 当前吞吐 | 1Gbps 需求 | 差距 |
|------|---------|-----------|------|
| pcap 单线程 | ~5,000 pkt/s | 12,500 pkt/s | **2.5×** |
| HTTP 解析 | ~1.1M req/s | 12,500 req/s | ✅ 充足 |
| SQLite WAL | ~34K req/s batch | 12,500 req/s | ✅ 充足 |
| emit 管道 | ~100 batch/50ms | 12,500/s | **125×** |
| 流处理 | 单 goroutine | 需要并行 | 架构级 |

## 架构方案

### 1. 多 Worker Pipeline

```
pcap Handle → BPF 内核过滤
     │
     ├─ Worker 1 → StreamPool shard 1 → HTTP 解析 → emit
     ├─ Worker 2 → StreamPool shard 2 → HTTP 解析 → emit
     ├─ Worker 3 → StreamPool shard 3 → HTTP 解析 → emit
     └─ Worker 4 → StreamPool shard 4 → HTTP 解析 → emit
                          │
                    Sharded Channel
                          │
                    SQLite Writer
```

### 2. 流分片 (Shard by 4-tuple hash)

```
streamShard = hash(srcIP, srcPort, dstIP, dstPort) % workerCount
```

每个 worker 拥有独立的 stream map + mutex，无锁竞争。

### 3. 零拷贝 HTTP 解析

```go
// bytes.Index 替代 strings.Split → 零分配
// header map 复用 pool → 减少 GC
// 直接 []byte 操作 → 避免 string 转换
```

### 4. 异步批量写入

```
emit → ring buffer (64K entries) → batch flush (500条/50ms) → SQLite
```

## 实施步骤

| 步骤 | 改动 | 预期提升 |
|------|------|---------|
| 1 | worker pool: 4 goroutines 并行处理包 | 4× |
| 2 | stream shard: 独立锁消除竞争 | 2× |
| 3 | bytes 解析: 零分配 HTTP parser | 2× |
| 4 | 大 ring buffer: 64K entries → 管道不阻塞 | 5× |
| 5 | SQLite 连接池: 2 writes + 1 read | 2× |
