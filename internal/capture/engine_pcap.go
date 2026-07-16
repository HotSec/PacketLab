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

// openHandle 创建并配置 InactiveHandle，以指定 promisc 模式激活返回 *pcap.Handle。
// InactiveHandle 不可重复激活，每次尝试需新建。
func openHandle(iface string, promisc bool) (*pcap.Handle, error) {
	inactive, err := pcap.NewInactiveHandle(iface)
	if err != nil {
		return nil, err
	}
	defer inactive.CleanUp()
	// 内核缓冲区调大到 32MB（默认仅 ~2MB），降低高吞吐丢包
	if err := inactive.SetBufferSize(32 * 1024 * 1024); err != nil {
		slog.Warn("capture: SetBufferSize 失败（忽略，使用默认值）", "error", err)
	}
	if err := inactive.SetSnapLen(16384); err != nil {
		slog.Warn("capture: SetSnapLen 失败（忽略，使用默认值）", "error", err)
	}
	if err := inactive.SetPromisc(promisc); err != nil {
		slog.Warn("capture: SetPromisc 失败（忽略，使用默认值）", "error", err)
	}
	if err := inactive.SetTimeout(pcap.BlockForever); err != nil {
		slog.Warn("capture: SetTimeout 失败（忽略，使用默认值）", "error", err)
	}
	return inactive.Activate()
}

func (e *Engine) Start() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.running.Load() {
		return fmt.Errorf("capture already running")
	}

	e.stopCh = make(chan struct{})

	// 激活：优先尝试 promisc（混杂）模式；若失败（如非 root 用户、macOS 无权限），
	// 回退到非 promisc 模式（对本机流量捕获完全足够）。
	h, err := openHandle(e.iface, true)
	if err != nil {
		h, err = openHandle(e.iface, false)
		if err != nil {
			return fmt.Errorf("pcap.Activate(%s): %w", e.iface, err)
		}
		slog.Info("capture: promisc 模式不可用，使用非混杂模式（仅捕获本机流量）", "iface", e.iface)
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

	e.ringBuf = NewMemRingBuffer(e.ringBufSize)
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
	// 快照 workerChs，避免与 Stop 的潜在并发修改 race
	// Snapshot workerChs to avoid race with any concurrent mutation by Stop
	workerChs := e.workerChs
	// defer 保证：无论 packetLoop 如何退出（range 结束 / running=false 早退 / panic），
	// 都会关闭 worker channels 并等待 workers 退出，避免 goroutine 泄漏
	// defer ensures worker channels are always closed and workers awaited,
	// regardless of how packetLoop exits (range end / running=false early return / panic),
	// preventing goroutine leaks.
	defer func() {
		for _, ch := range workerChs {
			close(ch)
		}
		e.workerSg.Wait()
	}()

	packetSource := gopacket.NewPacketSource(e.handle, e.handle.LinkType())
	lastReport := time.Now()
	for packet := range packetSource.Packets() {
		if !e.running.Load() {
			return // defer 会执行清理 / defer will run cleanup
		}
		e.stats.PacketsRecv.Add(1)
		workerIdx := e.flowHash(packet)
		// 越界守卫：防御 workerChs 长度与 workers 不一致的边界情况
		// Bounds guard: defends against workerChs length mismatch with workers
		if workerIdx < 0 || workerIdx >= len(workerChs) {
			continue
		}
		ch := workerChs[workerIdx]
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
	// range 正常结束（handle.Close() 导致 channel 关闭）时，defer 也会执行清理
	// When range ends normally (handle.Close() closes the channel), defer handles cleanup
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
