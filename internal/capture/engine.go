package capture

import (
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"packetlab/internal/models"
	"packetlab/internal/store"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

// Engine 网卡抓包引擎
type Engine struct {
	iface  string
	bpf    string
	handle pcapHandle
	store  *store.Store
	hub    interface {
		BroadcastCapture(req *models.CapturedRequest)
		BroadcastUpdate(req *models.CapturedRequest)
	}
	running       atomic.Bool
	mu            sync.Mutex
	stopCh        chan struct{}
	stats         Stats
	streamTimeout time.Duration // 流空闲超时（超过则 GC 清理并 emit）
	maxResBytes   int64         // 单个响应体最大保留字节（截断阈值，与代理侧 config 统一）
	ringBufSize   int           // 网卡抓包环形缓冲区条目数（<=0 时在 Start 路径用默认值兜底）
	maxStreams    int           // 单个 worker Assembler 的最大并发流数（<=0 时 NewAssembler 用默认值 1000 兜底）

	streamPool *TCPStreamPool
	assembler  *Assembler
	workers    int                    // worker 数量
	workerChs  []chan gopacket.Packet // per-worker packet channels
	workerSg   sync.WaitGroup

	// packetLoopDone 在 packetLoop 退出时 close，Stop 通过 <-packetLoopDone 等待
	// packetLoop 完成 workerSg.Wait() 与 worker channel 关闭，避免快速 Stop→Start
	// 循环时 WaitGroup panic 与数据竞争。
	// packetLoopDone is closed when packetLoop exits; Stop waits on it so that
	// packetLoop's deferred workerSg.Wait() and worker channel closes complete
	// before flushEmitBuf / writer.Stop, preventing WaitGroup panic and data
	// races during rapid Stop→Start cycles.
	packetLoopDone chan struct{}

	// 进程缓存
	procCache    map[string]*models.ProcessInfo
	procCacheTS  map[string]time.Time // 每条缓存写入时间，用于 TTL 淘汰
	procCacheMu  sync.RWMutex
	procCacheTTL time.Duration // 单条缓存有效期（默认 30s）

	// 批量发射 buffer (ring)
	emitBuf  []*models.CapturedRequest
	emitHead int
	emitTail int
	emitMu   sync.Mutex

	// 2.5Gbps 支撑: 内存环形缓冲区 + 异步写入
	ringBuf *MemRingBuffer
	writer  *AsyncWriterPool
}

const emitBufSize = 65536 // 64K entries ring buffer

type pcapHandle interface {
	gopacket.PacketDataSource
	Close()
	LinkType() layers.LinkType
	SetBPFFilter(expr string) error
}

// Stats 抓包统计
type Stats struct {
	PacketsRecv    atomic.Int64
	HTTPFound      atomic.Int64
	PacketsDrop    atomic.Int64
	StreamsDrop    atomic.Int64
	StreamsEvicted atomic.Int64 // 因超过 maxStreams 而 LRU 淘汰的流数
}

// New 创建抓包引擎
func New(iface, bpf string, st *store.Store,
	hub interface {
		BroadcastCapture(req *models.CapturedRequest)
		BroadcastUpdate(req *models.CapturedRequest)
	}) *Engine {

	if bpf == "" {
		bpf = "tcp"
	}

	e := &Engine{
		iface:         iface,
		bpf:           bpf,
		store:         st,
		hub:           hub,
		stopCh:        make(chan struct{}),
		procCache:     make(map[string]*models.ProcessInfo),
		procCacheTS:   make(map[string]time.Time),
		procCacheTTL:  30 * time.Second,
		streamTimeout: 2 * time.Minute, // 默认流空闲超时 2 分钟（可被 SetStreamTimeout 覆盖）
		maxResBytes:   4 * 1024 * 1024, // 默认 4MB，与 config.DefaultMaxResBodyKB 对齐
	}
	return e
}

// SetStreamTimeout 设置流空闲超时（超过则 GC 清理并 emit）
func (e *Engine) SetStreamTimeout(d time.Duration) {
	if d > 0 {
		e.streamTimeout = d
	}
}

// SetMaxResBytes 设置单条响应体最大保留字节（截断阈值），与代理侧 config 统一
func (e *Engine) SetMaxResBytes(n int64) {
	if n > 0 {
		e.maxResBytes = n
	}
}

// SetRingBufSize 设置网卡抓包环形缓冲区条目数（向上取 2 的幂）。
// 必须在 Start 之前调用；<=0 时忽略，由 Start 路径用默认值兜底。
func (e *Engine) SetRingBufSize(n int) {
	if n > 0 {
		e.ringBufSize = n
	}
}

// SetMaxStreams 设置最大并发 TCP 流数（超限 LRU 淘汰）。
// 必须在 Start / NewAssembler 之前调用；<=0 时忽略，由 NewAssembler 用默认值 1000 兜底。
// Set max concurrent TCP streams (LRU eviction on overflow).
// Must be called before Start / NewAssembler; <=0 is ignored and
// NewAssembler falls back to default 1000.
func (e *Engine) SetMaxStreams(n int) {
	if n > 0 {
		e.maxStreams = n
	}
}

// procTableBuildMu 串行化 buildFn（lsof/netstat 外部命令，最坏可阻塞数秒）执行：
// 大量新 TCP 流同时 cache miss 时只启动一个外部命令，其余等待后复用结果，
// 避免每流各 fork 一个 lsof 进程并长时间持锁（流锁/assembler 锁）。
// Serializes buildFn (lsof/netstat, may block seconds): when many new flows miss
// the cache at once, only one external command runs and the rest reuse its result.
var procTableBuildMu sync.Mutex

// resolveProcessCached 通用的进程解析缓存逻辑（TTL + 容量上限淘汰）。
// buildFn 由各平台实现：macOS/Linux 用 lsof，Windows 用 netstat。
func (e *Engine) resolveProcessCached(srcIP string, srcPort uint16, buildFn func() map[string]*models.ProcessInfo) *models.ProcessInfo {
	key := fmt.Sprintf("%s:%d", srcIP, srcPort)
	now := time.Now()

	e.procCacheMu.RLock()
	if ts, ok := e.procCacheTS[key]; ok {
		if now.Sub(ts) < e.procCacheTTL {
			p := e.procCache[key]
			e.procCacheMu.RUnlock()
			return p
		}
	}
	e.procCacheMu.RUnlock()

	// 串行化外部命令执行；等待期间其他 goroutine 可能已填充缓存，先复查
	procTableBuildMu.Lock()
	defer procTableBuildMu.Unlock()
	e.procCacheMu.RLock()
	if ts, ok := e.procCacheTS[key]; ok && now.Sub(ts) < e.procCacheTTL {
		p := e.procCache[key]
		e.procCacheMu.RUnlock()
		return p
	}
	e.procCacheMu.RUnlock()

	table := buildFn()
	if table == nil {
		return nil
	}

	e.procCacheMu.Lock()
	defer e.procCacheMu.Unlock()

	// 防御性初始化：支持直接用结构体字面量构造 Engine（如测试）的场景
	if e.procCache == nil {
		e.procCache = make(map[string]*models.ProcessInfo, len(table))
	}
	if e.procCacheTS == nil {
		e.procCacheTS = make(map[string]time.Time, len(table))
	}
	if e.procCacheTTL == 0 {
		e.procCacheTTL = 30 * time.Second
	}

	// 容量上限淘汰：超过 maxProcCache 时优先淘汰已过期条目，避免全量清空
	// 丢失热数据。若淘汰后仍满（极少见，所有条目都在 TTL 内），则全量重建。
	const maxProcCache = 10000
	if len(e.procCache) >= maxProcCache {
		for k, ts := range e.procCacheTS {
			if now.Sub(ts) >= e.procCacheTTL {
				delete(e.procCache, k)
				delete(e.procCacheTS, k)
			}
		}
		if len(e.procCache) >= maxProcCache {
			e.procCache = make(map[string]*models.ProcessInfo, len(table))
			e.procCacheTS = make(map[string]time.Time, len(table))
		}
	}

	for k, v := range table {
		e.procCache[k] = v
		e.procCacheTS[k] = now
	}

	return table[key]
}

// Stop 停止抓包（幂等）
func (e *Engine) Stop() {
	e.mu.Lock()
	if !e.running.Swap(false) {
		e.mu.Unlock()
		return
	}

	// 先关闭 handle，packetLoop 的 range 会退出，defer 会清理 worker channels
	// Close handle first so packetLoop's range exits and defer cleans up worker channels
	if e.handle != nil {
		e.handle.Close()
		e.handle = nil
	}
	e.mu.Unlock() // 释放锁，避免阻塞操作持锁 / release lock before blocking ops

	select {
	case <-e.stopCh:
	default:
		close(e.stopCh)
	}

	// 等待 packetLoop 退出：其 defer 会关闭 worker channels 并 workerSg.Wait()，
	// 避免 Stop 后 Start 时 worker/WaitGroup 仍在使用导致 panic 与数据竞争。
	// Wait for packetLoop to exit: its defer closes worker channels and calls
	// workerSg.Wait(), so workers fully drain before we touch flushEmitBuf /
	// writer below. nil when Start was never called (stub or test path).
	if e.packetLoopDone != nil {
		<-e.packetLoopDone
	}

	// flush 剩余 emit 数据（可能阻塞，不持锁）
	// flush remaining emit buffer (may block, not holding lock)
	e.flushEmitBuf()
	// 停止异步写入池（可能阻塞，不持锁）
	// stop async writer pool (may block, not holding lock)
	if e.writer != nil {
		e.writer.Stop()
	}

	slog.Info("capture: 抓包已停止")
}

// IsRunning 是否运行中
func (e *Engine) IsRunning() bool {
	return e.running.Load()
}

// GetStats 获取统计
func (e *Engine) GetStats() (int64, int64) {
	return e.stats.PacketsRecv.Load(), e.stats.HTTPFound.Load()
}

// GetMetrics 获取运行指标
func (e *Engine) GetMetrics() map[string]interface{} {
	m := map[string]interface{}{
		"packets":         e.stats.PacketsRecv.Load(),
		"http":            e.stats.HTTPFound.Load(),
		"running":         e.running.Load(),
		"streams_evicted": e.stats.StreamsEvicted.Load(),
	}
	if e.ringBuf != nil {
		m["ring_usage"] = e.ringBuf.Usage()
		m["ring_dropped"] = e.ringBuf.Dropped()
	}
	if e.writer != nil {
		m["writer_written"] = e.writer.Written()
	}
	return m
}

// flowHash 基于五元组计算 worker 索引，确保同一流的数据包到同一 worker
func (e *Engine) flowHash(packet gopacket.Packet) int {
	tcpLayer := packet.Layer(layers.LayerTypeTCP)
	if tcpLayer == nil {
		return 0
	}
	tcp, ok := tcpLayer.(*layers.TCP)
	if !ok {
		return 0
	}

	var sIP, dIP uint32
	if ip4Layer := packet.Layer(layers.LayerTypeIPv4); ip4Layer != nil {
		if ip, ok := ip4Layer.(*layers.IPv4); ok {
			for _, b := range ip.SrcIP.To4() {
				sIP = sIP<<8 | uint32(b)
			}
			for _, b := range ip.DstIP.To4() {
				dIP = dIP<<8 | uint32(b)
			}
		}
	} else if ip6Layer := packet.Layer(layers.LayerTypeIPv6); ip6Layer != nil {
		if ip, ok := ip6Layer.(*layers.IPv6); ok {
			// IPv6 16 字节地址折叠成 uint32（FNV-like 简易哈希，仅需分布均匀）
			sIP = foldIPv6(ip.SrcIP)
			dIP = foldIPv6(ip.DstIP)
		}
	} else {
		return 0
	}
	h := sIP ^ dIP ^ uint32(tcp.SrcPort) ^ uint32(tcp.DstPort)
	h = (h >> 16) ^ h
	return int(h) % e.workers
}

// foldIPv6 将 16 字节 IPv6 地址折叠成一个 uint32（用于 worker 哈希分布，非密码学用途）
func foldIPv6(ip net.IP) uint32 {
	b := ip.To16()
	if b == nil {
		return 0
	}
	// 每 4 字节异或折叠
	var v uint32
	for i := 0; i < 16; i += 4 {
		v ^= uint32(b[i])<<24 | uint32(b[i+1])<<16 | uint32(b[i+2])<<8 | uint32(b[i+3])
	}
	return v
}

// workerLoop 独立 goroutine 处理数据包（per-worker Assembler + 流超时清理）
func (e *Engine) workerLoop(id int) {
	defer e.workerSg.Done()
	assembler := NewAssembler(e.streamPool)
	ch := e.workerChs[id]
	// GC 周期取流超时的 1/8（最少 5s），保证过期流及时回收
	gcInterval := e.streamTimeout / 8
	if gcInterval < 5*time.Second {
		gcInterval = 5 * time.Second
	}
	gcTicker := time.NewTicker(gcInterval)
	defer gcTicker.Stop()
	for {
		select {
		case packet, ok := <-ch:
			if !ok {
				assembler.FlushAllWithPending(e)
				return
			}
			assembler.Assemble(packet)
		case <-gcTicker.C:
			// flush 超过 streamTimeout 未活跃的流：cutoff = now - streamTimeout
			// 修复：原代码用 now + streamTimeout（未来时间），导致所有流每次 GC 都被强制 flush
			cutoff := time.Now().Add(-e.streamTimeout)
			assembler.FlushOlderThan(cutoff, e)
		}
	}
}

// flushLoop 定期刷新 emit 缓冲区
func (e *Engine) flushLoop() {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for e.running.Load() {
		select {
		case <-ticker.C:
			e.flushEmitBuf()
		case <-e.stopCh:
			e.flushEmitBuf() // 最后刷新
			return
		}
	}
}

// emit 输出 HTTP 请求到存储
func (e *Engine) emit(req *models.CapturedRequest) {
	req.CaptureMode = "nic"
	id, err := e.store.Save(req)
	if err != nil {
		slog.Warn("capture: save failed", "url", req.URL, "error", err)
		return
	}
	req.ID = id
	if e.hub != nil {
		e.hub.BroadcastCapture(req)
	}
	e.stats.HTTPFound.Add(1)
}

// bulkEmit 批量输出
func (e *Engine) bulkEmit(reqs []*models.CapturedRequest) {
	if len(reqs) == 0 {
		return
	}
	for _, req := range reqs {
		req.CaptureMode = "nic"
	}
	ids, err := e.store.SaveBatch(reqs)
	if err != nil {
		slog.Warn("capture: bulk save failed", "count", len(reqs), "error", err)
		return
	}
	for i, id := range ids {
		if i < len(reqs) {
			reqs[i].ID = id
		}
	}
	for _, req := range reqs {
		// HTTPFound 已在 emitNonBlocking 入口计数，这里不再重复 / HTTPFound already counted at emitNonBlocking entry, no double-count here
		if e.hub != nil && req.ID > 0 {
			e.hub.BroadcastCapture(req)
		}
	}
}

func (e *Engine) emitNonBlocking(req *models.CapturedRequest) {
	// 立即计数 HTTPFound（与 emit 一致），避免 ring buffer drop 或 save 失败导致漏计
	// Increment HTTPFound at entry (consistent with emit) to avoid undercount
	// when ring buffer drops records or save fails.
	e.stats.HTTPFound.Add(1)
	req.CaptureMode = "nic"

	if e.ringBuf != nil {
		e.ringBuf.Push(req)
		return
	}
	e.emitMu.Lock()
	if e.emitBuf == nil {
		e.emitBuf = make([]*models.CapturedRequest, emitBufSize)
	}
	next := (e.emitHead + 1) % emitBufSize
	if next == e.emitTail {
		e.emitMu.Unlock()
		e.flushEmitBuf()
		e.emitMu.Lock()
		next = (e.emitHead + 1) % emitBufSize
	}
	e.emitBuf[e.emitHead] = req
	e.emitHead = next
	count := (e.emitHead - e.emitTail + emitBufSize) % emitBufSize
	e.emitMu.Unlock()
	if count >= 1000 {
		e.flushEmitBuf()
	}
}

func (e *Engine) flushEmitBuf() {
	e.emitMu.Lock()
	if e.emitBuf == nil || e.emitHead == e.emitTail {
		e.emitMu.Unlock()
		return
	}
	// 提取所有条目
	var batch []*models.CapturedRequest
	if e.emitHead > e.emitTail {
		batch = make([]*models.CapturedRequest, e.emitHead-e.emitTail)
		copy(batch, e.emitBuf[e.emitTail:e.emitHead])
	} else {
		n := emitBufSize - e.emitTail + e.emitHead
		batch = make([]*models.CapturedRequest, n)
		copy(batch, e.emitBuf[e.emitTail:])
		copy(batch[emitBufSize-e.emitTail:], e.emitBuf[:e.emitHead])
	}
	e.emitHead = 0
	e.emitTail = 0
	e.emitMu.Unlock()

	if len(batch) > 0 {
		e.bulkEmit(batch)
	}
}

// ========================================
// TCP Assembler (简化版 tcpassembly)
// ========================================

// Assembler TCP 流重组器
//
// 锁顺序约定（跨 Assembler.mu 与 TCPStream.mu 时务必按此顺序获取，避免死锁）：
//  1. Assembler.mu
//  2. TCPStream.mu
//
// 参考实现：FlushOlderThan / HandleClose / evictOldestIdleStream 均遵循此顺序。
type Assembler struct {
	pool       *TCPStreamPool
	streams    map[string]*TCPStream
	mu         sync.Mutex
	maxStreams int // 单 worker 最大并发流数（NewAssembler 从 pool.engine 读取，默认 1000）
}

// NewAssembler 创建重组器
//
// maxStreams 从 pool.engine.maxStreams 读取（带默认值 1000 兜底），
// 这样 workerLoop 创建 assembler 时无需修改 NewAssembler 签名。
// maxStreams is read from pool.engine.maxStreams (with default fallback 1000),
// so workerLoop can create assemblers without changing NewAssembler signature.
func NewAssembler(pool *TCPStreamPool) *Assembler {
	maxStreams := 1000
	if pool != nil && pool.engine != nil && pool.engine.maxStreams > 0 {
		maxStreams = pool.engine.maxStreams
	}
	return &Assembler{
		pool:       pool,
		streams:    make(map[string]*TCPStream),
		maxStreams: maxStreams,
	}
}

// Assemble 组装数据包到流中（双方向归一化），支持 IPv4 与 IPv6
func (a *Assembler) Assemble(packet gopacket.Packet) {
	tcpLayer := packet.Layer(layers.LayerTypeTCP)
	if tcpLayer == nil {
		return
	}
	tcp, ok := tcpLayer.(*layers.TCP)
	if !ok {
		return
	}

	// 提取 IP 层（IPv4 或 IPv6）
	var srcIP, dstIP net.IP
	if ip4Layer := packet.Layer(layers.LayerTypeIPv4); ip4Layer != nil {
		ip, ok := ip4Layer.(*layers.IPv4)
		if !ok {
			return
		}
		srcIP, dstIP = ip.SrcIP, ip.DstIP
	} else if ip6Layer := packet.Layer(layers.LayerTypeIPv6); ip6Layer != nil {
		ip, ok := ip6Layer.(*layers.IPv6)
		if !ok {
			return
		}
		srcIP, dstIP = ip.SrcIP, ip.DstIP
	} else {
		return
	}

	isFIN := tcp.FIN
	isRST := tcp.RST

	srcIPStr := srcIP.String()
	dstIPStr := dstIP.String()
	srcPort := uint16(tcp.SrcPort)
	dstPort := uint16(tcp.DstPort)

	isClientToServer := determineDirection(srcPort, dstPort)

	var clientIP net.IP
	var clientPort, serverPort uint16
	if isClientToServer {
		clientIP = srcIP
		clientPort = srcPort
		serverPort = dstPort
	} else {
		clientIP = dstIP
		clientPort = dstPort
		serverPort = srcPort
	}

	var keyA, keyB string
	var portA, portB uint16
	if srcIPStr < dstIPStr || (srcIPStr == dstIPStr && srcPort < dstPort) {
		keyA, keyB = srcIPStr, dstIPStr
		portA, portB = srcPort, dstPort
	} else {
		keyA, keyB = dstIPStr, srcIPStr
		portA, portB = dstPort, srcPort
	}
	// 分隔符用 # 而非 - / :：IPv6 地址本身含 :，旧格式 key 会发生解析歧义
	streamKey := fmt.Sprintf("%s#%d#%s#%d", keyA, portA, keyB, portB)

	var evictedStream *TCPStream
	a.mu.Lock()
	stream, ok := a.streams[streamKey]
	if !ok {
		if len(a.streams) >= a.maxStreams {
			// 容量超限：LRU 淘汰 lastActive 最老的流。evictOldestIdleStream 在持
			// a.mu 时 delete 并返回 stream 指针；flush 必须在释放 a.mu 后执行
			// （tryExtractHTTPOnClose / flushSSEEvents 可能做 I/O: store.Save /
			// hub.Broadcast），由 flushEvictedStream 完成。
			// Capacity exceeded: LRU-evict the oldest lastActive stream.
			// evictOldestIdleStream deletes under a.mu and returns the pointer;
			// flush runs after a.mu is released (tryExtractHTTPOnClose /
			// flushSSEEvents may do I/O: store.Save / hub.Broadcast).
			evictedStream = a.evictOldestIdleStream()
			if evictedStream != nil {
				a.pool.engine.stats.StreamsEvicted.Add(1)
			}
			if len(a.streams) >= a.maxStreams {
				// 淘汰后仍满（理论上不应发生，evictOldestIdleStream 必删一条）→ 丢弃新流
				// Still full after eviction (shouldn't happen) → drop the new flow
				a.mu.Unlock()
				flushEvictedStream(evictedStream)
				a.pool.engine.stats.StreamsDrop.Add(1)
				return
			}
		}
		stream = a.pool.New(clientIP, clientPort, serverPort)
		a.streams[streamKey] = stream
	}
	a.mu.Unlock()

	// 锁外 flush 被淘汰流的残留数据（pendingReq / SSE 事件）。
	// Flush the evicted stream's residual data outside a.mu.
	flushEvictedStream(evictedStream)

	// SYN 方向修正：TCP 三次握手中 SYN 的发起方才是客户端，端口启发式
	// 在客户端/服务端都无标准端口（如自定义端口服务）时会误判；
	// 抓包中途开始（未见 SYN）时 SYN-ACK 的接收方是客户端。
	// SYN fixes direction from the handshake: the SYN sender is the client.
	// The port heuristic can misjudge when neither side uses a standard port.
	// When capture starts mid-connection (no SYN seen), the SYN-ACK
	// receiver is the client.
	if tcp.SYN && !tcp.ACK {
		stream.clientPort = srcPort
		isClientToServer = true
	} else if tcp.SYN && tcp.ACK && stream.clientPort != 0 {
		stream.clientPort = dstPort
		isClientToServer = false
	} else if stream.clientPort != 0 {
		isClientToServer = srcPort == stream.clientPort
	}

	if len(tcp.Payload) > 0 {
		stream.Feed(tcp.Payload, isClientToServer)
	}

	if isFIN || isRST {
		stream.HandleClose(a)
	}
}

// determineDirection 基于端口号判断数据方向（目标为服务端口则源为客户端）
func determineDirection(srcPort, dstPort uint16) bool {
	if isLikelyServerPort(dstPort) {
		return true
	}
	if isLikelyServerPort(srcPort) {
		return false
	}
	return srcPort > dstPort
}

// isLikelyServerPort 判断是否为常见服务端口
func isLikelyServerPort(port uint16) bool {
	switch port {
	case 80, 443, 8080, 8443, 3000, 5000, 8000, 8888, 9000, 9090,
		2080, 2083, 2086, 2087, 4443, 7443, 11180:
		return true
	default:
		return port < 1024
	}
}

// FlushOlderThan 清理过期流（持流锁保护，先 tryExtractHTTP 再 emit pendingReq）
func (a *Assembler) FlushOlderThan(t time.Time, engine *Engine) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, s := range a.streams {
		s.mu.Lock()
		if s.nonHTTP {
			s.mu.Unlock()
			delete(a.streams, key)
			continue
		}
		if s.lastActive.Before(t) {
			if s.ssePending {
				s.flushSSEEvents()
			} else {
				s.tryExtractHTTPOnClose()
			}
			s.mu.Unlock()
			delete(a.streams, key)
		} else {
			s.mu.Unlock()
		}
	}
}

