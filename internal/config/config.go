package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
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
	CaptureRingEntries int // 网卡抓包：环形缓冲区条目数（向上取 2 的幂），0=使用默认值
	MaxStreams int // 网卡抓包：最大并发 TCP 流数（超限 LRU 淘汰），0=使用默认值，最小 64
	MaxReqBodyKB   int // 请求体最大 KB，0=使用默认值
	MaxResBodyKB   int // 响应体最大 KB，0=使用默认值
	AllowOrigins []string // CORS/WebSocket 允许的 Origin 白名单（空 = 仅 localhost）
	InterceptPendingTimeout time.Duration // 拦截器 pending 请求超时（默认 15s，范围 1s~10m）
	CleanupRetentionDays int           // 自动清理：保留 N 天的请求数据（0=禁用自动清理，默认 7）
	CleanupInterval      time.Duration // 自动清理：执行间隔（默认 6h）
}

// Default validated default values
const (
	DefaultProxyPort    = 8080
	DefaultAPIPort      = 9090
	DefaultTimeoutSec   = 30
	DefaultMaxReqBodyKB = 2048  // 2MB
	DefaultMaxResBodyKB = 4096  // 4MB
	DefaultStreamTimeoutMin = 2 // 网卡抓包流空闲超时默认 2 分钟
	DefaultCaptureRingEntries = 262144 // 网卡抓包环形缓冲区默认条目数（256K，向上取 2 的幂）
	DefaultMaxStreams = 1000 // 网卡抓包最大并发 TCP 流数默认值（超限 LRU 淘汰）
	DefaultInterceptPendingTimeout = 15 * time.Second // 拦截器 pending 请求默认超时
	DefaultCleanupRetentionDays = 7               // 自动清理默认保留 7 天
	DefaultCleanupInterval      = 6 * time.Hour    // 自动清理默认间隔 6 小时
	defaultOrg          = "PacketLab"
)

// Load 从命令行参数和环境变量加载配置，fail-fast 校验
func Load(proxyPort, apiPort int, dbPath string, noProxy, noMitm, insecure bool,
	capture bool, captureIface, captureBPF string, captureNoProc bool,
	streamTimeoutMin, maxReqBodyKB, maxResBodyKB, captureRingEntries, maxStreams int,
	interceptPendingTimeout time.Duration,
	cleanupRetentionDays int, cleanupInterval time.Duration) (*Config, error) {
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

	// Ensure base/cert/DB directories exist. --db 可能指向 BaseDir 外部，
	// 此时 BaseDir/CertDir 不会被 dbPath 的 MkdirAll 自动创建，需显式建。
	// Ensure base/cert/DB directories exist. --db may point outside BaseDir,
	// in which case BaseDir/CertDir won't be created by dbPath's MkdirAll.
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("config: cannot create base directory %s: %w", baseDir, err)
	}
	certDir := filepath.Join(baseDir, "certs")
	if err := os.MkdirAll(certDir, 0755); err != nil {
		return nil, fmt.Errorf("config: cannot create cert directory %s: %w", certDir, err)
	}
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
	if captureRingEntries < 0 {
		return nil, fmt.Errorf("config: capture-ring-entries must be >= 0, got %d", captureRingEntries)
	}
	if captureRingEntries == 0 {
		captureRingEntries = DefaultCaptureRingEntries
	}
	// maxStreams：负数非法；0 用默认值；正数但 < 64 非法（计划要求 >= 64）
	// maxStreams: negative invalid; 0 uses default; positive but < 64 invalid (plan requires >= 64)
	if maxStreams < 0 {
		return nil, fmt.Errorf("config: capture-max-streams must be >= 0, got %d", maxStreams)
	}
	if maxStreams == 0 {
		maxStreams = DefaultMaxStreams
	}
	if maxStreams > 0 && maxStreams < 64 {
		return nil, fmt.Errorf("config: capture-max-streams must be >= 64, got %d", maxStreams)
	}
	// interceptPendingTimeout: 0 用默认值；超出 1s~10m 范围非法
	// interceptPendingTimeout: 0 uses default; outside 1s~10m invalid
	if interceptPendingTimeout == 0 {
		interceptPendingTimeout = DefaultInterceptPendingTimeout
	}
	if interceptPendingTimeout < time.Second || interceptPendingTimeout > 10*time.Minute {
		return nil, fmt.Errorf("config: intercept-pending-timeout must be between 1s and 10m, got %v", interceptPendingTimeout)
	}
	// cleanupRetentionDays: 负数非法；0=禁用自动清理（保留 0，不替换为默认值）
	// cleanupRetentionDays: negative invalid; 0 disables auto cleanup (keep 0, do not substitute default)
	if cleanupRetentionDays < 0 {
		return nil, fmt.Errorf("config: cleanup-retention-days must be >= 0, got %d", cleanupRetentionDays)
	}
	// cleanupInterval: 0 用默认值；< 1m 非法（避免过于频繁清理）
	// cleanupInterval: 0 uses default; < 1m invalid (avoid overly frequent cleanup)
	if cleanupInterval == 0 {
		cleanupInterval = DefaultCleanupInterval
	}
	if cleanupInterval < time.Minute {
		return nil, fmt.Errorf("config: cleanup-interval must be >= 1m, got %v", cleanupInterval)
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
		CaptureRingEntries:     captureRingEntries,
		MaxStreams:             maxStreams,
		MaxReqBodyKB:           maxReqBodyKB,
		MaxResBodyKB:           maxResBodyKB,
		InterceptPendingTimeout: interceptPendingTimeout,
		CleanupRetentionDays:   cleanupRetentionDays,
		CleanupInterval:        cleanupInterval,
	}
	return cfg, nil
}

// Addr formats a port into a listen address string
func (c *Config) ProxyAddr() string  { return fmt.Sprintf(":%d", c.ProxyPort) }
func (c *Config) APIAddr() string    { return fmt.Sprintf(":%d", c.APIPort) }
func (c *Config) OrgName() string    { return defaultOrg }
