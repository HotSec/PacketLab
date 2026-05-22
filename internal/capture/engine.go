package capture

import (
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

	streamPool *TCPStreamPool
	assembler  *Assembler

	// 进程缓存
	procCache   map[string]*models.ProcessInfo
	procCacheMu sync.RWMutex
}

// Stats 抓包统计
type Stats struct {
	PacketsRecv atomic.Int64
	HTTPFound   atomic.Int64
}

func (s Stats) Packets() int64  { return s.PacketsRecv.Load() }
func (s Stats) HTTP() int64     { return s.HTTPFound.Load() }

// New 创建抓包引擎
func New(iface, bpf string, st *store.Store,
	hub interface{ BroadcastCapture(req *models.CapturedRequest) }) *Engine {

	if bpf == "" {
		bpf = "tcp port 80 or tcp port 443"
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

	e.running.Store(true)
	e.streamPool = NewTCPStreamPool(e)
	e.assembler = NewAssembler(e.streamPool)

	go e.packetLoop()
	go e.gcLoop()
	return nil
}

// Stop 停止抓包（幂等，可多次调用）
func (e *Engine) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.running.Swap(false) {
		return // 已经在停止中或已停止
	}

	// 先关闭 handle，让 packetLoop 退出
	if e.handle != nil {
		e.handle.Close()
		e.handle = nil
	}

	// 关闭 stopCh（gcLoop 会退出）
	select {
	case <-e.stopCh:
		// 已关闭
	default:
		close(e.stopCh)
	}

	// 清理流——先 emit 所有 pendingReq
	if e.assembler != nil {
		e.assembler.FlushAllWithPending(e)
		e.assembler = nil
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

// packetLoop 循环读取数据包
func (e *Engine) packetLoop() {
	packetSource := gopacket.NewPacketSource(e.handle, e.handle.LinkType())
	lastReport := time.Now()
	for packet := range packetSource.Packets() {
		if !e.running.Load() {
			return
		}
		e.stats.PacketsRecv.Add(1)
		e.assembler.Assemble(packet)
		// 每 5 秒输出统计
		if now := time.Now(); now.Sub(lastReport) >= 5*time.Second {
			slog.Debug("capture: 数据包统计", "packets", e.stats.PacketsRecv.Load(), "http", e.stats.HTTPFound.Load())
			lastReport = now
		}
	}
}

// gcLoop 定期清理过期流
func (e *Engine) gcLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for e.running.Load() {
		select {
		case <-ticker.C:
			e.assembler.FlushOlderThan(time.Now().Add(-5*time.Minute), e)
		case <-e.stopCh:
			return
		}
	}
}

// emit 输出 HTTP 请求到存储
func (e *Engine) emit(req *models.CapturedRequest) {
	req.CaptureMode = "nic"
	e.store.Save(req)
	if e.hub != nil {
		e.hub.BroadcastCapture(req)
	}
	e.stats.HTTPFound.Add(1)
}

// ResolveProcess 解析进程信息（带缓存）
func (e *Engine) ResolveProcess(srcIP net.IP, srcPort uint16) *models.ProcessInfo {
	key := fmt.Sprintf("%s:%d", srcIP.String(), srcPort)
	e.procCacheMu.RLock()
	if p, ok := e.procCache[key]; ok {
		e.procCacheMu.RUnlock()
		return p
	}
	e.procCacheMu.RUnlock()

	p := resolveProcessDarwin(srcIP, srcPort)
	e.procCacheMu.Lock()
	e.procCache[key] = p
	e.procCacheMu.Unlock()
	return p
}

// resolveProcessDarwin macOS 进程解析
func resolveProcessDarwin(srcIP net.IP, srcPort uint16) *models.ProcessInfo {
	// lsof -i tcp -n -P -sTCP:ESTABLISHED | grep "localhost:<port>" 或 "<ip>:<port>"
	cmd := exec.Command("lsof", "-i", "tcp", "-n", "-P", "-sTCP:ESTABLISHED")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	portStr := fmt.Sprintf(":%d->", srcPort)
	ipStr := srcIP.String()
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, portStr) || strings.Contains(line, ipStr+":") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				name := fields[0]
				pid := 0
				fmt.Sscanf(fields[1], "%d", &pid)
				return &models.ProcessInfo{Name: name, PID: pid}
			}
		}
	}
	return nil
}

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
	ip, _ := ipLayer.(*layers.IPv4)
	tcp, _ := tcpLayer.(*layers.TCP)

	if len(tcp.Payload) == 0 {
		return
	}

	// 归一化 4 元组流键：同一 TCP 连接的双方向映射到同一流
	// key = "min(src,dst):min(sport,dport):max(src,dst):max(sport,dport)"
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
		if len(a.streams) >= 1000 {
			a.mu.Unlock()
			return
		}
		stream = a.pool.New(ipA, layers.TCPPort(portA), ipB, layers.TCPPort(portB))
		a.streams[streamKey] = stream
	}
	a.mu.Unlock()

	// 判断方向：src 是否等于流记录的 A 端
	isClientToServer := srcIPStr == keyA && srcPort == portA
	stream.Feed(tcp.Payload, isClientToServer)
}