// FlushAllWithPending 清理所有流并 emit pendingReq
func (a *Assembler) FlushAllWithPending(engine *Engine) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.streams {
		s.mu.Lock()
		if !s.nonHTTP {
			if s.ssePending {
				s.flushSSEEvents()
			} else {
				s.tryExtractHTTPOnClose()
			}
		}
		s.mu.Unlock()
	}
	a.streams = make(map[string]*TCPStream)
}

// evictOldestIdleStream 淘汰 lastActive 最老的流（LRU），从 a.streams 中 delete
// 并返回被淘汰流指针。
//
// 调用方负责在释放 a.mu 后对返回的 stream 调用 flushEvictedStream 以 flush 残留
// 数据（pendingReq / SSE 事件）。不能依赖 GC ticker 兜底：FlushOlderThan（GC
// ticker）只遍历 a.streams，被 delete 的流已不在 map 中，永远不会被 GC 处理。
//
// 调用前必须已持有 a.mu；本方法在持有 a.mu 期间内获取 stream.mu 读 lastActive，
// 符合锁顺序约定（Assembler.mu → TCPStream.mu）。
//
// Evict the stream with the oldest lastActive (LRU): deletes it from a.streams
// and returns the stream pointer. Caller MUST flush the returned stream after
// releasing a.mu (via flushEvictedStream); GC ticker cannot be relied on
// because FlushOlderThan only iterates a.streams, and the evicted stream is
// already removed from the map.
//
// Caller MUST already hold a.mu; this method acquires stream.mu while
// holding a.mu, consistent with lock order (Assembler.mu → TCPStream.mu).
func (a *Assembler) evictOldestIdleStream() *TCPStream {
	var oldestKey string
	var oldestTime time.Time
	for k, stream := range a.streams {
		stream.mu.Lock()
		t := stream.lastActive
		stream.mu.Unlock()
		if oldestTime.IsZero() || t.Before(oldestTime) {
			oldestKey = k
			oldestTime = t
		}
	}
	if oldestKey == "" {
		return nil
	}
	stream := a.streams[oldestKey]
	delete(a.streams, oldestKey)
	return stream
}

