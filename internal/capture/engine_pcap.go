//go:build (linux || darwin) && cgo

package capture

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/pcap"
)

// bpfCache 缓存已编译的 BPF 指令，避免重复启停抓包时重新编译同一表达式。
// key: "iface|linkType|snaplen|expr"，value: 编译后的指令（不可变，可安全并发复用）。
var bpfCache sync.Map

func (e *Engine) Start() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.running.Load() {
		return fmt.Errorf("capture already running")
	}

	e.stopCh = make(chan struct{})

	// 使用 InactiveHandle 配置内核缓冲区后再激活，避免高吞吐丢包
	inactive, err := pcap.NewInactiveHandle(e.iface)
	if err != nil {
		return fmt.Errorf("pcap.NewInactiveHandle(%s): %w", e.iface, err)
	}
	defer inactive.CleanUp()
	// 内核缓冲区调大到 32MB（默认仅 ~2MB）
	if err := inactive.SetBufferSize(32 * 1024 * 1024); err != nil {
		slog.Warn("capture: SetBufferSize 失败（忽略，使用默认值）", "error", err)
	}
	if err := inactive.SetSnapLen(16384); err != nil {
		slog.Warn("capture: SetSnapLen 失败（忽略，使用默认值）", "error", err)
	}
	if err := inactive.SetPromisc(true); err != nil {
		slog.Warn("capture: SetPromisc 失败（忽略，使用默认值）", "error", err)
	}
	if err := inactive.SetTimeout(pcap.BlockForever); err != nil {
		slog.Warn("capture: SetTimeout 失败（忽略，使用默认值）", "error", err)
	}
	h, err := inactive.Activate()
	if err != nil {
		return fmt.Errorf("pcap.Activate(%s): %w", e.iface, err)
	}
	e.handle = h

	// 应用 BPF 过滤：优先复用缓存指令，未命中则编译并缓存
	cacheKey := fmt.Sprintf("%s|%d|%d|%s", e.iface, h.LinkType(), 16384, e.bpf)
	if cached, ok := bpfCache.Load(cacheKey); ok {
		if err := h.SetBPFInstructionFilter(cached.([]pcap.BPFInstruction)); err != nil {
			slog.Warn("capture: 缓存 BPF 应用失败，回退到重新编译", "error", err)
			if err := h.SetBPFFilter(e.bpf); err != nil {
				e.handle.Close()
				e.handle = nil
				return fmt.Errorf("SetBPFFilter(%s): %w", e.bpf, err)
			}
		}
	} else {
		insns, ierr := h.CompileBPFFilter(e.bpf)
		if ierr != nil {
			e.handle.Close()
			e.handle = nil
			return fmt.Errorf("CompileBPFFilter(%s): %w", e.bpf, ierr)
		}
		if err := h.SetBPFInstructionFilter(insns); err != nil {
			e.handle.Close()
			e.handle = nil
			return fmt.Errorf("SetBPFInstructionFilter(%s): %w", e.bpf, err)
		}
		bpfCache.Store(cacheKey, insns)
	}

	slog.Info("capture: 开始抓包", "iface", e.iface, "bpf", e.bpf)

	e.workers = 4
	e.streamPool = NewTCPStreamPool(e)
	e.workerChs = make([]chan gopacket.Packet, e.workers)
	for i := range e.workerChs {
		e.workerChs[i] = make(chan gopacket.Packet, 256)
	}

	e.running.Store(true)

	e.ringBuf = NewMemRingBuffer(262144)
	e.writer = NewAsyncWriterPool(e.store, e.ringBuf, 4, 30*time.Millisecond)
	e.writer.engine = e
	e.writer.Start()

	for i := 0; i < e.workers; i++ {
		e.workerSg.Add(1)
		go e.workerLoop(i)
	}
	go e.packetLoop()
	go e.flushLoop()
	return nil
}

func (e *Engine) packetLoop() {
	packetSource := gopacket.NewPacketSource(e.handle, e.handle.LinkType())
	lastReport := time.Now()
	for packet := range packetSource.Packets() {
		if !e.running.Load() {
			return
		}
		e.stats.PacketsRecv.Add(1)
		workerIdx := e.flowHash(packet)
		ch := e.workerChs[workerIdx]
		select {
		case ch <- packet:
		default:
			e.stats.PacketsDrop.Add(1)
		}
		if now := time.Now(); now.Sub(lastReport) >= 5*time.Second {
			slog.Debug("capture: 数据包统计", "packets", e.stats.PacketsRecv.Load(), "http", e.stats.HTTPFound.Load(), "pkt_drop", e.stats.PacketsDrop.Load(), "stream_drop", e.stats.StreamsDrop.Load())
			lastReport = now
		}
	}
	for _, ch := range e.workerChs {
		close(ch)
	}
	e.workerSg.Wait()
}

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

func DetectInterface() string {
	devs, err := pcap.FindAllDevs()
	if err != nil {
		return "en0"
	}
	preferred := []string{"en0", "en1", "eth0", "wlp2s0", "wlan0"}
	for _, name := range preferred {
		for _, d := range devs {
			if d.Name == name && len(d.Addresses) > 0 {
				slog.Info("capture: 自动检测网卡", "iface", name)
				return name
			}
		}
	}
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
