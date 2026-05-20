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
	proxy     *goproxy.ProxyHttpServer
	store     *store.Store
	onCapture OnCapture
	mu        sync.RWMutex
	running   bool
	port      int
	mitmEnabled bool
}

// New 创建代理服务器
func New(port int, st *store.Store, caCert, caKey []byte, onCapture OnCapture) *Server {
	proxy := goproxy.NewProxyHttpServer()
	proxy.Verbose = false

	s := &Server{
		proxy:     proxy,
		store:     st,
		onCapture: onCapture,
		port:      port,
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
		// 存储请求上下文
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

		// 读取请求体
		if req.Body != nil {
			bodyBytes, err := io.ReadAll(req.Body)
			if err == nil {
				captured.ReqBody = string(bodyBytes)
				// 恢复 body 以便后续处理
				req.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
			}
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

		// 读取响应体
		if resp.Body != nil {
			bodyBytes, err := io.ReadAll(resp.Body)
			if err == nil {
				captured.ResBody = truncateBody(string(bodyBytes), 64*1024)
				captured.SizeBytes = int64(len(bodyBytes))
				resp.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
			} else {
				// 读取失败时也需恢复（避免客户端收到损坏响应）
				resp.Body = io.NopCloser(bytes.NewReader(nil))
			}
		}

		// 异步保存
		go func() {
			id, err := s.store.Save(captured)
			if err == nil {
				captured.ID = id
			}
			if s.onCapture != nil {
				s.onCapture(captured)
			}
		}()

		return resp
	})

	// CONNECT 处理器（HTTPS 隧道）
	s.proxy.OnRequest().HandleConnectFunc(func(host string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
		// MITM 未启用时，记录 CONNECT 隧道信息
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

			go func() {
				id, _ := s.store.Save(captured)
				captured.ID = id
				if s.onCapture != nil {
					s.onCapture(captured)
				}
			}()
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

// flattenHeaders 将 http.Header 转为 map[string]string（取每个 header 的第一个值）
func flattenHeaders(h http.Header) map[string]string {
	result := make(map[string]string)
	for k, v := range h {
		if len(v) > 0 {
			result[k] = v[0]
		}
	}
	return result
}

// truncateBody 截断过大的响应体
func truncateBody(body string, maxSize int) string {
	if len(body) > maxSize {
		return body[:maxSize] + fmt.Sprintf("\n\n... [截断: 共 %d bytes]", len(body))
	}
	return body
}