// flushEvictedStream 在锁外 flush 被淘汰流的残留数据（pendingReq / SSE 事件）。
//
// 必须在释放 a.mu 后调用：tryExtractHTTPOnClose / flushSSEEvents 可能做 I/O
// （store.Save / hub.Broadcast），不应持 a.mu。本函数自身获取 stream.mu
// （Assembler.mu 已释放，仅持 stream.mu，无锁序冲突）。
//
// s 为 nil 时为 no-op（调用方无需做 nil 检查）。
//
// Flush residual data (pendingReq / SSE events) of an evicted stream outside
// a.mu. Must be called after releasing a.mu: tryExtractHTTPOnClose /
// flushSSEEvents may do I/O (store.Save / hub.Broadcast) and must not hold
// a.mu. This function acquires stream.mu (a.mu already released, no
// lock-order conflict). No-op when s is nil.
func flushEvictedStream(s *TCPStream) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nonHTTP {
		return
	}
	if s.ssePending {
		s.flushSSEEvents()
	} else {
		s.tryExtractHTTPOnClose()
	}
}

// ========================================
// TCP Stream
// ========================================

// TCPStreamPool TCP 流工厂
type TCPStreamPool struct {
	engine *Engine
}

// NewTCPStreamPool 创建流工厂
func NewTCPStreamPool(e *Engine) *TCPStreamPool {
	return &TCPStreamPool{engine: e}
}

