package main

import (
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"packetlab/internal/api"
	"packetlab/internal/models"
	"packetlab/internal/proxy"
	"packetlab/internal/store"
)

//go:embed web/*
var webFS embed.FS

func main() {
	proxyPort := flag.Int("proxy-port", 8080, "代理监听端口")
	apiPort := flag.Int("api-port", 9090, "API 服务端口")
	dbPath := flag.String("db", "", "SQLite 数据库路径 (默认 ~/.packetlab/data.db)")
	noProxy := flag.Bool("no-proxy", false, "仅启动 API，不启动代理")
	noMitm := flag.Bool("no-mitm", false, "禁用 HTTPS MITM 解密")
	flag.Parse()

	baseDir := filepath.Join(userHome(), ".packetlab")

	// 数据库路径
	if *dbPath == "" {
		*dbPath = filepath.Join(baseDir, "data.db")
	}
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0755); err != nil {
		log.Fatalf("创建数据库目录失败: %v", err)
	}

	// 初始化存储
	st, err := store.New(*dbPath)
	if err != nil {
		log.Fatalf("初始化数据库失败: %v", err)
	}
	defer st.Close()
	log.Printf("[store] 数据库: %s", *dbPath)

	// 加载前端
	frontendHandler := loadFrontend()

	// 创建 API 服务器
	apiSrv := api.New(st, frontendHandler)

	// 创建拦截控制器
	interceptMode, _ := st.GetSetting("intercept_mode")
	if interceptMode == "" {
		interceptMode = "auto"
	}
	interceptor := proxy.NewInterceptor(15, func(req *models.PendingRequest) {
		apiSrv.BroadcastIntercept(req)
	})
	interceptor.SetMode(interceptMode)
	// 加载已有规则
	if rules, err := st.ListRules(); err == nil {
		interceptor.SetRules(rules)
	}
	// 暴露给 API
	apiSrv.SetInterceptor(interceptor)

	// 代理回调：捕获到新请求时通过 API WebSocket 广播
	onCapture := func(req *models.CapturedRequest) {
		apiSrv.BroadcastCapture(req)
	}

	// 加载/生成 CA 证书（HTTPS MITM）
	var caCert, caKey []byte
	if !*noMitm {
		certDir := filepath.Join(baseDir, "certs")
		caCert, caKey, err = proxy.LoadOrGenerateCA(certDir)
		if err != nil {
			log.Printf("[mitm] CA 证书加载失败: %v，HTTPS MITM 已禁用", err)
		} else {
			log.Printf("[mitm] HTTPS MITM 已启用")
			log.Printf("[mitm] 安装 CA 证书以解密 HTTPS 流量: %s/ca.crt", certDir)
		}
	} else {
		log.Println("[mitm] HTTPS MITM 已禁用 (--no-mitm)")
	}

	// 启动代理
	if !*noProxy {
		proxySrv := proxy.New(*proxyPort, st, caCert, caKey, onCapture, interceptor)

		go func() {
			log.Printf("[proxy] 启动代理: :%d", *proxyPort)
			if err := proxySrv.Start(); err != nil {
				log.Printf("[proxy] 代理启动失败: %v", err)
			}
		}()
	} else {
		log.Println("[proxy] 代理已禁用 (--no-proxy)")
	}

	// 优雅关闭
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		log.Printf("\n[server] 收到信号 %v，正在关闭...", sig)
		st.Close()
		os.Exit(0)
	}()

	mitmStatus := "禁用"
	if caCert != nil {
		mitmStatus = "已启用"
	}

	log.Printf(`
╔══════════════════════════════════════════════════════╗
║            PacketLab — 流量捕获工具 v2.0             ║
╠══════════════════════════════════════════════════════╣
║  Web 界面:    http://localhost:%-5d                ║
║  代理端口:     :%-5d                             ║
║  HTTPS MITM:  %-38s ║
║  配置浏览器代理为 localhost:%d                   ║
╚══════════════════════════════════════════════════════╝
`, *apiPort, *proxyPort, mitmStatus, *proxyPort)

	apiAddr := fmt.Sprintf(":%d", *apiPort)
	if err := http.ListenAndServe(apiAddr, apiSrv.Handler()); err != nil {
		log.Fatalf("[api] 启动失败: %v", err)
	}
}

func loadFrontend() http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Printf("[web] 嵌入前端加载失败: %v", err)
		return http.NotFoundHandler()
	}
	return http.FileServer(http.FS(sub))
}

func userHome() string {
	home, _ := os.UserHomeDir()
	return home
}
