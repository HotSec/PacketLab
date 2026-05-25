//go:build (linux || darwin) && cgo

package capture

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/pcap"
)

func (e *Engine) Start() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.running.Load() {
		return fmt.Errorf("capture already running")
	}

	e.stopCh = make(chan struct{})

	var err error
	h, err := pcap.OpenLive(e.iface, 65536, true, pcap.BlockForever)
	if err != nil {
		return fmt.Errorf("pcap.OpenLive(%s): %w", e.iface, err)
	}
	e.handle = h

	if err := e.handle.SetBPFFilter(e.bpf); err != nil {
		e.handle.Close()
		e.handle = nil
		return fmt.Errorf("SetBPFFilter(%s): %w", e.bpf, err)
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