// New 创建新 TCP 流
func (p *TCPStreamPool) New(clientIP net.IP, clientPort, serverPort uint16) *TCPStream {
	return &TCPStream{
		engine:     p.engine,
		srcIP:      clientIP,
		srcPort:    clientPort,
		dstPort:    serverPort,
		clientPort: clientPort,
		lastActive: time.Now(),
	}
}

// TCPStream 单个 TCP 流（双方向缓冲区 + 线程安全）
type TCPStream struct {
	mu         sync.Mutex
	engine     *Engine
	srcIP      net.IP
	srcPort    uint16
	dstPort    uint16
	clientPort uint16 // 客户端端口（SYN 握手表征的确定方向，0 表示未知）
	clientBuf  []byte // 客户端→服务端 数据
	serverBuf  []byte // 服务端→客户端 数据
	lastActive time.Time
	pendingReq *models.CapturedRequest
	nonHTTP    bool // 标记为非HTTP流，跳过后续处理
	firstData  bool // 是否已收到首批数据（用于非HTTP检测）
	sniEmitted bool // 是否已emit TLS SNI记录

	// SSE 流式追踪
	ssePending bool   // 是否为 SSE 流（已 emit 初始记录）
	sseReqID   int64  // SSE 流对应的 DB 记录 ID
	sseBuf     []byte // SSE 事件累积缓冲
}

