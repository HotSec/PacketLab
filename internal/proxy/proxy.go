package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"packetlab/internal/api"
	"packetlab/internal/models"
	"packetlab/internal/store"

	"github.com/elazarl/goproxy"
)

// OnCapture 新请求捕获回调
type OnCapture func(req *models.CapturedRequest)

// Server 代理服务器
type Server struct {
	proxy       *goproxy.ProxyHttpServer
	httpServer  *http.Server
	store       *store.Store
	batchWriter *BatchWriter
	interceptor *Interceptor
	onCapture   OnCapture
	mu          sync.RWMutex
	running     bool
	port        int
	mitmEnabled bool
}

// New 创建代理服务器
func New(port int, st *store.Store, caCert, caKey []byte, onCapture OnCapture, interceptor *Interceptor) *Server {
	proxy := goproxy.NewProxyHttpServer()
	proxy.Verbose = false

	bw := NewBatchWriter(st, onCapture, 50, 200*time.Millisecond)

	s := &Server{
		proxy:       proxy,
		store:       st,
		batchWriter: bw,
		interceptor: interceptor,
		onCapture:   onCapture,
		port:        port,
	}

	// 配置 HTTPS MITM
	if caCert != nil && caKey != nil {
		cert, err := tls.X509KeyPair(caCert, caKey)
		if err == nil {
			s.mitmEnabled = true
			goproxy.GoproxyCa = cert
			goproxy.OkConnect = &goproxy.ConnectAction{
				Action:    goproxy.ConnectMitm,
				TLSConfig: goproxy.TLSConfigFromCA(&cert),
			}
			goproxy.MitmConnect = &goproxy.ConnectAction{
				Action:    goproxy.ConnectMitm,
				TLSConfig: goproxy.TLSConfigFromCA(&cert),
			}
			goproxy.HTTPMitmConnect = &goproxy.ConnectAction{
				Action:    goproxy.ConnectMitm,
				TLSConfig: goproxy.TLSConfigFromCA(&cert),
			}
		}
	}

	s.setupHandlers()
	return s
}

// setupHandlers 注册请求/响应处理器
func (s *Server) setupHandlers() {
	// 请求处理器 — 始终记录请求信息，拦截为可选步骤
	s.proxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		// 1. 先捕获请求信息（无论是否启用拦截）
		captured := &models.CapturedRequest{
			Method:     req.Method,
			URL:        req.URL.String(),
			Host:       req.URL.Host,
			Path:       req.URL.Path,
			Protocol:   req.Proto,
			IsHTTPS:    req.URL.Scheme == "https",
			ReqHeaders: api.FlattenHeaders(req.Header),
			CapturedAt: time.Now(),
		}
		if req.Body != nil {
			bodyBytes, err := io.ReadAll(req.Body)
			if err == nil && len(bodyBytes) > 0 {
				if len(bodyBytes) > 32*1024 {
					captured.ReqBody = string(bodyBytes[:32*1024])
				} else {
					captured.ReqBody = string(bodyBytes)
				}
				req.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
			}
		}
		ctx.UserData = captured

		// 2. 拦截检查（manual 模式会阻塞等待）
		if s.interceptor != nil {
			return s.interceptor.Handle(req, ctx, func(r *http.Request) {
				_ = r
			})
		}
		return req, nil
	})

	// 响应处理器 — 记录响应信息
	s.proxy.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
		if ctx.UserData == nil {
			return resp
		}

		captured, ok := ctx.UserData.(*models.CapturedRequest)
		if !ok {
			return resp
		}

		captured.StatusCode = resp.StatusCode
		captured.Protocol = resp.Proto
		captured.ResHeaders = api.FlattenHeaders(resp.Header)
		captured.DurationMs = time.Since(captured.CapturedAt).Milliseconds()

		// 读取响应体（完整转发，捕获最多 64KB）
		if resp.Body != nil {
			bodyBytes, err := io.ReadAll(resp.Body)
			if err == nil && len(bodyBytes) > 0 {
				captured.SizeBytes = int64(len(bodyBytes))
				if len(bodyBytes) > 64*1024 {
					captured.ResBody = string(bodyBytes[:64*1024])
				} else {
					captured.ResBody = string(bodyBytes)
				}
				resp.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
			}
		}

		// 入队批量写入（高流量下不阻塞代理）
		s.batchWriter.Enqueue(captured)

		return resp
	})

	// CONNECT 处理器 — 不记录隧道信息，仅处理 MITM 决策
	s.proxy.OnRequest().HandleConnectFunc(func(host string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
		// 跳过非 HTTP 协议的 MITM（WNS、WebSocket、自定义 TCP 等）
		if !shouldMITM(host) {
			return &goproxy.ConnectAction{Action: goproxy.ConnectAccept, TLSConfig: nil}, host
		}

		return goproxy.OkConnect, host
	})
}

// Start 启动代理服务
func (s *Server) Start() error {
	s.mu.Lock()
	s.running = true
	s.mu.Unlock()

	addr := portToAddr(s.port)
	s.httpServer = &http.Server{
		Addr:    addr,
		Handler: s.proxy,
	}

	slog.Info("代理服务器启动", "addr", addr)

	return s.httpServer.ListenAndServe()
}

// Stop 优雅停止代理服务器
func (s *Server) Stop() {
	s.batchWriter.Stop()
	if s.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.httpServer.Shutdown(ctx); err != nil {
			slog.Warn("代理服务器关闭超时", "error", err)
		}
	}
	s.mu.Lock()
	s.running = false
	s.mu.Unlock()
}

// IsRunning 是否运行中
func (s *Server) IsRunning() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.running
}

// Port 代理端口
func (s *Server) Port() int {
	return s.port
}

func portToAddr(port int) string { return ":" + strconv.Itoa(port) }

// shouldMITM 判断某 host 是否适合 MITM 解密
// 跳过已知的非 HTTP 协议主机（WNS、遥测、自定义 TCP 等）
func shouldMITM(host string) bool {
	skipSuffixes := []string{
		".wns.windows.com",        // Windows Push Notification
		".notify.windows.com",     // Windows Notification
		".push.apple.com",         // Apple Push Notification
		".talk.google.com",        // Google Hangouts
		".events.data.msn.cn",     // Microsoft Telemetry
		".events.data.msn.com",    // Microsoft Telemetry
		".events.data.microsoft.com", // Microsoft Telemetry
		"ntp.msn.cn",              // MSN Time Sync (non-HTTP)
		"ntp.msn.com",             // MSN Time Sync
		".telemetry.microsoft.com", // Microsoft Telemetry
		".vortex.data.microsoft.com", // Microsoft Telemetry
		".settings.data.microsoft.com", // Microsoft Settings Sync
	}
	for _, s := range skipSuffixes {
		if len(host) >= len(s) && host[len(host)-len(s):] == s {
			return false
		}
	}
	// 精确匹配或包含
	for _, kw := range []string{"telemetry", "vortex.data", "settings-win.data"} {
		if strings.Contains(host, kw) {
			return false
		}
	}
	return true
}