// FlushOlderThan 清理过期流（先 emit pendingReq）
func (a *Assembler) FlushOlderThan(t time.Time, engine *Engine) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, s := range a.streams {
		if s.lastActive.Before(t) {
			if s.pendingReq != nil {
				engine.emit(s.pendingReq)
			}
			delete(a.streams, key)
		}
	}
}

// FlushAllWithPending 清理所有流并 emit pendingReq
func (a *Assembler) FlushAllWithPending(engine *Engine) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.streams {
		if s.pendingReq != nil {
			engine.emit(s.pendingReq)
		}
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
		lastActive: time.Now(),
	}
}

// TCPStream 单个 TCP 流（双方向数据合并）
type TCPStream struct {
	engine     *Engine
	srcIP      net.IP
	srcPort    uint16
	buf        []byte
	lastActive time.Time
	pendingReq *models.CapturedRequest // 等待匹配响应的请求
}

// Feed 喂入 TCP 数据（direction=true 表示 client→server）
func (s *TCPStream) Feed(data []byte, clientToServer bool) {
	s.lastActive = time.Now()
	s.buf = append(s.buf, data...)
	if len(s.buf) > 2*1024*1024 {
		s.buf = s.buf[len(s.buf)-2*1024*1024:]
	}
	s.tryExtractHTTP()
}

// tryExtractHTTP 尝试从流中提取 HTTP 请求和响应
func (s *TCPStream) tryExtractHTTP() {
	for {
		idx := findHTTPMessageEnd(s.buf)
		if idx < 0 {
			return
		}
		msgData := s.buf[:idx]
		s.buf = s.buf[idx:]

		// 判断是请求还是响应
		if isHTTPResponse(msgData) {
			resp := parseHTTPResponse(msgData)
			if resp != nil && s.pendingReq != nil {
				s.pendingReq.StatusCode = resp.StatusCode
				s.pendingReq.ResHeaders = resp.Headers
				s.pendingReq.ResBody = resp.Body
				s.pendingReq.DurationMs = time.Since(s.pendingReq.CapturedAt).Milliseconds()
				s.pendingReq.SizeBytes = int64(len(msgData))
				s.engine.emit(s.pendingReq)
				s.pendingReq = nil
			}
		} else {
			req := parseHTTPRequest(msgData, s.srcIP, s.srcPort, s.engine)
			if req != nil {
				// 保存为 pending，等待响应到达后补全再 emit
				if s.pendingReq != nil {
					// 前一个请求未收到响应（pipeline），直接 emit
					s.engine.emit(s.pendingReq)
				}
				s.pendingReq = req
			}
		}
	}
}

// isHTTPResponse 判断是否是 HTTP 响应（以 HTTP/ 开头）
func isHTTPResponse(data []byte) bool {
	return len(data) >= 4 && (string(data[:4]) == "HTTP")
}

