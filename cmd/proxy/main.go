package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"packetlab/internal/api"
	"packetlab/internal/capture"
	"packetlab/internal/config"
	"packetlab/internal/llm"
	"packetlab/internal/models"
	"packetlab/internal/proxy"
	"packetlab/internal/store"
)

// version 在 release 构建时通过 -ldflags "-X main.version=..." 注入。
var version = "dev"

//go:embed web/*
var webFS embed.FS

// stringListFlag 支持重复指定的字符串 flag（如 --llm-endpoint 可指定多次）。
type stringListFlag []string

func (s *stringListFlag) String() string { return strings.Join(*s, ",") }
func (s *stringListFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	// 命令行参数
	proxyPort := flag.Int("proxy-port", config.DefaultProxyPort, "代理监听端口")
	apiPort := flag.Int("api-port", config.DefaultAPIPort, "API 服务端口")
	apiHost := flag.String("api-host", "127.0.0.1", "API/Web 监听地址 (默认 127.0.0.1，仅本机；局域网访问请显式指定，如 0.0.0.0)")
	apiToken := flag.String("api-token", os.Getenv("PACKETLAB_API_TOKEN"), "API/Web 鉴权 token（设置后所有 /api 与 /ws 请求需携带 Bearer token；可通过环境变量 PACKETLAB_API_TOKEN 设置）")
	dbPath := flag.String("db", "", "SQLite 数据库路径 (默认 ~/.packetlab/data.db)")
	noProxy := flag.Bool("no-proxy", false, "仅启动 API，不启动代理")
	noMitm := flag.Bool("no-mitm", false, "禁用 HTTPS MITM 解密")
	insecure := flag.Bool("insecure", false, "跳过 TLS 证书验证（MITM 上游与重发请求；仅开发环境）")

	captureFlag := flag.Bool("capture", false, "启用网卡抓包")
	captureIFace := flag.String("capture-iface", "", "指定抓包网卡（默认自动检测）")
	captureBPF := flag.String("capture-bpf", "tcp", "BPF 过滤器 (默认捕获所有 TCP)")
	captureNoProc := flag.Bool("capture-no-proc", false, "禁用进程关联")
	captureStreamTimeout := flag.Int("capture-stream-timeout", config.DefaultStreamTimeoutMin, "网卡抓包流空闲超时（分钟，0=默认2）")
	captureRingEntries := flag.Int("capture-ring-entries", config.DefaultCaptureRingEntries, "网卡抓包环形缓冲区条目数（向上取 2 的幂，0=默认262144）")
	captureMaxStreams := flag.Int("capture-max-streams", config.DefaultMaxStreams, "max concurrent TCP streams for NIC capture (LRU eviction when exceeded, min 64)")
	interceptPendingTimeout := flag.Duration("intercept-pending-timeout", config.DefaultInterceptPendingTimeout,
		"interceptor pending request timeout (Go duration, e.g. 30s, 2m; range 1s~10m)")
	cleanupRetentionDays := flag.Int("cleanup-retention-days", config.DefaultCleanupRetentionDays,
		"auto cleanup: delete requests/logs older than N days (0=disable auto cleanup)")
	cleanupInterval := flag.Duration("cleanup-interval", config.DefaultCleanupInterval,
		"auto cleanup interval (Go duration, e.g. 6h, 30m; min 1m)")
	maxReqBodyKB := flag.Int("max-req-body-kb", config.DefaultMaxReqBodyKB, "请求体最大 KB (0=使用默认值2048)")
	maxResBodyKB := flag.Int("max-res-body-kb", config.DefaultMaxResBodyKB, "响应体最大 KB (0=使用默认值4096)")
	apiAllowOrigins := flag.String("api-allow-origins", "", "逗号分隔的 CORS/WebSocket 允许 Origin 列表（默认仅 localhost）")
	// 可重复指定的自定义 OpenAI 兼容 LLM 端点（host[=显示名]），如 --llm-endpoint api.deepseek.com=DeepSeek
	var llmEndpoints stringListFlag
	flag.Var(&llmEndpoints, "llm-endpoint", "自定义 OpenAI 兼容 LLM 端点（可重复指定，格式 host[=显示名]，如 api.deepseek.com=DeepSeek）")
	flag.Parse()

	// 结构化日志
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// 集中化配置 fail-fast 校验
	cfg, err := config.Load(*proxyPort, *apiPort, *dbPath, *noProxy, *noMitm, *insecure,
		*apiHost, *apiToken,
		*captureFlag, *captureIFace, *captureBPF, *captureNoProc,
		*captureStreamTimeout, *maxReqBodyKB, *maxResBodyKB, *captureRingEntries, *captureMaxStreams,
		*interceptPendingTimeout,
		*cleanupRetentionDays, *cleanupInterval,
		[]string(llmEndpoints))
	if err != nil {
		slog.Error("配置加载失败", "error", err)
		os.Exit(1)
	}
	// 注册自定义 OpenAI 兼容 LLM 端点（LLM 识别使用）
	for _, spec := range cfg.LLMEndpoints {
		host, name := spec, ""
		if i := strings.Index(spec, "="); i > 0 {
			host, name = spec[:i], spec[i+1:]
		}
		host, name = strings.TrimSpace(host), strings.TrimSpace(name)
		if host == "" {
			slog.Warn("忽略无效 --llm-endpoint 配置", "spec", spec)
			continue
		}
		llm.RegisterCustomEndpoint(llm.CustomEndpoint{Host: host, Name: name})
		slog.Info("已注册自定义 LLM 端点", "host", host, "name", name)
	}
	// 解析 --api-allow-origins 为白名单（空 = 仅 localhost/回环）
	if *apiAllowOrigins != "" {
		var allowList []string
		for _, o := range strings.Split(*apiAllowOrigins, ",") {
			o = strings.TrimSpace(o)
			if o != "" {
				allowList = append(allowList, o)
			}
		}
		cfg.AllowOrigins = allowList
	}
	slog.Info("配置已加载", "proxy_port", cfg.ProxyPort, "api_port", cfg.APIPort,
		"api_host", cfg.APIHost, "api_auth", cfg.APIToken != "",
		"db", cfg.DBPath, "no_proxy", cfg.NoProxy, "no_mitm", cfg.NoMitm, "insecure", cfg.Insecure,
		"max_req_body_kb", cfg.MaxReqBodyKB, "max_res_body_kb", cfg.MaxResBodyKB)
	if cfg.APIToken != "" {
		tokenFile := filepath.Join(cfg.BaseDir, "token")
		slog.Info("API 鉴权已启用：Web 界面首次访问会要求输入 token（从本日志或 "+tokenFile+" 获取）",
			"hint", "cat "+tokenFile)
	}

	// 初始化存储
	st, err := store.New(cfg.DBPath)
	if err != nil {
		slog.Error("初始化数据库失败", "error", err)
		os.Exit(1)
	}
	defer st.Close()
	slog.Info("数据库已初始化", "path", cfg.DBPath)

	// 加载前端
	frontendHandler := loadFrontend()

	// 创建 API 服务器
	apiSrv := api.New(st, frontendHandler, cfg.Insecure, cfg.APIToken, cfg.AllowOrigins)

	// 创建拦截控制器
	interceptMode, _ := st.GetSetting("intercept_mode")
	if interceptMode == "" {
		interceptMode = "auto"
	}
	interceptor := proxy.NewInterceptor(cfg.InterceptPendingTimeout, func(req *models.PendingRequest) {
		apiSrv.BroadcastIntercept(req)
	}, st)
	interceptor.SetMode(interceptMode)
	if rules, err := st.ListRules(); err == nil {
		interceptor.SetRules(rules)
	} else {
		slog.Warn("加载拦截规则失败", "error", err)
	}
	apiSrv.SetInterceptor(interceptor)

	// 网卡抓包引擎
	var capEngine *capture.Engine
	if cfg.Capture {
		iface := cfg.CaptureIface
		if iface == "" {
			iface = capture.DetectInterface()
		}
		capEngine = capture.New(iface, cfg.CaptureBPF, st, apiSrv)
		capEngine.SetStreamTimeout(time.Duration(cfg.CaptureStreamTimeoutMin) * time.Minute)
		capEngine.SetMaxResBytes(int64(cfg.MaxResBodyKB) * 1024)
		capEngine.SetRingBufSize(cfg.CaptureRingEntries)
		capEngine.SetMaxStreams(cfg.MaxStreams)
		if err := capEngine.Start(); err != nil {
			slog.Warn("抓包引擎启动失败（可能需要 sudo 权限）",
				"iface", iface, "error", err,
				"hint", "如需抓包请用 sudo 启动: sudo ./packetlab --capture")
			capEngine = nil
		} else {
			apiSrv.SetCaptureEngine(capEngine)
			slog.Info("网卡抓包已启动", "iface", iface, "bpf", cfg.CaptureBPF)
		}
	}

	// 捕获回调
	onCapture := func(req *models.CapturedRequest) {
		if req.IsSSE && req.ID > 0 {
			apiSrv.BroadcastUpdate(req)
		} else {
			apiSrv.BroadcastCapture(req)
		}
	}

	// 加载/生成 CA 证书（HTTPS MITM）
	var caCert, caKey []byte
	if !cfg.NoMitm {
		caCert, caKey, err = proxy.LoadOrGenerateCA(cfg.CertDir)
		if err != nil {
			slog.Warn("CA 证书加载失败，HTTPS MITM 已禁用", "error", err)
		} else {
			slog.Info("HTTPS MITM 已启用", "cert_dir", cfg.CertDir)
		}
	} else {
		slog.Info("HTTPS MITM 已禁用 (--no-mitm)")
	}

	// 启动代理
	var proxySrv *proxy.Server
	if !cfg.NoProxy {
		proxySrv = proxy.New(cfg.ProxyPort, st, caCert, caKey, onCapture, interceptor, cfg.MaxReqBodyKB, cfg.MaxResBodyKB, cfg.Insecure)
	}
	// 代理状态上报（--no-proxy 或未创建时为 false）
	apiSrv.SetProxyRunning(proxySrv != nil)

	// 错误收集 channel：proxy/API 启动失败时通知主 goroutine 触发关闭
	errCh := make(chan error, 2)

	if !cfg.NoProxy {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("goroutine panic", "component", "proxy", "r", r, "stack", string(debug.Stack()))
				}
			}()
			slog.Info("代理服务器启动", "port", cfg.ProxyPort)
			if err := proxySrv.Start(); err != nil && err != http.ErrServerClosed {
				slog.Error("代理服务器启动失败", "error", err)
				errCh <- err
			}
		}()
	} else {
		slog.Info("代理已禁用 (--no-proxy)")
	}

	// API HTTP 服务器
	apiHTTPServer := &http.Server{
		Addr:         cfg.APIAddr(),
		Handler:      apiSrv.Handler(),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// 后台定期清理：每 N 小时执行一次，retention_days 由 settings 表控制（<=0 表示禁用）
	cleanupStop := startAutoCleanup(st, cfg.CleanupRetentionDays, cfg.CleanupInterval)

	// 优雅关闭
	shutdownCh := make(chan os.Signal, 1)
	signal.Notify(shutdownCh, syscall.SIGINT, syscall.SIGTERM)

	mitmStatus := "禁用"
	if caCert != nil {
		mitmStatus = "已启用"
	}

	slog.Info("PacketLab "+version+" 已就绪",
		"web_url", fmt.Sprintf("http://localhost:%d", cfg.APIPort),
		"proxy_port", cfg.ProxyPort,
		"mitm", mitmStatus)

	// 在 goroutine 中启动 API，主 goroutine 等待信号
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("goroutine panic", "component", "api", "r", r, "stack", string(debug.Stack()))
			}
		}()
		slog.Info("API 服务器启动", "addr", cfg.APIAddr())
		if err := apiHTTPServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("API 服务器错误", "error", err)
			errCh <- err
		}
	}()

	// 等待关闭信号或服务器启动失败
	select {
	case sig := <-shutdownCh:
		slog.Info("收到关闭信号，开始优雅退出...", "signal", sig.String())
	case err := <-errCh:
		slog.Error("服务器启动失败，开始关闭", "error", err)
	}

	// 优雅关闭顺序（依赖反向）：
	// 1) API/Web 服务器最先停：不再接受新请求（/api 与 WS），等待在途请求完成（30s 上限）。
	//    若先停抓包引擎，API 排空期间引擎已死，UI 与 /api 还在服务却收不到新捕获数据。
	// 2) 代理其次停：在途请求经 OnResponse → batchWriter.Enqueue 完成入队后
	//    batchWriter.Stop() 排空（详见 proxy.Stop 注释）。
	// 3) 抓包引擎再停：清洗 pendingReq、排空 ring 异步 writer。
	// 4) 拦截器最后刷盘：logCh 在所有 Handle 退出后才关闭。
	// Graceful shutdown order (reverse dependency): API server first (stops
	// accepting new /api + WS traffic), then proxy (drains in-flight requests),
	// then capture engine (flushes pending streams + async writer), then
	// interceptor (flushes logCh).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := apiHTTPServer.Shutdown(ctx); err != nil {
		slog.Error("API 服务器关闭失败", "error", err)
	}

	// 关闭代理（先停代理，等在途请求完成，避免 Stop 后 writeLog panic）
	if proxySrv != nil {
		proxySrv.Stop()
	}

	// 停止抓包引擎（清洗 pendingReq）
	if capEngine != nil {
		capEngine.Stop()
	}

	// 关闭拦截器：logCh 在所有 Handle 退出后才关闭，刷盘拦截日志
	if interceptor != nil {
		interceptor.Stop()
	}

	// 关闭 API 服务器资源（rateLimiter goroutine、wsHub）
	apiSrv.Stop()

	// 停止后台清理
	cleanupStop()

	// 数据库由 defer st.Close() 关闭（line 91），此处不再重复关闭以避免 double close

	slog.Info("PacketLab 已安全退出")
}

