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
	"syscall"
	"time"

	"packetlab/internal/api"
	"packetlab/internal/capture"
	"packetlab/internal/config"
	"packetlab/internal/models"
	"packetlab/internal/proxy"
	"packetlab/internal/store"
)

//go:embed web/*
var webFS embed.FS

func main() {
	// 命令行参数
	proxyPort := flag.Int("proxy-port", config.DefaultProxyPort, "代理监听端口")
	apiPort := flag.Int("api-port", config.DefaultAPIPort, "API 服务端口")
	dbPath := flag.String("db", "", "SQLite 数据库路径 (默认 ~/.packetlab/data.db)")
	noProxy := flag.Bool("no-proxy", false, "仅启动 API，不启动代理")
	noMitm := flag.Bool("no-mitm", false, "禁用 HTTPS MITM 解密")
	insecure := flag.Bool("insecure", false, "重发请求时跳过 TLS 证书验证（仅开发环境）")

	captureFlag   := flag.Bool("capture", false, "启用网卡抓包")
	captureIFace  := flag.String("capture-iface", "", "指定抓包网卡（默认自动检测）")
	captureBPF    := flag.String("capture-bpf", "tcp", "BPF 过滤器 (默认捕获所有 TCP)")
	captureNoProc := flag.Bool("capture-no-proc", false, "禁用进程关联")
	maxReqBodyKB  := flag.Int("max-req-body-kb", config.DefaultMaxReqBodyKB, "请求体最大 KB (0=使用默认值32)")
	maxResBodyKB  := flag.Int("max-res-body-kb", config.DefaultMaxResBodyKB, "响应体最大 KB (0=使用默认值64)")
	flag.Parse()

	// 结构化日志
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// 集中化配置 fail-fast 校验
	cfg, err := config.Load(*proxyPort, *apiPort, *dbPath, *noProxy, *noMitm, *insecure,
		*captureFlag, *captureIFace, *captureBPF, *captureNoProc, *maxReqBodyKB, *maxResBodyKB)
	if err != nil {
		slog.Error("配置加载失败", "error", err)
		os.Exit(1)
	}
	slog.Info("配置已加载", "proxy_port", cfg.ProxyPort, "api_port", cfg.APIPort,
		"db", cfg.DBPath, "no_proxy", cfg.NoProxy, "no_mitm", cfg.NoMitm, "insecure", cfg.Insecure,
		"max_req_body_kb", cfg.MaxReqBodyKB, "max_res_body_kb", cfg.MaxResBodyKB)

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
	apiSrv := api.New(st, frontendHandler, cfg.Insecure)

	// 创建拦截控制器
	interceptMode, _ := st.GetSetting("intercept_mode")
	if interceptMode == "" {
		interceptMode = "auto"
	}
	interceptor := proxy.NewInterceptor(15, func(req *models.PendingRequest) {
		apiSrv.BroadcastIntercept(req)
	}, st)
	interceptor.SetMode(interceptMode)
	if rules, err := st.ListRules(); err == nil {
		interceptor.SetRules(rules)
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
		proxySrv = proxy.New(cfg.ProxyPort, st, caCert, caKey, onCapture, interceptor, cfg.MaxReqBodyKB, cfg.MaxResBodyKB)
		go func() {
			slog.Info("代理服务器启动", "port", cfg.ProxyPort)
			if err := proxySrv.Start(); err != nil {
				slog.Error("代理服务器启动失败", "error", err)
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

	// 优雅关闭
	shutdownCh := make(chan os.Signal, 1)
	signal.Notify(shutdownCh, syscall.SIGINT, syscall.SIGTERM)

	mitmStatus := "禁用"
	if caCert != nil {
		mitmStatus = "已启用"
	}

	slog.Info("PacketLab v2.0 已就绪",
		"web_url", fmt.Sprintf("http://localhost:%d", cfg.APIPort),
		"proxy_port", cfg.ProxyPort,
		"mitm", mitmStatus)

	// 在 goroutine 中启动 API，主 goroutine 等待信号
	go func() {
		slog.Info("API 服务器启动", "addr", cfg.APIAddr())
		if err := apiHTTPServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("API 服务器错误", "error", err)
		}
	}()

	// 等待关闭信号
	sig := <-shutdownCh
	slog.Info("收到关闭信号，开始优雅退出...", "signal", sig.String())

	// 停止抓包引擎（清洗 pendingReq）
	if capEngine != nil {
		capEngine.Stop()
	}

	// 优雅关闭 API 服务器（30s 超时）
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := apiHTTPServer.Shutdown(ctx); err != nil {
		slog.Error("API 服务器关闭失败", "error", err)
	}

	// 关闭代理
	if proxySrv != nil {
		proxySrv.Stop()
	}

	// 关闭 API 服务器资源（rateLimiter goroutine、wsHub）
	apiSrv.Stop()

	// 关闭数据库
	if err := st.Close(); err != nil {
		slog.Error("数据库关闭失败", "error", err)
	}

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