// findHTTPMessageEnd 查找 HTTP 消息结束位置（支持 Content-Length 和 chunked）
func findHTTPMessageEnd(data []byte) int {
	idx := -1
	for i := 0; i < len(data)-3; i++ {
		if data[i] == '\r' && data[i+1] == '\n' && data[i+2] == '\r' && data[i+3] == '\n' {
			idx = i + 4
			break
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
		// 解析 chunk size (hex)
		sizeStr := string(data[pos:nl])
		sizeStr = strings.TrimSpace(sizeStr)
		var size int
		fmt.Sscanf(sizeStr, "%x", &size)
		if size == 0 {
			// 最后一个 chunk，需要 trailing \r\n
			trailerEnd := nl + 2
			if trailerEnd+1 < len(data) && data[trailerEnd] == '\r' && data[trailerEnd+1] == '\n' {
				return trailerEnd + 2
			}
			if trailerEnd < len(data) && data[trailerEnd] == '\n' {
				return trailerEnd + 1
			}
			return -1 // 等待更多数据
		}
		// 跳过 chunk data + \r\n
		next := nl + 2 + size + 2 // \r\n + data + \r\n
		if next > len(data) {
			return -1
		}
		pos = next
	}
	return -1
}

// parseHTTPResponse 解析 HTTP 响应
func parseHTTPResponse(data []byte) *struct {
	StatusCode int
	Headers    map[string]string
	Body       string
} {
	s := string(data)
	lines := strings.Split(s, "\r\n")
	if len(lines) < 1 {
		return nil
	}
	// 状态行: HTTP/1.1 200 OK
	parts := strings.SplitN(lines[0], " ", 3)
	if len(parts) < 2 {
		return nil
	}
	var statusCode int
	fmt.Sscanf(parts[1], "%d", &statusCode)
	if statusCode < 100 || statusCode > 599 {
		return nil
	}

	headers := make(map[string]string)
	bodyStart := -1
	for i, line := range lines[1:] {
		if line == "" {
			bodyStart = i + 2
			break
		}
		if ci := strings.Index(line, ":"); ci > 0 {
			headers[strings.TrimSpace(line[:ci])] = strings.TrimSpace(line[ci+1:])
		}
	}

	body := ""
	if bodyStart > 0 && bodyStart < len(lines) {
		body = strings.Join(lines[bodyStart:], "\r\n")
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
	s := strings.ToLower(string(headerData))
	lines := strings.Split(s, "\r\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "transfer-encoding:"); ok {
			return strings.TrimSpace(after)
		}
	}
	return ""
}

// parseHTTPRequest 解析 HTTP 请求
func parseHTTPRequest(data []byte, srcIP net.IP, srcPort uint16, engine *Engine) *models.CapturedRequest {
	s := string(data)
	lines := strings.Split(s, "\r\n")
	if len(lines) < 1 {
		return nil
	}

	// 请求行: METHOD URL HTTP/1.1
	parts := strings.SplitN(lines[0], " ", 3)
	if len(parts) < 2 {
		return nil
	}
	method := parts[0]
	urlPath := parts[1]

	// 解析 Headers
	headers := make(map[string]string)
	host := ""
	bodyStart := -1
	for i, line := range lines[1:] {
		if line == "" {
			bodyStart = i + 2 // 跳过空行
			break
		}
		if colonIdx := strings.Index(line, ":"); colonIdx > 0 {
			key := strings.TrimSpace(line[:colonIdx])
			val := strings.TrimSpace(line[colonIdx+1:])
			headers[key] = val
			if strings.EqualFold(key, "Host") {
				host = val
			}
		}
	}

	if host == "" {
		host = srcIP.String()
	}

	// 构造 URL
	url := fmt.Sprintf("https://%s%s", host, urlPath)
	if !strings.HasPrefix(urlPath, "/") {
		url = fmt.Sprintf("https://%s/%s", host, urlPath)
	}

	body := ""
	if bodyStart > 0 && bodyStart < len(lines) {
		body = strings.Join(lines[bodyStart:], "\r\n")
	}

	// 进程关联（使用引擎缓存）
	var procPID int
	var procName string
	if proc := engine.ResolveProcess(srcIP, srcPort); proc != nil {
		procPID = proc.PID
		procName = proc.Name
	}

	return &models.CapturedRequest{
		Method:      method,
		URL:         url,
		Host:        host,
		Path:        urlPath,
		Protocol:    "HTTP/1.1",
		IsHTTPS:     false,
		ReqHeaders:  headers,
		ReqBody:     body,
		CapturedAt:  time.Now(),
		CaptureMode: "nic",
		ProcessPID:  procPID,
		ProcessName: procName,
	}
}
