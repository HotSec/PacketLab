package proxy

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"packetlab/internal/models"
	"packetlab/internal/store"

	"github.com/elazarl/goproxy"
)

// OnCapture 新请求捕获回调
type OnCapture func(req *models.CapturedRequest)

// Server 代理服务器
type Server struct {
	proxy       *goproxy.ProxyHttpServer
	store       *store.Store
	batchWriter *BatchWriter
	onCapture   OnCapture
	mu          sync.RWMutex
	running     bool
	port        int
	mitmEnabled bool
}

// New 创建代理服务器
func New(port int, st *store.Store, caCert, caKey []byte, onCapture OnCapture) *Server {
	proxy := goproxy.NewProxyHttpServer()
	proxy.Verbose = false

	bw := NewBatchWriter(st, onCapture, 50, 200*time.Millisecond)

	s := &Server{
		proxy:       proxy,
		store:       st,
		batchWriter: bw,
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
	// 请求处理器 — 记录请求信息
	s.proxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		captured := &models.CapturedRequest{
			Method:     req.Method,
			URL:        req.URL.String(),
			Host:       req.URL.Host,
			Path:       req.URL.Path,
			Protocol:   req.Proto,
			IsHTTPS:    req.URL.Scheme == "https",
			ReqHeaders: flattenHeaders(req.Header),
			CapturedAt: time.Now(),
		}

		// 读取请求体（限制 32KB）
		if req.Body != nil {
			limitedReader := io.LimitReader(req.Body, 32*1024)
			bodyBytes, err := io.ReadAll(limitedReader)
			if err == nil {
				captured.ReqBody = string(bodyBytes)
			}
			// 恢复 body
			req.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
		}

		ctx.UserData = captured
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
		captured.ResHeaders = flattenHeaders(resp.Header)
		captured.DurationMs = time.Since(captured.CapturedAt).Milliseconds()

		// 读取响应体（限制 64KB）
		if resp.Body != nil {
			limitedReader := io.LimitReader(resp.Body, 64*1024)
			bodyBytes, err := io.ReadAll(limitedReader)
			if err == nil {
				captured.ResBody = string(bodyBytes)
				captured.SizeBytes = int64(len(bodyBytes))
				resp.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
			} else {
				resp.Body = io.NopCloser(bytes.NewReader(nil))
			}
		}

		// 入队批量写入（高流量下不阻塞代理）
		s.batchWriter.Enqueue(captured)

		return resp
	})

	// CONNECT 处理器（HTTPS 隧道 — MITM 未启用时记录）
	s.proxy.OnRequest().HandleConnectFunc(func(host string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
		if !s.mitmEnabled {
			captured := &models.CapturedRequest{
				Method:     "CONNECT",
				URL:        "https://" + host,
				Host:       host,
				Path:       "/",
				Protocol:   "HTTPS",
				IsHTTPS:    true,
				ReqHeaders: map[string]string{"Host": host},
				CapturedAt: time.Now(),
			}
			s.batchWriter.Enqueue(captured)
		}
		return goproxy.OkConnect, host
	})
}

// Start 启动代理服务
func (s *Server) Start() error {
	s.mu.Lock()
	s.running = true
	s.mu.Unlock()

	addr := fmt.Sprintf(":%d", s.port)
	server := &http.Server{
		Addr:    addr,
		Handler: s.proxy,
	}

	fmt.Printf("[proxy] 代理服务器启动在 %s\n", addr)
	fmt.Printf("[proxy] 配置浏览器代理为 localhost:%d\n", s.port)

	return server.ListenAndServe()
}

// Stop 停止代理
func (s *Server) Stop() {
	s.batchWriter.Stop()
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

// flattenHeaders 将 http.Header 转为 map[string]string（取第一个值）
func flattenHeaders(h http.Header) map[string]string {
	result := make(map[string]string)
	for k, v := range h {
		if len(v) > 0 {
			result[k] = v[0]
		}
	}
	return result
}
