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
	stopCh  chan struct{}
	stats   Stats
	mu      sync.Mutex

	streamPool *TCPStreamPool
	assembler  *Assembler
}

// Stats 抓包统计
type Stats struct {
	PacketsRecv atomic.Int64
	HTTPFound   atomic.Int64
}

// New 创建抓包引擎
func New(iface, bpf string, st *store.Store,
	hub interface{ BroadcastCapture(req *models.CapturedRequest) }) *Engine {

	if bpf == "" {
		bpf = "tcp port 80 or tcp port 443"
	}

	e := &Engine{
		iface:  iface,
		bpf:    bpf,
		store:  st,
		hub:    hub,
		stopCh: make(chan struct{}),
	}
	return e
}

// Start 启动抓包
func (e *Engine) Start() error {
	// 打开网卡
	var err error
	e.handle, err = pcap.OpenLive(e.iface, 65536, true, pcap.BlockForever)
	if err != nil {
		return fmt.Errorf("pcap.OpenLive(%s): %w", e.iface, err)
	}

	if err := e.handle.SetBPFFilter(e.bpf); err != nil {
		e.handle.Close()
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

// Stop 停止抓包
func (e *Engine) Stop() {
	e.running.Store(false)
	close(e.stopCh)
	if e.handle != nil {
		e.handle.Close()
	}
	if e.assembler != nil {
		e.assembler.FlushAll()
	}
	slog.Info("capture: 抓包已停止")
}

// IsRunning 是否运行中
func (e *Engine) IsRunning() bool {
	return e.running.Load()
}

// GetStats 获取统计
func (e *Engine) GetStats() Stats {
	return Stats{
		PacketsRecv: atomic.Int64{},
		HTTPFound:   atomic.Int64{},
	}
}

// packetLoop 循环读取数据包
func (e *Engine) packetLoop() {
	packetSource := gopacket.NewPacketSource(e.handle, e.handle.LinkType())
	for packet := range packetSource.Packets() {
		if !e.running.Load() {
			return
		}
		e.stats.PacketsRecv.Add(1)
		e.assembler.Assemble(packet)
	}
}

// gcLoop 定期清理过期流
func (e *Engine) gcLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for e.running.Load() {
		select {
		case <-ticker.C:
			e.assembler.FlushOlderThan(time.Now().Add(-5 * time.Minute))
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

// ResolveProcess 解析进程信息（平台相关）
func ResolveProcess(srcIP net.IP, srcPort uint16) *models.ProcessInfo {
	return resolveProcessDarwin(srcIP, srcPort)
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
	for _, d := range devs {
		for _, addr := range d.Addresses {
			ip := addr.IP
			if ip != nil && !ip.IsLoopback() && ip.To4() != nil && !strings.HasPrefix(ip.String(), "169.254") {
				return d.Name
			}
		}
	}
	// 回退到常见网卡
	for _, name := range []string{"en0", "eth0", "wlp2s0"} {
		for _, d := range devs {
			if d.Name == name {
				return name
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

// Assemble 组装数据包到流中
func (a *Assembler) Assemble(packet gopacket.Packet) {
	ipLayer := packet.Layer(layers.LayerTypeIPv4)
	tcpLayer := packet.Layer(layers.LayerTypeTCP)
	if ipLayer == nil || tcpLayer == nil {
		return
	}
	ip, _ := ipLayer.(*layers.IPv4)
	tcp, _ := tcpLayer.(*layers.TCP)

	// 只处理有数据的 TCP 包
	if len(tcp.Payload) == 0 {
		return
	}

	// 流 ID = srcIP:srcPort->dstIP:dstPort
	streamKey := fmt.Sprintf("%s:%d", ip.SrcIP, tcp.SrcPort)

	a.mu.Lock()
	stream, ok := a.streams[streamKey]
	if !ok {
		if len(a.streams) >= 1000 {
			a.mu.Unlock()
			return
		}
		stream = a.pool.New(ip.SrcIP, tcp.SrcPort, ip.DstIP, tcp.DstPort)
		a.streams[streamKey] = stream
	}
	a.mu.Unlock()

	stream.Feed(tcp.Payload)
}

// FlushOlderThan 清理过期流
func (a *Assembler) FlushOlderThan(t time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, s := range a.streams {
		if s.lastActive.Before(t) {
			delete(a.streams, key)
		}
	}
}

// FlushAll 清理所有流
func (a *Assembler) FlushAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
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

// TCPStream 单个 TCP 流
type TCPStream struct {
	engine     *Engine
	srcIP      net.IP
	srcPort    uint16
	buf        []byte
	lastActive time.Time
}

// Feed 喂入 TCP 数据
func (s *TCPStream) Feed(data []byte) {
	s.lastActive = time.Now()
	s.buf = append(s.buf, data...)
	if len(s.buf) > 2*1024*1024 {
		s.buf = s.buf[len(s.buf)-2*1024*1024:] // 截断到 2MB
	}
	s.tryExtractHTTP()
}

// tryExtractHTTP 尝试从流中提取 HTTP 请求
func (s *TCPStream) tryExtractHTTP() {
	for {
		idx := findHTTPRequestEnd(s.buf)
		if idx < 0 {
			return
		}
		reqData := s.buf[:idx]
		s.buf = s.buf[idx:]

		req := parseHTTPRequest(reqData, s.srcIP, s.srcPort)
		if req != nil {
			s.engine.emit(req)
		}
	}
}

// findHTTPRequestEnd 查找 HTTP 请求结束位置
func findHTTPRequestEnd(data []byte) int {
	// 查找 \r\n\r\n（请求头结束）
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
	// 检查是否有 Content-Length
	cl := parseContentLength(data[:idx])
	if cl > 0 {
		bodyEnd := idx + cl
		if bodyEnd <= len(data) {
			return bodyEnd
		}
		return -1 // 等待更多数据
	}
	return idx
}

// parseContentLength 从 HTTP 头解析 Content-Length
func parseContentLength(headerData []byte) int {
	s := string(headerData)
	lines := strings.Split(s, "\r\n")
	for _, line := range lines {
		if len(line) > 16 && strings.EqualFold(line[:15], "content-length:") {
			var cl int
			fmt.Sscanf(strings.TrimSpace(line[15:]), "%d", &cl)
			return cl
		}
	}
	return -1
}

// parseHTTPRequest 解析 HTTP 请求
func parseHTTPRequest(data []byte, srcIP net.IP, srcPort uint16) *models.CapturedRequest {
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

	// 进程关联
	var procPID int
	var procName string
	if proc := ResolveProcess(srcIP, srcPort); proc != nil {
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