// Feed 喂入 TCP 数据（按方向分离缓冲区）
func (s *TCPStream) Feed(data []byte, clientToServer bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastActive = time.Now()

	if s.nonHTTP {
		return
	}

	// SSE 流已 emit 初始记录，后续服务端数据追加事件
	if s.ssePending && !clientToServer {
		s.sseBuf = append(s.sseBuf, data...)
		// 定时更新 DB（每 4KB 或超过阈值时）
		if len(s.sseBuf) >= 4096 {
			s.flushSSEEvents()
		}
		// 限制 SSE 缓冲区大小（与响应体截断阈值一致）
		if int64(len(s.sseBuf)) > s.engine.maxResBytes {
			slog.Warn("capture: SSE events truncated (exceeds maxResBytes limit)", "id", s.sseReqID, "total_size", len(s.sseBuf))
			s.sseBuf = s.sseBuf[int64(len(s.sseBuf))-s.engine.maxResBytes:]
		}
		return
	}

	if clientToServer {
		s.clientBuf = append(s.clientBuf, data...)
	} else {
		s.serverBuf = append(s.serverBuf, data...)
	}

	if !s.firstData {
		s.firstData = true
		var first []byte
		if len(s.clientBuf) > 0 {
			first = s.clientBuf
		} else {
			first = s.serverBuf
		}
		if len(first) > 0 && !looksLikeHTTP(first) {
			if isTLSClientHello(first) && !s.sniEmitted {
				sni := extractSNI(first)
				if sni != "" {
					s.emitTLSRecord(sni)
					s.sniEmitted = true
				}
			}
			s.nonHTTP = true
			s.clientBuf = nil
			s.serverBuf = nil
			return
		}
	}

	s.tryExtractHTTP()

	// 缓冲上限与 maxResBytes 一致（而非固定 8MB）：避免每个方向每流在配置
	// 阈值之外再冗余 2 倍内存；超限时截断为最近 maxResBytes 字节。
	// Buffer cap tracks maxResBytes (not a hardcoded 8MB) so per-stream
	// memory stays bounded by the configured limit; oversized buffers are
	// trimmed to the most recent maxResBytes.
	bufLimit := int64(streamBufLimit(s))
	if int64(len(s.clientBuf)) > bufLimit {
		s.clientBuf = truncateBuffer(s.clientBuf, int(bufLimit))
	}
	if int64(len(s.serverBuf)) > bufLimit {
		s.serverBuf = truncateBuffer(s.serverBuf, int(bufLimit))
	}
}

// streamBufLimit 返回单流单方向缓冲上限：maxResBytes（含 0 未配置时回退 4MB）
func streamBufLimit(s *TCPStream) int64 {
	if s.engine.maxResBytes > 0 {
		return s.engine.maxResBytes
	}
	return 4 * 1024 * 1024
}

// isSSEHeader 检测响应头是否为 SSE（Content-Type: text/event-stream）
func isSSEHeader(headerData []byte) bool {
	return strings.Contains(strings.ToLower(string(headerData)), "text/event-stream")
}

// flushSSEEvents 将 SSE 事件缓冲区更新到 DB 并推送 WebSocket
func (s *TCPStream) flushSSEEvents() {
	if s.sseReqID <= 0 || len(s.sseBuf) == 0 {
		return
	}
	events := string(s.sseBuf)
	if err := s.engine.store.UpdateResBody(s.sseReqID, events, events, int64(len(s.sseBuf))); err != nil {
		slog.Warn("capture: SSE update DB failed", "id", s.sseReqID, "error", err)
	}
	// 推送 WebSocket 增量更新（仅元数据，body 由前端按需请求）
	if s.engine.hub != nil {
		s.engine.hub.BroadcastUpdate(&models.CapturedRequest{
			ID:        s.sseReqID,
			IsSSE:     true,
			SizeBytes: int64(len(s.sseBuf)),
		})
	}
}

// truncateBuffer 截断缓冲区，尽量在 HTTP 消息边界处截断
func truncateBuffer(buf []byte, keepSize int) []byte {
	if len(buf) <= keepSize {
		return buf
	}
	cut := len(buf) - keepSize
	trimmed := buf[cut:]
	nl := indexDoubleCRLF(trimmed)
	if nl >= 0 && nl < 4096 {
		trimmed = trimmed[nl+4:]
	}
	return trimmed
}

// looksLikeHTTP 检测数据是否看起来像 HTTP
func looksLikeHTTP(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	for _, prefix := range httpMethodPrefixes {
		if len(data) >= len(prefix) && string(data[:len(prefix)]) == prefix {
			return true
		}
	}
	if len(data) >= 4 && string(data[:4]) == "HTTP" {
		return true
	}
	return false
}

var httpMethodPrefixes = []string{"GET ", "POST", "PUT ", "DELE", "PATC", "HEAD", "OPTI", "CONN", "TRAC"}

// HandleClose 处理 TCP 连接关闭（FIN/RST），先尝试提取残留数据再 emit
// 锁顺序：先 Assembler.mu，再 TCPStream.mu（与 FlushOlderThan 一致，避免死锁）
func (s *TCPStream) HandleClose(a *Assembler) {
	a.mu.Lock()
	defer a.mu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.nonHTTP {
		for key, stream := range a.streams {
			if stream == s {
				delete(a.streams, key)
				break
			}
		}
		return
	}

	// SSE 流关闭，最终 flush
	if s.ssePending {
		s.flushSSEEvents()
		for key, stream := range a.streams {
			if stream == s {
				delete(a.streams, key)
				break
			}
		}
		return
	}

	s.tryExtractHTTPOnClose()

	for key, stream := range a.streams {
		if stream == s {
			delete(a.streams, key)
			break
		}
	}
}