func loadFrontend() http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		slog.Error("嵌入前端加载失败", "error", err)
		return http.NotFoundHandler()
	}
	return http.FileServer(http.FS(sub))
}

// startAutoCleanup 启动后台定期清理 goroutine，返回停止函数。
// retentionDays 首次启动时若 settings 表无 'retention_days' 则写入；若已有则尊重已存在的 settings 值。
// cleanupInterval 决定执行间隔（必须 >= 1m）。
// retentionDays <= 0 时视为禁用：仅写入 settings 后返回 no-op stop 函数，不启动 goroutine。
// startAutoCleanup starts the background cleanup goroutine, returns a stop function.
// On first start, writes retentionDays to settings if 'retention_days' is unset;
// if already set, the existing setting is respected.
// When retentionDays <= 0, auto cleanup is disabled: only settings are written and a no-op
// stop function is returned (no goroutine is started).
func startAutoCleanup(st *store.Store, retentionDays int, cleanupInterval time.Duration) func() {
	// 首次启动写入 settings（如未设置）；已存在则尊重 settings 中的值
	// Write default to settings on first start (if unset); respect existing value otherwise
	if existing, err := st.GetSetting("retention_days"); err == nil && existing == "" {
		_ = st.SetSetting("retention_days", strconv.Itoa(retentionDays))
	}
	// 0=禁用自动清理：不启动 goroutine
	// 0 disables auto cleanup: do not start the goroutine
	if retentionDays <= 0 {
		return func() {}
	}

	ticker := time.NewTicker(cleanupInterval)
	stopCh := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("goroutine panic", "component", "cleanup", "r", r, "stack", string(debug.Stack()))
			}
		}()
		for {
			select {
			case <-ticker.C:
				// retention_days<=0 时 Cleanup 内部直接返回，零成本
				// When retention_days<=0, Cleanup is a no-op
				dr, dl, days, err := st.Cleanup(0)
				if err != nil {
					slog.Warn("auto cleanup failed", "error", err)
					continue
				}
				if days > 0 && (dr > 0 || dl > 0) {
					slog.Info("auto cleanup", "deleted_requests", dr, "deleted_logs", dl, "retention_days", days)
				}
			case <-stopCh:
				ticker.Stop()
				return
			}
		}
	}()
	return func() {
		close(stopCh)
		wg.Wait()
	}
}
