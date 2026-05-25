//go:build !((linux || darwin) && cgo)

package capture

import (
	"fmt"
	"log/slog"
)

func (e *Engine) Start() error {
	return fmt.Errorf("网卡抓包需要 CGO 和 libpcap，请使用 CGO_ENABLED=1 编译并安装 libpcap-dev")
}

func (e *Engine) packetLoop() {}

func ListInterfaces() ([]string, error) {
	return nil, fmt.Errorf("网卡抓包需要 CGO 和 libpcap")
}

func DetectInterface() string {
	slog.Warn("capture: pcap 不可用，返回默认网卡 en0")
	return "en0"
}