// tryExtractHTTP 从双缓冲区中提取 HTTP 请求/响应并消费
func (s *TCPStream) tryExtractHTTP() {
	for {
		if s.pendingReq == nil {
			if len(s.clientBuf) == 0 {
				return
			}
			idx := findHTTPMessageEnd(s.clientBuf)
			if idx < 0 {
				return
			}
			msgData := s.clientBuf[:idx]
			s.clientBuf = s.clientBuf[idx:]
			if !isHTTPResponse(msgData) {
				req := parseHTTPRequest(msgData, s.srcIP, s.srcPort, s.dstPort, s.engine)
				if req != nil {
					s.pendingReq = req
				}
			}
			if s.pendingReq == nil {
				return
			}
		}

		if len(s.serverBuf) == 0 {
			return
		}

		if !isHTTPResponse(s.serverBuf) {
			return
		}

		idx := findHTTPMessageEnd(s.serverBuf)
		if idx < 0 {
			return
		}

		headerData := s.serverBuf[:idx]
		hasCL := parseContentLength(headerData) >= 0
		hasChunked := strings.Contains(parseTransferEncoding(headerData), "chunked")
		isSSE := isSSEHeader(headerData)

		// SSE 响应：无 Content-Length，收到响应头即 emit，后续数据作为增量更新
		if isSSE && !hasCL && !hasChunked {
			msgData := s.serverBuf[:idx]
			s.serverBuf = s.serverBuf[idx:]
			resp := parseHTTPResponse(msgData)
			if resp != nil && resp.StatusCode >= 200 {
				req := s.pendingReq
				req.StatusCode = resp.StatusCode
				req.ResHeaders = resp.Headers
				req.ResBody = resp.Body
				req.IsSSE = true
				req.SSEEvents = resp.Body
				req.DurationMs = time.Since(req.CapturedAt).Milliseconds()
				req.SizeBytes = int64(len(msgData))
				// 同步 Save 以确保拿到真实 ID，SSE 响应头很小，开销可忽略
				id, err := s.engine.store.Save(req)
				if err != nil {
					// save 失败时不能仅 continue：pendingReq 已在上方准备就绪，
					// 但 ssePending 未设为 true，下次循环 pendingReq!=nil 而
					// serverBuf 已被消费 → tryExtractHTTP 直接 return，后续 SSE
					// 数据走 serverBuf 累积直至被 truncateBuffer 截断。
					// 标记 nonHTTP 终止该流的后续处理，避免数据堆积与误判。
					// On save failure we must not just continue: pendingReq is
					// ready but ssePending is not set, so the next loop iteration
					// sees pendingReq!=nil with consumed serverBuf and returns,
					// leaving subsequent SSE data to accumulate in serverBuf until
					// truncateBuffer truncates it. Mark nonHTTP to terminate the
					// stream and drop further data cleanly.
					slog.Warn("capture: SSE initial save failed, marking stream non-HTTP", "url", req.URL, "error", err)
					s.pendingReq = nil
					s.nonHTTP = true
					s.clientBuf = nil
					s.serverBuf = nil
					continue
				}
				// save 成功后再清 pendingReq，确保失败路径不会留下半完成的 SSE 状态
				// Clear pendingReq only after successful save so the failure path
				// above can cleanly reset stream state.
				s.pendingReq = nil
				req.ID = id
				s.ssePending = true
				s.sseReqID = id
				s.engine.stats.HTTPFound.Add(1)
				if s.engine.hub != nil {
					s.engine.hub.BroadcastUpdate(req)
				}
			}
			continue
		}

		if !hasCL && !hasChunked {
			statusCode := parseStatusCodeFromHeader(headerData)
			if statusCode > 0 && !responseCanHaveNoBody(statusCode) {
				return
			}
		}

		msgData := s.serverBuf[:idx]
		s.serverBuf = s.serverBuf[idx:]

		resp := parseHTTPResponse(msgData)
		if resp == nil {
			continue
		}

		if resp.StatusCode >= 100 && resp.StatusCode < 200 {
			continue
		}

		s.pendingReq.StatusCode = resp.StatusCode
		s.pendingReq.ResHeaders = resp.Headers
		s.pendingReq.ResBody = resp.Body
		s.pendingReq.DurationMs = time.Since(s.pendingReq.CapturedAt).Milliseconds()
		s.pendingReq.SizeBytes = int64(len(msgData))
		s.engine.emitNonBlocking(s.pendingReq)
		s.pendingReq = nil
	}
}

// responseCanHaveNoBody 判断响应是否可以没有 body（无需等待连接关闭）
func responseCanHaveNoBody(statusCode int) bool {
	return (statusCode >= 100 && statusCode < 200) || statusCode == 204 || statusCode == 304
}

// parseStatusCodeFromHeader 从 HTTP 头部字节中快速解析状态码
func parseStatusCodeFromHeader(headerData []byte) int {
	nl := indexByte(headerData, '\n')
	if nl < 0 {
		return 0
	}
	firstLine := bytesTrimSpace(headerData[:nl])
	sp1 := indexByte(firstLine, ' ')
	if sp1 < 0 {
		return 0
	}
	rest := firstLine[sp1+1:]
	sp2 := indexByte(rest, ' ')
	var codeStr []byte
	if sp2 >= 0 {
		codeStr = rest[:sp2]
	} else {
		codeStr = rest
	}
	var code int
	fmt.Sscanf(string(codeStr), "%d", &code)
	return code
}

// tryExtractHTTPOnClose 连接关闭时提取残留数据，处理无Content-Length的响应body
func (s *TCPStream) tryExtractHTTPOnClose() {
	for {
		s.tryExtractHTTP()

		if s.pendingReq == nil && len(s.clientBuf) == 0 {
			return
		}

		if s.pendingReq == nil && len(s.clientBuf) > 0 {
			idx := findHTTPMessageEnd(s.clientBuf)
			if idx >= 0 {
				msgData := s.clientBuf[:idx]
				s.clientBuf = s.clientBuf[idx:]
				if !isHTTPResponse(msgData) {
					req := parseHTTPRequest(msgData, s.srcIP, s.srcPort, s.dstPort, s.engine)
					if req != nil {
						s.pendingReq = req
					}
				}
			}
			if s.pendingReq == nil {
				s.clientBuf = nil
				s.serverBuf = nil
				return
			}
		}

		if s.pendingReq == nil {
			return
		}

		if len(s.serverBuf) > 0 {
			headerEnd := indexDoubleCRLF(s.serverBuf)
			if headerEnd >= 0 {
				headerData := s.serverBuf[:headerEnd]
				bodyStart := headerEnd + 4
				var body string
				if len(s.serverBuf) > bodyStart {
					body = string(s.serverBuf[bodyStart:])
				}
				resp := parseHTTPResponseFromHeader(headerData, body)
				if resp != nil {
					if resp.StatusCode >= 100 && resp.StatusCode < 200 {
						// 1xx informational response：跳过，等待最终响应
						s.serverBuf = nil
						continue
					}
					s.pendingReq.StatusCode = resp.StatusCode
					s.pendingReq.ResHeaders = resp.Headers
					s.pendingReq.ResBody = resp.Body
					s.pendingReq.DurationMs = time.Since(s.pendingReq.CapturedAt).Milliseconds()
					s.pendingReq.SizeBytes = int64(len(s.serverBuf))
				}
			} else if isHTTPResponse(s.serverBuf) {
				resp := parseHTTPResponse(s.serverBuf)
				if resp != nil && resp.StatusCode >= 200 {
					s.pendingReq.StatusCode = resp.StatusCode
					s.pendingReq.ResHeaders = resp.Headers
					s.pendingReq.ResBody = resp.Body
					s.pendingReq.DurationMs = time.Since(s.pendingReq.CapturedAt).Milliseconds()
					s.pendingReq.SizeBytes = int64(len(s.serverBuf))
				}
			}
		}

		if s.pendingReq != nil {
			s.engine.emitNonBlocking(s.pendingReq)
			s.pendingReq = nil
		}
		s.serverBuf = nil
		s.clientBuf = nil
	}
}

// isHTTPResponse 判断是否是 HTTP 响应（以 HTTP/ 开头）
func isHTTPResponse(data []byte) bool {
	return len(data) >= 4 && (string(data[:4]) == "HTTP")
}

