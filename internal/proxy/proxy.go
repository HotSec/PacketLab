package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"packetlab/internal/api"
	"packetlab/internal/llm"
	"packetlab/internal/models"
	"packetlab/internal/store"

	"github.com/elazarl/goproxy"
)

// OnCapture 新请求捕获回调
type OnCapture func(req *models.CapturedRequest)

// requestContext is stored in ctx.UserData, extending the captured request
// with LLM-specific parsed data.
type requestContext struct {
	captured   *models.CapturedRequest
	llmProvider llm.Provider
	llmReqInfo *llm.RequestInfo
}

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
			// 全量读取请求体：转发侧需要完整 body，存储侧再取截断
			fullBody, err := io.ReadAll(req.Body)
			if err == nil {
				// 转发完整 body（设置 ContentLength 与 GetBody 以支持重试）
				req.Body = io.NopCloser(bytes.NewReader(fullBody))
				req.ContentLength = int64(len(fullBody))
				req.GetBody = func() (io.ReadCloser, error) {
					return io.NopCloser(bytes.NewReader(fullBody)), nil
				}
				// 存储侧取截断
				if len(fullBody) > 0 {
					storeBody := fullBody
					if int64(len(storeBody)) > maxReqBytes {
						storeBody = storeBody[:maxReqBytes]
					}
					captured.ReqBody = string(storeBody)
				}
			}
		}

		// LLM 检测：检查是否为已知 LLM API 请求
		reqCtx := &requestContext{captured: captured}
		provider := llm.DetectProvider(captured.Host, captured.Path)
		if provider != llm.ProviderUnknown {
			reqCtx.llmProvider = provider
			reqCtx.llmReqInfo = processLLMRequest(provider, captured.ReqBody)
		}

		ctx.UserData = reqCtx

		// 2. 拦截检查（manual 模式会阻塞等待）
		if s.interceptor != nil {
			// storeFunc 将 modify 后的请求同步回 captured，确保 OnResponse 持久化
			// 的是用户修改后的最终请求（method/url/host/headers/body）。
			storeFunc := func(r *http.Request) {
				if r == nil {
					return
				}
				captured.Method = r.Method
				captured.URL = r.URL.String()
				captured.Host = r.URL.Host
				captured.Path = r.URL.Path
				captured.ReqHeaders = api.FlattenHeaders(r.Header)
				if r.Body != nil {
					bodyBytes, err := io.ReadAll(r.Body)
					if err != nil {
						slog.Warn("proxy: storeFunc read modified body failed", "url", captured.URL, "error", err)
						return
					}
					// 恢复 body 供后续转发消费
					r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
					r.ContentLength = int64(len(bodyBytes))
					if len(bodyBytes) > 0 {
						storeBody := bodyBytes
						if int64(len(storeBody)) > maxReqBytes {
							storeBody = storeBody[:maxReqBytes]
						}
						captured.ReqBody = string(storeBody)
					} else {
						captured.ReqBody = ""
					}
				} else {
					captured.ReqBody = ""
				}
			}
			return s.interceptor.Handle(req, ctx, storeFunc)
		}
		return req, nil
	})

	// 响应处理器 — 记录响应信息
	s.proxy.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
		if ctx.UserData == nil {
			return resp
		}

		reqCtx, ok := ctx.UserData.(*requestContext)
		if !ok {
			return resp
		}
		captured := reqCtx.captured

		captured.StatusCode = resp.StatusCode
		captured.Protocol = resp.Proto
		captured.ResHeaders = api.FlattenHeaders(resp.Header)
		captured.DurationMs = time.Since(captured.CapturedAt).Milliseconds()

		isLLM := llm.IsValidProvider(reqCtx.llmProvider)

		// 检测 SSE 响应
		if isSSEResponse(resp) && resp.Body != nil {
			captured.IsSSE = true
			s.handleSSEResponse(resp, captured, reqCtx)
			return resp
		}

		// 普通响应：读取响应体（完整转发，捕获最多 maxResBodyKB）
		maxResBytes := int64(s.maxResBodyKB) * 1024
		if resp.Body != nil {
			// 全量读取响应体：转发侧需要完整 body，存储侧再取截断
			fullBody, err := io.ReadAll(resp.Body)
			resp.Body = io.NopCloser(bytes.NewReader(fullBody)) // 始终恢复完整 body
			if err != nil {
				slog.Warn("proxy: read response body failed", "url", captured.URL, "error", err)
			} else if len(fullBody) > 0 {
				captured.SizeBytes = int64(len(fullBody))
				storeBody := fullBody
				if int64(len(storeBody)) > maxResBytes {
					storeBody = storeBody[:maxResBytes]
				}
				captured.ResBody = string(storeBody)
			}
		}

		// LLM 请求：同步保存 + 提取 LLM 数据
		if isLLM {
			s.saveLLMCaptured(captured, reqCtx)
			return resp
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
	// 先 Shutdown HTTP 服务器：在途请求需要继续走 OnResponse → batchWriter.Enqueue
	// 完成入队；若先 batchWriter.Stop()，Shutdown 期间的在途请求 OnResponse 调用
	// Enqueue 会将 req 永久滞留 channel（无人消费）。
	// Shutdown HTTP server first: in-flight requests still go through
	// OnResponse → batchWriter.Enqueue; stopping batchWriter first would leave
	// those enqueued reqs stranded with no consumer.
	if s.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.httpServer.Shutdown(ctx); err != nil {
			slog.Warn("代理服务器关闭超时", "error", err)
		}
	}
	s.batchWriter.Stop()
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
func shouldMITM(rawHost string) bool {
	host := rawHost
	if h, _, err := net.SplitHostPort(rawHost); err == nil {
		host = h
	}
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
		if host == s || strings.HasSuffix(host, s) {
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
func (s *Server) handleSSEResponse(resp *http.Response, captured *models.CapturedRequest, reqCtx *requestContext) {
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

	// LLM 流式响应：创建 assembler
	isLLM := reqCtx != nil && llm.IsValidProvider(reqCtx.llmProvider)
	var llmAssembler *llm.StreamAssembler
	if isLLM {
		llmAssembler = llm.NewStreamAssembler(reqCtx.llmProvider)
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
		// sseDataBuf 累积同一 SSE 事件内的多行 data，遇空行（事件边界）再 Feed。
		// SSE 规范允许一个事件包含多个 data: 行，应用 \n 连接后作为单个 payload。
		var sseDataBuf strings.Builder
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

			// LLM: 累积同一事件的 data 行，遇空行（事件边界）再 Feed。
			// 支持 SSE 多行 data（Anthropic 等协议可能使用），单行 data
			// 行为与原 parseSSEDataLine 一致（每个事件一个 data 行时仍逐条 Feed）。
			if llmAssembler != nil {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "data:") {
					data := strings.TrimSpace(trimmed[5:])
					if data != "" && data != "[DONE]" {
						if sseDataBuf.Len() > 0 {
							sseDataBuf.WriteString("\n")
						}
						sseDataBuf.WriteString(data)
					}
				} else if trimmed == "" {
					// 事件边界：Feed 累积的 data
					if sseDataBuf.Len() > 0 {
						llmAssembler.Feed([]byte(sseDataBuf.String()))
						sseDataBuf.Reset()
					}
				}
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

		// 检查 scanner 错误（如单行超过 1MB 缓冲区限制）
		if err := scanner.Err(); err != nil {
			slog.Warn("proxy: SSE scanner error, stream may be incomplete", "url", captured.URL, "error", err)
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

		// LLM: save assembled exchange after stream completes
		if llmAssembler != nil {
			// flush 残留 sseDataBuf：流末尾未以空行收尾时，最后一条事件可能尚未 Feed
			if sseDataBuf.Len() > 0 {
				llmAssembler.Feed([]byte(sseDataBuf.String()))
				sseDataBuf.Reset()
			}
			resInfo := llmAssembler.Result()
			ex := buildLLMExchange(reqCtx.llmProvider, reqCtx.llmReqInfo, resInfo, captured.CapturedAt)
			saveLLMExchange(s.store, captured.ID, ex)
		}

		if s.onCapture != nil {
			capturedMu.Lock()
			s.onCapture(captured)
			capturedMu.Unlock()
		}
	}()
}

// saveLLMCaptured handles synchronous save + LLM data extraction for non-streamed LLM responses.
func (s *Server) saveLLMCaptured(captured *models.CapturedRequest, reqCtx *requestContext) {
	// Synchronous save to get the ID immediately
	id, err := s.store.Save(captured)
	if err != nil {
		slog.Warn("proxy: LLM save failed", "url", captured.URL, "error", err)
		return
	}
	captured.ID = id

	// Parse response body as LLM response
	resInfo := processLLMResponse(reqCtx.llmProvider, captured.ResBody)
	ex := buildLLMExchange(reqCtx.llmProvider, reqCtx.llmReqInfo, resInfo, captured.CapturedAt)
	saveLLMExchange(s.store, id, ex)

	if s.onCapture != nil {
		s.onCapture(captured)
	}
}
