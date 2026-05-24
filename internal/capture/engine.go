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
	"github.com/google/gopacket/pcap"
)

// Engine 网卡抓包引擎
type Engine struct {
	iface   string
	bpf     string
	handle  *pcap.Handle
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

// Stats 抓包统计
type Stats struct {
	PacketsRecv atomic.Int64
	HTTPFound   atomic.Int64
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

// Start 启动抓包
func (e *Engine) Start() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.running.Load() {
		return fmt.Errorf("capture already running")
	}

	// 重新创建 stopCh
	e.stopCh = make(chan struct{})

	var err error
	e.handle, err = pcap.OpenLive(e.iface, 65536, true, pcap.BlockForever)
	if err != nil {
		return fmt.Errorf("pcap.OpenLive(%s): %w", e.iface, err)
	}

	if err := e.handle.SetBPFFilter(e.bpf); err != nil {
		e.handle.Close()
		e.handle = nil
		return fmt.Errorf("SetBPFFilter(%s): %w", e.bpf, err)
	}

	slog.Info("capture: 开始抓包", "iface", e.iface, "bpf", e.bpf)

	// 多 worker 并行处理
	e.workers = 4
	e.streamPool = NewTCPStreamPool(e)
	e.workerChs = make([]chan gopacket.Packet, e.workers)
	for i := range e.workerChs {
		e.workerChs[i] = make(chan gopacket.Packet, 256)
	}

	e.running.Store(true)

	// 2.5Gbps: 初始化内存环形缓冲区 + 异步写入池
	e.ringBuf = NewMemRingBuffer(262144) // 262K entries
	e.writer = NewAsyncWriterPool(e.store, e.ringBuf, 4, 30*time.Millisecond)
	e.writer.Start()

	for i := 0; i < e.workers; i++ {
		e.workerSg.Add(1)
		go e.workerLoop(i)
	}
	go e.packetLoop()
	go e.flushLoop()
	return nil
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

// packetLoop 读取数据包并分发到 worker
func (e *Engine) packetLoop() {
	packetSource := gopacket.NewPacketSource(e.handle, e.handle.LinkType())
	lastReport := time.Now()
	workerIdx := 0
	for packet := range packetSource.Packets() {
		if !e.running.Load() {
			return
		}
		e.stats.PacketsRecv.Add(1)
		// 轮询分发到 worker
		ch := e.workerChs[workerIdx]
		workerIdx = (workerIdx + 1) % e.workers
		select {
		case ch <- packet:
		default:
			// worker 满，丢弃
		}
		if now := time.Now(); now.Sub(lastReport) >= 5*time.Second {
			slog.Debug("capture: 数据包统计", "packets", e.stats.PacketsRecv.Load(), "http", e.stats.HTTPFound.Load())
			lastReport = now
		}
	}
	// 关闭 worker channels
	for _, ch := range e.workerChs {
		close(ch)
	}
	e.workerSg.Wait()
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
	if _, err := e.store.SaveBatch(reqs); err != nil {
		slog.Warn("capture: bulk save failed", "count", len(reqs), "error", err)
		return
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
	if !e.running.Load() {
		return
	}
	// 2.5Gbps: 直接写入内存环形缓冲区（零阻塞）
	if e.ringBuf != nil {
		e.ringBuf.Push(req)
		return
	}
	// 回退到旧 emit 管道
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

// bulkEmit 批量输出

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

// resolveProcessDarwin macOS 进程解析
// ListInterfaces 列出可用网卡
func ListInterfaces() ([]string, error) {
	devs, err := pcap.FindAllDevs()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, d := range devs {
		if len(d.Addresses) > 0 {
			names = append(names, d.Name)
		}
	}
	return names, nil
}

// DetectInterface 自动检测活跃网卡
func DetectInterface() string {
	devs, err := pcap.FindAllDevs()
	if err != nil {
		return "en0"
	}
	// 优先匹配物理网卡
	preferred := []string{"en0", "en1", "eth0", "wlp2s0", "wlan0"}
	for _, name := range preferred {
		for _, d := range devs {
			if d.Name == name && len(d.Addresses) > 0 {
				slog.Info("capture: 自动检测网卡", "iface", name)
				return name
			}
		}
	}
	// 回退：第一个有 IPv4 地址的非 loopback 非 utun 网卡
	for _, d := range devs {
		if strings.HasPrefix(d.Name, "utun") || strings.HasPrefix(d.Name, "lo") {
			continue
		}
		for _, addr := range d.Addresses {
			ip := addr.IP
			if ip != nil && !ip.IsLoopback() && ip.To4() != nil &&
				!strings.HasPrefix(ip.String(), "169.254") {
				slog.Info("capture: 回退网卡", "iface", d.Name)
				return d.Name
			}
		}
	}
	return "en0"
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

	var keyA, keyB string
	var portA, portB uint16
	var ipA, ipB net.IP
	if srcIPStr < dstIPStr || (srcIPStr == dstIPStr && srcPort < dstPort) {
		keyA, keyB = srcIPStr, dstIPStr
		portA, portB = srcPort, dstPort
		ipA, ipB = ip.SrcIP, ip.DstIP
	} else {
		keyA, keyB = dstIPStr, srcIPStr
		portA, portB = dstPort, srcPort
		ipA, ipB = ip.DstIP, ip.SrcIP
	}
	streamKey := fmt.Sprintf("%s:%d-%s:%d", keyA, portA, keyB, portB)

	a.mu.Lock()
	stream, ok := a.streams[streamKey]
	if !ok {
		if len(a.streams) >= 2000 {
			a.mu.Unlock()
			return
		}
		stream = a.pool.New(ipA, layers.TCPPort(portA), ipB, layers.TCPPort(portB))
		a.streams[streamKey] = stream
	}
	a.mu.Unlock()

	if len(tcp.Payload) > 0 {
		isClientToServer := srcIPStr == keyA && srcPort == portA
		stream.Feed(tcp.Payload, isClientToServer)
	}

	if isFIN || isRST {
		stream.HandleClose(a)
	}
}

// FlushOlderThan 清理过期流（持流锁保护，先 emit pendingReq）
func (a *Assembler) FlushOlderThan(t time.Time, engine *Engine) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, s := range a.streams {
		s.mu.Lock()
		if s.lastActive.Before(t) {
			if s.pendingReq != nil {
				engine.emit(s.pendingReq)
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
		if s.pendingReq != nil {
			engine.emit(s.pendingReq)
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
func (p *TCPStreamPool) New(srcIP net.IP, srcPort layers.TCPPort, dstIP net.IP, dstPort layers.TCPPort) *TCPStream {
	return &TCPStream{
		engine:     p.engine,
		srcIP:      srcIP,
		srcPort:    uint16(srcPort),
		dstPort:    uint16(dstPort),
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
}

const streamBufMax = 2 * 1024 * 1024 // 2MB max per stream

// Feed 喂入 TCP 数据（按方向分离缓冲区）
func (s *TCPStream) Feed(data []byte, clientToServer bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastActive = time.Now()

	if clientToServer {
		s.clientBuf = append(s.clientBuf, data...)
	} else {
		s.serverBuf = append(s.serverBuf, data...)
	}
	s.tryExtractHTTP()
	if len(s.clientBuf) > streamBufMax {
		s.clientBuf = s.clientBuf[len(s.clientBuf)-256*1024:]
	}
	if len(s.serverBuf) > streamBufMax {
		s.serverBuf = s.serverBuf[len(s.serverBuf)-256*1024:]
	}
}

// HandleClose 处理 TCP 连接关闭（FIN/RST），先尝试提取残留数据再 emit
func (s *TCPStream) HandleClose(a *Assembler) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.tryExtractHTTP()

	if s.pendingReq != nil {
		s.engine.emitNonBlocking(s.pendingReq)
		s.pendingReq = nil
	}
}

// tryExtractHTTP 从双缓冲区中提取 HTTP 请求/响应并消费
func (s *TCPStream) tryExtractHTTP() {
	for {
		// 1. 如果没有 pending 请求，尝试从 clientBuf 提取
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

		// 2. 有 pending 请求，尝试从 serverBuf 提取响应
		if len(s.serverBuf) == 0 {
			return
		}
		idx := findHTTPMessageEnd(s.serverBuf)
		if idx < 0 {
			return
		}
		msgData := s.serverBuf[:idx]
		s.serverBuf = s.serverBuf[idx:]
		if isHTTPResponse(msgData) {
			resp := parseHTTPResponse(msgData)
			if resp != nil {
				s.pendingReq.StatusCode = resp.StatusCode
				s.pendingReq.ResHeaders = resp.Headers
				s.pendingReq.ResBody = resp.Body
				s.pendingReq.DurationMs = time.Since(s.pendingReq.CapturedAt).Milliseconds()
				s.pendingReq.SizeBytes = int64(len(msgData))
				s.engine.emitNonBlocking(s.pendingReq)
				s.pendingReq = nil
			}
		}
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
	if cl > 0 {
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
	headerEnd := -1
	for i := 0; i < len(data)-3; i++ {
		if data[i] == '\r' && data[i+1] == '\n' && data[i+2] == '\r' && data[i+3] == '\n' {
			headerEnd = i
			break
		}
	}
	if headerEnd < 0 {
		return nil
	}

	headerData := string(data[:headerEnd])
	lines := strings.Split(headerData, "\r\n")
	if len(lines) < 1 {
		return nil
	}

	// 状态行: HTTP/1.1 200 OK
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

	body := ""
	bodyBytes := data[headerEnd+4:]
	if len(bodyBytes) > 0 {
		body = string(bodyBytes)
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