// findHTTPMessageEnd 查找 HTTP 消息结束位置（支持 CRLF / LF / Content-Length / chunked）
func findHTTPMessageEnd(data []byte) int {
	idx := -1
	// 优先 CRLF
	for i := 0; i < len(data)-3; i++ {
		if data[i] == '\r' && data[i+1] == '\n' && data[i+2] == '\r' && data[i+3] == '\n' {
			idx = i + 4
			break
		}
	}
	// 回退 LFLF（非标准但常见）
	if idx < 0 {
		for i := 0; i < len(data)-1; i++ {
			if data[i] == '\n' && data[i+1] == '\n' {
				idx = i + 2
				break
			}
		}
	}
	if idx < 0 {
		return -1
	}

	headerData := data[:idx]

	// Content-Length
	cl := parseContentLength(headerData)
	if cl >= 0 {
		bodyEnd := idx + cl
		if bodyEnd <= len(data) {
			return bodyEnd
		}
		return -1 // 等待更多数据
	}

	// Chunked Transfer-Encoding
	te := parseTransferEncoding(headerData)
	if strings.Contains(te, "chunked") {
		// 查找 chunked 结束标记: 0\r\n\r\n
		chunkEnd := findChunkedEnd(data[idx:])
		if chunkEnd >= 0 {
			return idx + chunkEnd
		}
		return -1
	}

	// 无 Content-Length 无 chunked → 以 headers 结束
	return idx
}

// findChunkedEnd 查找 chunked 编码的结束位置
func findChunkedEnd(data []byte) int {
	pos := 0
	for pos < len(data) {
		// 查找 \r\n
		nl := -1
		for i := pos; i < len(data)-1; i++ {
			if data[i] == '\r' && data[i+1] == '\n' {
				nl = i
				break
			}
		}
		if nl < 0 {
			return -1
		}
		// 解析 chunk size (hex)，使用 uint64 防溢出
		sizeStr := strings.TrimSpace(string(data[pos:nl]))
		var size uint64
		if _, err := fmt.Sscanf(sizeStr, "%x", &size); err != nil || size > 100*1024*1024 {
			return -1 // 无效或过大 chunk
		}
		if size == 0 {
			trailerEnd := nl + 2
			if trailerEnd+1 < len(data) && data[trailerEnd] == '\r' && data[trailerEnd+1] == '\n' {
				return trailerEnd + 2
			}
			if trailerEnd < len(data) && data[trailerEnd] == '\n' {
				return trailerEnd + 1
			}
			return -1
		}
		next := nl + 2 + int(size) + 2
		if next > len(data) {
			return -1
		}
		pos = next
	}
	return -1
}

// parseHTTPResponse 解析 HTTP 响应（字节级 body 提取）
func parseHTTPResponse(data []byte) *struct {
	StatusCode int
	Headers    map[string]string
	Body       string
} {
	headerEnd := indexDoubleCRLF(data)
	if headerEnd < 0 {
		return nil
	}

	return parseHTTPResponseFromHeader(data[:headerEnd], string(data[headerEnd+4:]))
}

// parseHTTPResponseFromHeader 从 header 字节和 body 字符串解析响应
func parseHTTPResponseFromHeader(headerData []byte, body string) *struct {
	StatusCode int
	Headers    map[string]string
	Body       string
} {
	headerStr := string(headerData)
	// 同时支持 CRLF 与 LF：先按 \n 切分，再去除行尾 \r，与 parseHTTPRequest 保持一致
	lines := strings.Split(headerStr, "\n")
	if len(lines) < 1 {
		return nil
	}

	firstLine := strings.TrimSuffix(lines[0], "\r")
	parts := strings.SplitN(firstLine, " ", 3)
	if len(parts) < 2 {
		return nil
	}
	var statusCode int
	if _, err := fmt.Sscanf(parts[1], "%d", &statusCode); err != nil || statusCode < 100 || statusCode > 599 {
		return nil
	}

	headers := make(map[string]string)
	for _, line := range lines[1:] {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			break
		}
		if ci := strings.Index(line, ":"); ci > 0 {
			headers[strings.TrimSpace(line[:ci])] = strings.TrimSpace(line[ci+1:])
		}
	}

	return &struct {
		StatusCode int
		Headers    map[string]string
		Body       string
	}{StatusCode: statusCode, Headers: headers, Body: body}
}

// parseContentLength 从 HTTP 头解析 Content-Length（健壮版）。
// 兼容 CRLF 与 LF 两种行尾：先按 \n 切分再去除行尾 \r，
// 否则纯 LF 响应会把整个头部当一行，Content-Length 永远找不到。
// Handles both CRLF and LF line endings: split on \n then trim \r,
// otherwise a bare-LF header block is treated as a single line.
func parseContentLength(headerData []byte) int {
	s := strings.ToLower(string(headerData))
	lines := strings.Split(s, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if after, ok := strings.CutPrefix(line, "content-length:"); ok {
			var cl int
			fmt.Sscanf(strings.TrimSpace(after), "%d", &cl)
			return cl
		}
	}
	return -1
}

// parseTransferEncoding 检查是否为 chunked 传输
func parseTransferEncoding(headerData []byte) string {
	data := headerData
	for len(data) > 0 {
		nl := indexByte(data, '\n')
		if nl < 0 {
			break
		}
		line := trimCR(bytesTrimSpace(data[:nl]))
		if hasPrefixFold(line, []byte("transfer-encoding:")) {
			return string(bytesTrimSpace(line[17:]))
		}
		data = data[nl+1:]
	}
	return ""
}

// parseHTTPRequest 解析 HTTP 请求（零分配版本）
func parseHTTPRequest(data []byte, srcIP net.IP, srcPort, dstPort uint16, engine *Engine) *models.CapturedRequest {
	headerEnd := indexDoubleCRLF(data)
	if headerEnd < 0 {
		return nil
	}

	headers := make(map[string]string, 16)
	host := ""
	var method, urlPath string

	rem := data[:headerEnd]
	// 请求行
	nl := indexByte(rem, '\n')
	if nl < 0 {
		return nil
	}
	firstLine := bytesTrimSpace(rem[:nl])
	sp1 := indexByte(firstLine, ' ')
	if sp1 < 0 {
		return nil
	}
	method = string(firstLine[:sp1])
	sp2 := indexByte(firstLine[sp1+1:], ' ')
	if sp2 >= 0 {
		urlPath = string(firstLine[sp1+1 : sp1+1+sp2])
	} else {
		urlPath = string(firstLine[sp1+1:])
	}
	rem = rem[nl+1:]

	// headers
	for len(rem) > 0 {
		nl = indexByte(rem, '\n')
		if nl < 0 {
			break
		}
		line := trimCR(bytesTrimSpace(rem[:nl]))
		if len(line) == 0 {
			rem = rem[nl+1:]
			break
		}
		colon := indexByte(line, ':')
		if colon > 0 {
			key := string(line[:colon])
			val := string(bytesTrimSpace(line[colon+1:]))
			headers[key] = val
			if strings.EqualFold(key, "Host") {
				host = val
			}
		}
		rem = rem[nl+1:]
	}
	headerEnd += 4 // skip \r\n\r\n

	if host == "" {
		// 回退为 IP 字面量（不含端口）：IPv6 整体括号化——IP 字面量无 host:port
		// 歧义，绝不能走 formatHostForURL 的拆分启发式
		host = formatIPForURL(srcIP)
	} else {
		// Host 头：无括号 IPv6 尾部数字组按 host:port 启发式处理（歧义无法消解，
		// 表达纯 IPv6 需方括号——见 formatHostForURL）
		host = formatHostForURL(host)
	}
	scheme := "http"
	isHTTPS := false
	if dstPort == 443 {
		scheme = "https"
		isHTTPS = true
	}
	if !strings.HasPrefix(urlPath, "/") {
		urlPath = "/" + urlPath
	}
	url := fmt.Sprintf("%s://%s%s", scheme, host, urlPath)

	body := ""
	if headerEnd < len(data) {
		body = string(data[headerEnd:])
	}

	var procPID int
	var procName string
	if engine != nil {
		if proc := engine.ResolveProcess(srcIP.String(), srcPort); proc != nil {
			procPID = proc.PID
			procName = proc.Name
		}
	}

	return &models.CapturedRequest{
		Method: method, URL: url, Host: host, Path: urlPath,
		Protocol: "HTTP/1.1", IsHTTPS: isHTTPS,
		ReqHeaders: headers, ReqBody: body,
		CapturedAt: time.Now(), CaptureMode: "nic",
		ProcessPID: procPID, ProcessName: procName,
	}
}

