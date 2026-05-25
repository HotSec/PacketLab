package capture

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os/exec"
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
	iface   string
	bpf     string
	handle  pcapHandle
	store   *store.Store
	hub     interface{ BroadcastCapture(req *models.CapturedRequest) }
	running atomic.Bool
	mu      sync.Mutex
	stopCh  chan struct{}
	stats   Stats

	streamPool  *TCPStreamPool
	assembler   *Assembler
	workers     int       // worker 数量
	workerChs   []chan gopacket.Packet // per-worker packet channels
	workerSg    sync.WaitGroup

	// 进程缓存
	procCache   map[string]*models.ProcessInfo
	procCacheMu sync.RWMutex

	// 批量发射 buffer (ring)
	emitBuf    []*models.CapturedRequest
	emitHead   int
	emitTail   int
	emitMu     sync.Mutex

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
	PacketsRecv  atomic.Int64
	HTTPFound    atomic.Int64
	PacketsDrop  atomic.Int64
	StreamsDrop  atomic.Int64
}

// New 创建抓包引擎
func New(iface, bpf string, st *store.Store,
	hub interface{ BroadcastCapture(req *models.CapturedRequest) }) *Engine {

	if bpf == "" {
		bpf = "tcp"
	}

	e := &Engine{
		iface:     iface,
		bpf:       bpf,
		store:     st,
		hub:       hub,
		stopCh:    make(chan struct{}),
		procCache: make(map[string]*models.ProcessInfo),
	}
	return e
}

// Stop 停止抓包（幂等）
func (e *Engine) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.running.Swap(false) {
		return
	}

	// 先关闭 handle，packetLoop 会退出并 close worker channels
	if e.handle != nil {
		e.handle.Close()
		e.handle = nil
	}

	select {
	case <-e.stopCh:
	default:
		close(e.stopCh)
	}

	// flush 剩余 emit 数据
	e.flushEmitBuf()
	// 停止异步写入池
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
		"packets":  e.stats.PacketsRecv.Load(),
		"http":     e.stats.HTTPFound.Load(),
		"running":  e.running.Load(),
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
	ipLayer := packet.Layer(layers.LayerTypeIPv4)
	tcpLayer := packet.Layer(layers.LayerTypeTCP)
	if ipLayer == nil || tcpLayer == nil {
		return 0
	}
	ip, ok1 := ipLayer.(*layers.IPv4)
	tcp, ok2 := tcpLayer.(*layers.TCP)
	if !ok1 || !ok2 {
		return 0
	}
	var sIP, dIP uint32
	for _, b := range ip.SrcIP.To4() {
		sIP = sIP<<8 | uint32(b)
	}
	for _, b := range ip.DstIP.To4() {
		dIP = dIP<<8 | uint32(b)
	}
	h := sIP ^ dIP ^ uint32(tcp.SrcPort) ^ uint32(tcp.DstPort)
	h = (h >> 16) ^ h
	return int(h) % e.workers
}

// workerLoop 独立 goroutine 处理数据包（per-worker Assembler + 流超时清理）
func (e *Engine) workerLoop(id int) {
	defer e.workerSg.Done()
	assembler := NewAssembler(e.streamPool)
	ch := e.workerChs[id]
	gcTicker := time.NewTicker(15 * time.Second)
	defer gcTicker.Stop()
	flushDeadline := time.Now().Add(2 * time.Minute)
	for {
		select {
		case packet, ok := <-ch:
			if !ok {
				assembler.FlushAllWithPending(e)
				return
			}
			assembler.Assemble(packet)
		case <-gcTicker.C:
			assembler.FlushOlderThan(flushDeadline, e)
			flushDeadline = time.Now().Add(2 * time.Minute)
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
		e.stats.HTTPFound.Add(1)
		if e.hub != nil && req.ID > 0 {
			e.hub.BroadcastCapture(req)
		}
	}
}

// emitBatch 缓冲区（非阻塞，防 backpressure）
var emitBuf struct {
	mu    sync.Mutex
	items []*models.CapturedRequest
}

func (e *Engine) emitNonBlocking(req *models.CapturedRequest) {
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

// ResolveProcess 解析进程信息（批量 lsof 建表 + 缓存）
func (e *Engine) ResolveProcess(srcIP net.IP, srcPort uint16) *models.ProcessInfo {
	key := fmt.Sprintf("%s:%d", srcIP.String(), srcPort)
	e.procCacheMu.RLock()
	if p, ok := e.procCache[key]; ok {
		e.procCacheMu.RUnlock()
		return p
	}
	e.procCacheMu.RUnlock()

	// 批量获取 lsof 输出，构建端口→进程表
	table := buildProcTable()
	if table == nil {
		return nil
	}

	// 将整张表放入缓存
	e.procCacheMu.Lock()
	for k, v := range table {
		e.procCache[k] = v
	}
	e.procCacheMu.Unlock()

	return table[key]
}

// buildProcTable 一次 lsof 调用构建端口→进程全表
func buildProcTable() map[string]*models.ProcessInfo {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "lsof", "-i", "tcp", "-n", "-P", "-sTCP:ESTABLISHED")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	table := make(map[string]*models.ProcessInfo)
	// lsof NAME 格式: "host:port->host:port" 或 "*:port"
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 9 {
			continue
		}
		name := fields[0]
		pid := 0
		fmt.Sscanf(fields[1], "%d", &pid)
		// NAME 字段在索引 8
		addr := fields[len(fields)-1]
		// 解析 "localIP:localPort->..."
		if idx := strings.Index(addr, "->"); idx > 0 {
			addr = addr[:idx]
		}
		table[addr] = &models.ProcessInfo{Name: name, PID: pid}
	}
	return table
}

