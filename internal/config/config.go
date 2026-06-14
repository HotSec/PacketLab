package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// Config 集中化应用配置 (fail-fast on invalid values)
type Config struct {
	ProxyPort      int
	APIPort        int
	DBPath         string
	NoProxy        bool
	NoMitm         bool
	Insecure       bool // 是否跳过 TLS 证书校验（仅开发环境）
	BaseDir        string
	CertDir        string
	Capture        bool
	CaptureIface   string
	CaptureBPF     string
	CaptureNoProc  bool
	CaptureStreamTimeoutMin int // 网卡抓包：流空闲超时（分钟），0=使用默认值
	MaxReqBodyKB   int // 请求体最大 KB，0=使用默认值
	MaxResBodyKB   int // 响应体最大 KB，0=使用默认值
}

// Default validated default values
const (
	DefaultProxyPort    = 8080
	DefaultAPIPort      = 9090
	DefaultTimeoutSec   = 30
	DefaultMaxReqBodyKB = 2048  // 2MB
	DefaultMaxResBodyKB = 4096  // 4MB
	DefaultStreamTimeoutMin = 2 // 网卡抓包流空闲超时默认 2 分钟
	defaultOrg          = "PacketLab"
)

// Load 从命令行参数和环境变量加载配置，fail-fast 校验
func Load(proxyPort, apiPort int, dbPath string, noProxy, noMitm, insecure bool,
	capture bool, captureIface, captureBPF string, captureNoProc bool,
	streamTimeoutMin, maxReqBodyKB, maxResBodyKB int) (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("config: cannot determine home directory: %w", err)
	}
	baseDir := filepath.Join(home, ".packetlab")

	if dbPath == "" {
		dbPath = filepath.Join(baseDir, "data.db")
	}
	if proxyPort <= 0 || proxyPort > 65535 {
		return nil, fmt.Errorf("config: invalid proxy-port %d (must be 1-65535)", proxyPort)
	}
	if apiPort <= 0 || apiPort > 65535 {
		return nil, fmt.Errorf("config: invalid api-port %d (must be 1-65535)", apiPort)
	}
	if proxyPort == apiPort {
		return nil, fmt.Errorf("config: proxy-port and api-port must differ (both set to %d)", proxyPort)
	}

	// Ensure DB directory exists
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, fmt.Errorf("config: cannot create DB directory %s: %w", filepath.Dir(dbPath), err)
	}

	if maxReqBodyKB < 0 {
		return nil, fmt.Errorf("config: max-req-body-kb must be >= 0, got %d", maxReqBodyKB)
	}
	if maxResBodyKB < 0 {
		return nil, fmt.Errorf("config: max-res-body-kb must be >= 0, got %d", maxResBodyKB)
	}
	if maxReqBodyKB == 0 {
		maxReqBodyKB = DefaultMaxReqBodyKB
	}
	if maxResBodyKB == 0 {
		maxResBodyKB = DefaultMaxResBodyKB
	}
	if streamTimeoutMin < 0 {
		return nil, fmt.Errorf("config: capture-stream-timeout must be >= 0, got %d", streamTimeoutMin)
	}
	if streamTimeoutMin == 0 {
		streamTimeoutMin = DefaultStreamTimeoutMin
	}

	cfg := &Config{
		ProxyPort:              proxyPort,
		APIPort:                apiPort,
		DBPath:                 dbPath,
		NoProxy:                noProxy,
		NoMitm:                 noMitm,
		Insecure:               insecure,
		BaseDir:                baseDir,
		CertDir:                filepath.Join(baseDir, "certs"),
		Capture:                capture,
		CaptureIface:           captureIface,
		CaptureBPF:             captureBPF,
		CaptureNoProc:          captureNoProc,
		CaptureStreamTimeoutMin: streamTimeoutMin,
		MaxReqBodyKB:           maxReqBodyKB,
		MaxResBodyKB:           maxResBodyKB,
	}
	return cfg, nil
}

// Addr formats a port into a listen address string
func (c *Config) ProxyAddr() string  { return fmt.Sprintf(":%d", c.ProxyPort) }
func (c *Config) APIAddr() string    { return fmt.Sprintf(":%d", c.APIPort) }
func (c *Config) OrgName() string    { return defaultOrg }