// ---- bytes helpers (零分配) ----

func indexByte(data []byte, c byte) int {
	for i, b := range data {
		if b == c {
			return i
		}
	}
	return -1
}

func indexDoubleCRLF(data []byte) int {
	for i := 0; i < len(data)-3; i++ {
		if data[i] == '\r' && data[i+1] == '\n' && data[i+2] == '\r' && data[i+3] == '\n' {
			return i
		}
	}
	return -1
}

func bytesTrimSpace(b []byte) []byte {
	for len(b) > 0 && b[0] == ' ' {
		b = b[1:]
	}
	for len(b) > 0 && b[len(b)-1] == ' ' {
		b = b[:len(b)-1]
	}
	return b
}

func trimCR(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == '\r' {
		b = b[:len(b)-1]
	}
	return b
}

func hasPrefixFold(b, prefix []byte) bool {
	if len(b) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		a, c := b[i], prefix[i]
		if a >= 'A' && a <= 'Z' {
			a += 'a' - 'A'
		}
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if a != c {
			return false
		}
	}
	return true
}

// formatIPForURL 将 IP 字面量格式化为 URL 主机段：IPv6 整体加方括号。
// 与 formatHostForURL 不同：IP 字面量不含端口，无 host:port 歧义（如
// "2001:db8::2:80" 是完整地址，不可拆出 ":80"）。
func formatIPForURL(ip net.IP) string {
	if ip == nil {
		return ""
	}
	if ip.To4() != nil {
		return ip.String()
	}
	return "[" + ip.String() + "]"
}

// formatHostForURL 为 URL 拼装格式化 Host 头的值：IPv6 字面量需加方括号，
// 否则生成 "http://::1/path" 之类的非法 URL。
// 裸 IPv6 尾部形如 ":8080"（无括号带端口，如 "fe80::1:8080"）时，
// 拆出数字端口再整体括号化："[fe80::1]:8080"。
// 仅用于 Host 头（host:port 语义）；IP 字面量请用 formatIPForURL。
// 已知歧义："2001:db8::2:80" 既可读作完整 IPv6 也可读作 host:port，
// Host 头语义下按后者处理（表达纯 IPv6 需方括号）。
func formatHostForURL(host string) string {
	if host == "" || strings.HasPrefix(host, "[") || strings.Count(host, ":") < 2 {
		return host
	}
	if i := strings.LastIndex(host, ":"); i > 0 && isAllDigits(host[i+1:]) && host[i-1] >= '0' && host[i-1] <= '9' {
		return "[" + host[:i] + "]:" + host[i+1:]
	}
	return "[" + host + "]"
}

func isAllDigits(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// emitTLSRecord 发送一条 TLS 连接记录（仅含 SNI 域名，无请求/响应内容）
func (s *TCPStream) emitTLSRecord(sni string) {
	req := &models.CapturedRequest{
		Method:      "TLS",
		URL:         "https://" + sni + "/",
		Host:        sni,
		Path:        "/",
		Protocol:    "HTTPS/TLS",
		IsHTTPS:     true,
		StatusCode:  0,
		ReqHeaders:  map[string]string{"TLS-SNI": sni, "Dst-Port": fmt.Sprintf("%d", s.dstPort)},
		ResHeaders:  map[string]string{},
		ReqBody:     "",
		ResBody:     "[TLS encrypted - content not available]",
		DurationMs:  0,
		SizeBytes:   0,
		CapturedAt:  time.Now(),
		CaptureMode: "nic",
	}
	if s.srcIP != nil {
		req.ReqHeaders["Src-IP"] = s.srcIP.String()
		req.ReqHeaders["Src-Port"] = fmt.Sprintf("%d", s.srcPort)
	}
	s.engine.emitNonBlocking(req)
}

// isTLSClientHello 检测数据是否为 TLS ClientHello
func isTLSClientHello(data []byte) bool {
	if len(data) < 6 {
		return false
	}
	return data[0] == 0x16 && data[1] == 0x03 && (data[2] >= 0x01 && data[2] <= 0x03)
}

// extractSNI 从 TLS ClientHello 中提取 SNI（Server Name Indication）
func extractSNI(data []byte) string {
	if len(data) < 44 {
		return ""
	}
	offset := 5 // TLS record header

	if offset+4 > len(data) {
		return ""
	}
	handshakeType := data[offset]
	if handshakeType != 0x01 {
		return ""
	}
	offset += 4

	if offset+2 > len(data) {
		return ""
	}
	offset += 2 // client version

	if offset+32 > len(data) {
		return ""
	}
	offset += 32 // random

	if offset >= len(data) {
		return ""
	}
	sessionIDLen := int(data[offset])
	offset += 1 + sessionIDLen

	if offset+2 > len(data) {
		return ""
	}
	cipherSuitesLen := int(data[offset])<<8 | int(data[offset+1])
	offset += 2 + cipherSuitesLen

	if offset >= len(data) {
		return ""
	}
	compressionMethodsLen := int(data[offset])
	offset += 1 + compressionMethodsLen

	if offset+2 > len(data) {
		return ""
	}
	extensionsLen := int(data[offset])<<8 | int(data[offset+1])
	offset += 2

	extensionsEnd := offset + extensionsLen
	if extensionsEnd > len(data) {
		extensionsEnd = len(data)
	}

	for offset+4 <= extensionsEnd {
		extType := int(data[offset])<<8 | int(data[offset+1])
		extLen := int(data[offset+2])<<8 | int(data[offset+3])
		offset += 4

		if extType == 0x0000 { // SNI extension
			if offset+2 > extensionsEnd {
				return ""
			}
			listLen := int(data[offset])<<8 | int(data[offset+1])
			offset += 2

			listEnd := offset + listLen
			if listEnd > extensionsEnd {
				listEnd = extensionsEnd
			}

			for offset+3 <= listEnd {
				nameType := data[offset]
				nameLen := int(data[offset+1])<<8 | int(data[offset+2])
				offset += 3

				if nameType == 0x00 && offset+nameLen <= listEnd {
					return string(data[offset : offset+nameLen])
				}
				offset += nameLen
			}
			return ""
		}
		offset += extLen
	}
	return ""
}