// ========================================
// TCP Assembler (简化版 tcpassembly)
// ========================================

// Assembler TCP 流重组器
type Assembler struct {
	pool    *TCPStreamPool
	streams map[string]*TCPStream
	mu      sync.Mutex
}

// NewAssembler 创建重组器
func NewAssembler(pool *TCPStreamPool) *Assembler {
	return &Assembler{
		pool:    pool,
		streams: make(map[string]*TCPStream),
	}
}

// Assemble 组装数据包到流中（双方向归一化）
func (a *Assembler) Assemble(packet gopacket.Packet) {
	ipLayer := packet.Layer(layers.LayerTypeIPv4)
	tcpLayer := packet.Layer(layers.LayerTypeTCP)
	if ipLayer == nil || tcpLayer == nil {
		return
	}
	ip, ok1 := ipLayer.(*layers.IPv4)
	tcp, ok2 := tcpLayer.(*layers.TCP)
	if !ok1 || !ok2 {
		return
	}

	isFIN := tcp.FIN
	isRST := tcp.RST

	srcIPStr := ip.SrcIP.String()
	dstIPStr := ip.DstIP.String()
	srcPort := uint16(tcp.SrcPort)
	dstPort := uint16(tcp.DstPort)

	isClientToServer := determineDirection(srcPort, dstPort)

	var clientIP net.IP
	var clientPort, serverPort uint16
	if isClientToServer {
		clientIP = ip.SrcIP
		clientPort = srcPort
		serverPort = dstPort
	} else {
		clientIP = ip.DstIP
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
	streamKey := fmt.Sprintf("%s:%d-%s:%d", keyA, portA, keyB, portB)

	a.mu.Lock()
	stream, ok := a.streams[streamKey]
	if !ok {
		if len(a.streams) >= 10000 {
			a.mu.Unlock()
			a.pool.engine.stats.StreamsDrop.Add(1)
			return
		}
		stream = a.pool.New(clientIP, clientPort, serverPort)
		a.streams[streamKey] = stream
	}
	a.mu.Unlock()

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
			s.tryExtractHTTPOnClose()
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
			s.tryExtractHTTPOnClose()
		}
		s.mu.Unlock()
	}
	a.streams = make(map[string]*TCPStream)
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
	clientBuf  []byte // 客户端→服务端 数据
	serverBuf  []byte // 服务端→客户端 数据
	lastActive time.Time
	pendingReq *models.CapturedRequest
	nonHTTP    bool   // 标记为非HTTP流，跳过后续处理
	firstData  bool   // 是否已收到首批数据（用于非HTTP检测）
	sniEmitted bool   // 是否已emit TLS SNI记录
}

const streamBufMax = 2 * 1024 * 1024 // 2MB max per stream

// Feed 喂入 TCP 数据（按方向分离缓冲区）
func (s *TCPStream) Feed(data []byte, clientToServer bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastActive = time.Now()

	if s.nonHTTP {
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

	if len(s.clientBuf) > streamBufMax {
		s.clientBuf = truncateBuffer(s.clientBuf, 256*1024)
	}
	if len(s.serverBuf) > streamBufMax {
		s.serverBuf = truncateBuffer(s.serverBuf, 256*1024)
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
func (s *TCPStream) HandleClose(a *Assembler) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.nonHTTP {
		a.mu.Lock()
		for key, stream := range a.streams {
			if stream == s {
				delete(a.streams, key)
				break
			}
		}
		a.mu.Unlock()
		return
	}

	s.tryExtractHTTPOnClose()

	a.mu.Lock()
	for key, stream := range a.streams {
		if stream == s {
			delete(a.streams, key)
			break
		}
	}
	a.mu.Unlock()
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
						s.pendingReq = nil
						s.serverBuf = nil
						s.clientBuf = nil
						return
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
	lines := strings.Split(headerStr, "\r\n")
	if len(lines) < 1 {
		return nil
	}

	parts := strings.SplitN(lines[0], " ", 3)
	if len(parts) < 2 {
		return nil
	}
	var statusCode int
	if _, err := fmt.Sscanf(parts[1], "%d", &statusCode); err != nil || statusCode < 100 || statusCode > 599 {
		return nil
	}

	headers := make(map[string]string)
	for _, line := range lines[1:] {
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

// parseContentLength 从 HTTP 头解析 Content-Length（健壮版）
func parseContentLength(headerData []byte) int {
	s := strings.ToLower(string(headerData))
	lines := strings.Split(s, "\r\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
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
		host = srcIP.String()
	}
	scheme := "http"
	isHTTPS := false
	if dstPort == 443 {
		scheme = "https"
		isHTTPS = true
	}
	url := fmt.Sprintf("%s://%s%s", scheme, host, urlPath)
	if !strings.HasPrefix(urlPath, "/") {
		url = fmt.Sprintf("%s://%s/%s", scheme, host, urlPath)
	}

	body := ""
	if headerEnd < len(data) {
		body = string(data[headerEnd:])
	}

	var procPID int
	var procName string
	if engine != nil {
		if proc := engine.ResolveProcess(srcIP, srcPort); proc != nil {
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
