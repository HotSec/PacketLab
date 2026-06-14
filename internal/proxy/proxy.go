package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
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
	proxy        *goproxy.ProxyHttpServer
	httpServer   *http.Server
	store        *store.Store
	batchWriter  *BatchWriter
	interceptor  *Interceptor
	onCapture    OnCapture
	mu           sync.RWMutex
	running      bool
	port         int
	mitmEnabled  bool
	maxReqBodyKB int
	maxResBodyKB int
}

// New 创建代理服务器
func New(port int, st *store.Store, caCert, caKey []byte, onCapture OnCapture, interceptor *Interceptor, maxReqBodyKB, maxResBodyKB int) *Server {
	proxy := goproxy.NewProxyHttpServer()
	proxy.Verbose = false

	bw := NewBatchWriter(st, onCapture, 50, 200*time.Millisecond)

	s := &Server{
		proxy:        proxy,
		store:        st,
		batchWriter:  bw,
		interceptor:  interceptor,
		onCapture:    onCapture,
		port:         port,
		maxReqBodyKB: maxReqBodyKB,
		maxResBodyKB: maxResBodyKB,
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
			Method:       req.Method,
			URL:          req.URL.String(),
			Host:         req.URL.Host,
			Path:         req.URL.Path,
			Protocol:     req.Proto,
			IsHTTPS:      req.URL.Scheme == "https" || (req.Method == "CONNECT"),
			ReqHeaders:   api.FlattenHeaders(req.Header),
			CapturedAt:   time.Now(),
			CaptureMode:  "proxy",
		}
		maxReqBytes := int64(s.maxReqBodyKB) * 1024
		if req.Body != nil {
			bodyBytes, err := io.ReadAll(io.LimitReader(req.Body, maxReqBytes+1))
			if err == nil && len(bodyBytes) > 0 {
				if len(bodyBytes) > int(maxReqBytes) {
					captured.ReqBody = string(bodyBytes[:maxReqBytes])
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

		// 检测 SSE 响应
		if isSSEResponse(resp) && resp.Body != nil {
			captured.IsSSE = true
			s.handleSSEResponse(resp, captured)
			return resp
		}

		// 普通响应：读取响应体（完整转发，捕获最多 maxResBodyKB）
		maxResBytes := int64(s.maxResBodyKB) * 1024
		if resp.Body != nil {
			bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxResBytes+1))
			resp.Body = io.NopCloser(bytes.NewBuffer(bodyBytes)) // 始终恢复 body
			if err != nil {
				slog.Warn("proxy: read response body failed", "url", captured.URL, "error", err)
			} else if len(bodyBytes) > 0 {
				captured.SizeBytes = int64(len(bodyBytes))
				if len(bodyBytes) > int(maxResBytes) {
					captured.ResBody = string(bodyBytes[:maxResBytes])
				} else {
					captured.ResBody = string(bodyBytes)
				}
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
	if s.running {
		s.mu.Unlock()
		return fmt.Errorf("proxy already running")
	}
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
		".wns.windows.com",
		".notify.windows.com",
		".push.apple.com",
		".talk.google.com",
		".events.data.msn.cn",
		".events.data.msn.com",
		".events.data.microsoft.com",
		"ntp.msn.cn",
		"ntp.msn.com",
		".telemetry.microsoft.com",
		".vortex.data.microsoft.com",
		".settings.data.microsoft.com",
		".settings-win.data.microsoft.com",
	}
	for _, s := range skipSuffixes {
		if len(host) >= len(s) && host[len(host)-len(s):] == s {
			return false
		}
	}
	return true
}

// isSSEResponse 检测响应是否为 SSE 流
func isSSEResponse(resp *http.Response) bool {
	ct := resp.Header.Get("Content-Type")
	return strings.Contains(ct, "text/event-stream")
}

// handleSSEResponse 处理 SSE 流式响应：用 Pipe 同时转发给客户端和捕获事件
func (s *Server) handleSSEResponse(resp *http.Response, captured *models.CapturedRequest) {
	if captured == nil {
		return
	}
	maxResBytes := int64(s.maxResBodyKB) * 1024

	// 同步保存初始记录（只有 headers，body 为空），确保拿到真实 ID
	captured.ResBody = ""
	captured.SSEEvents = ""
	id, err := s.store.Save(captured)
	if err != nil {
		slog.Warn("proxy: SSE initial save failed", "url", captured.URL, "error", err)
		return
	}
	captured.ID = id
	if s.onCapture != nil {
		s.onCapture(captured)
	}

	// 用 pipe 实现流式转发：写入端由 SSE 读取 goroutine 控制，读取端替代原始 body
	pr, pw := io.Pipe()

	originalBody := resp.Body
	resp.Body = pr

	// SSE goroutine 修改 captured 字段时需要加锁，防止 onCapture 回调并发读取
	var capturedMu sync.Mutex

	// 启动 goroutine 流式读取 SSE
	go func() {
		defer pw.Close()
		defer originalBody.Close()

		var captureBuf strings.Builder
		var totalSize int64
		scanner := bufio.NewScanner(originalBody)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

		lastUpdate := time.Now()
		updateInterval := 500 * time.Millisecond

		for scanner.Scan() {
			line := scanner.Text()
			lineBytes := []byte(line)
			lineBytes = append(lineBytes, '\n')
			totalSize += int64(len(lineBytes))

			// 转发给客户端
			if _, err := pw.Write(lineBytes); err != nil {
				return
			}

			// 捕获事件内容
			if int64(captureBuf.Len()) < maxResBytes {
				captureBuf.WriteString(line)
				captureBuf.WriteByte('\n')
			}

			// 定时更新 DB 和推送 WebSocket
			now := time.Now()
			if now.Sub(lastUpdate) >= updateInterval {
				lastUpdate = now
				capturedMu.Lock()
				captured.ResBody = captureBuf.String()
				captured.SSEEvents = captureBuf.String()
				captured.SizeBytes = totalSize
				captured.DurationMs = time.Since(captured.CapturedAt).Milliseconds()
				capturedMu.Unlock()

				if err := s.store.UpdateResBody(captured.ID, captured.ResBody, captured.SSEEvents, captured.SizeBytes); err != nil {
					slog.Warn("proxy: SSE update DB failed", "id", captured.ID, "error", err)
				}

				// 推送 WebSocket 更新
				if s.onCapture != nil {
					capturedMu.Lock()
					s.onCapture(captured)
					capturedMu.Unlock()
				}
			}
		}

		// SSE 流结束，最终更新
		capturedMu.Lock()
		captured.ResBody = captureBuf.String()
		captured.SSEEvents = captureBuf.String()
		captured.SizeBytes = totalSize
		captured.DurationMs = time.Since(captured.CapturedAt).Milliseconds()
		capturedMu.Unlock()

		if err := s.store.UpdateResBody(captured.ID, captured.ResBody, captured.SSEEvents, captured.SizeBytes); err != nil {
			slog.Warn("proxy: SSE final update DB failed", "id", captured.ID, "error", err)
		}

		if s.onCapture != nil {
			capturedMu.Lock()
			s.onCapture(captured)
			capturedMu.Unlock()
		}
	}()
}
