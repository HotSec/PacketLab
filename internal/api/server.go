package api

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"packetlab/internal/capture"
	"packetlab/internal/models"
	"packetlab/internal/store"

	"github.com/gorilla/websocket"
)

const maxRequestBodySize = 10 * 1024 * 1024 // 10 MB

// Server API 服务器
type Server struct {
	store         *store.Store
	hub           *wsHub
	mux           *http.ServeMux
	frontend      http.Handler
	insecure      bool
	captureEngine interface {
		IsRunning() bool
		Start() error
		Stop()
	}
	interceptor interface {
		GetMode() string
		SetMode(mode string)
		GetPending() []models.PendingRequest
		Resolve(id string, result models.InterceptResult) error
		SetRules(rules []models.InterceptRule)
	}
}

// New 创建 API 服务器
func New(st *store.Store, frontendHandler http.Handler, insecure bool) *Server {
	hub := newWSHub()
	go hub.run()

	s := &Server{
		store:    st,
		hub:      hub,
		frontend: frontendHandler,
		insecure: insecure,
	}

	s.setupRoutes()
	return s
}

// setupRoutes 注册路由
func (s *Server) setupRoutes() {
	mux := http.NewServeMux()

	// 健康检查
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/ready", s.handleReady)

	// API 路由
	mux.HandleFunc("/api/requests", s.handleListRequests)
	mux.HandleFunc("/api/requests/", s.handleRequestByID)
	mux.HandleFunc("/api/resend", s.handleResend)
	mux.HandleFunc("/api/clear", s.handleClear)
	mux.HandleFunc("/api/stats", s.handleStats)
	mux.HandleFunc("/api/proxy/status", s.handleProxyStatus)

	// API 地图
	mux.HandleFunc("/api/apimap", s.handleAPIMap)
	mux.HandleFunc("/api/apimap/notes", s.handleAPINotes)
	mux.HandleFunc("/api/apimap/notes/", s.handleAPINoteByID)
	mux.HandleFunc("/api/apimap/hosts", s.handleAPIHosts)

	// 拦截
	mux.HandleFunc("/api/intercept/mode", s.handleInterceptMode)
	mux.HandleFunc("/api/intercept/pending", s.handleInterceptPending)
	mux.HandleFunc("/api/intercept/action", s.handleInterceptAction)
	mux.HandleFunc("/api/intercept/rules", s.handleInterceptRules)
	mux.HandleFunc("/api/intercept/rules/", s.handleInterceptRuleByID)

	// 网卡抓包
	mux.HandleFunc("/api/capture/status", s.handleCaptureStatus)
	mux.HandleFunc("/api/capture/interfaces", s.handleCaptureInterfaces)
	mux.HandleFunc("/api/capture/start", s.handleCaptureStart)
	mux.HandleFunc("/api/capture/stop", s.handleCaptureStop)

	// 导出
	mux.HandleFunc("/api/export/har", s.handleExportHAR)

	// WebSocket
	mux.HandleFunc("/ws", s.handleWebSocket)

	// 静态文件（SPA fallback）
	if s.frontend != nil {
		mux.Handle("/", s.frontend)
	}

	s.mux = mux
}

// Handler 返回 HTTP Handler（含 CORS + 安全头）
func (s *Server) Handler() http.Handler {
	return securityHeadersMiddleware(corsMiddleware(s.mux))
}

// SetCaptureEngine 设置抓包引擎
func (s *Server) SetCaptureEngine(ce interface {
	IsRunning() bool
	Start() error
	Stop()
}) {
	s.captureEngine = ce
}

// SetInterceptor 设置拦截控制器引用
func (s *Server) SetInterceptor(interceptor interface {
	GetMode() string
	SetMode(mode string)
	GetPending() []models.PendingRequest
	Resolve(id string, result models.InterceptResult) error
	SetRules(rules []models.InterceptRule)
}) {
	s.interceptor = interceptor
}

// BroadcastIntercept 广播待审批请求到 WebSocket
func (s *Server) BroadcastIntercept(req *models.PendingRequest) {
	s.hub.broadcastIntercept(req)
}

// BroadcastCapture 广播新捕获的请求
func (s *Server) BroadcastCapture(req *models.CapturedRequest) {
	s.hub.broadcast(req)
}

// ========================================
// Health Checks
// ========================================

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	checks := map[string]string{"database": "ok"}
	_, _, _, err := s.store.Stats()
	if err != nil {
		checks["database"] = "error"
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"status": "degraded",
			"checks": checks,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "ok",
		"checks": checks,
	})
}

// ========================================
// Handlers
// ========================================

// handleListRequests GET /api/requests
func (s *Server) handleListRequests(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, apiError("Method not allowed"))
		return
	}

	method := r.URL.Query().Get("method")
	search := r.URL.Query().Get("search")
	host := r.URL.Query().Get("host")
	errorOnly := r.URL.Query().Get("error_only") == "true"
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	items, total, err := s.store.List(method, search, host, errorOnly, limit, offset)
	if err != nil {
		slog.Error("list requests failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, apiError("Failed to list requests"))
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"data":  items,
		"total": total,
		"limit": limit,
	})
}

// handleRequestByID GET/DELETE /api/requests/{id}
func (s *Server) handleRequestByID(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Path[len("/api/requests/"):]
	if idStr == "" {
		writeJSON(w, http.StatusBadRequest, apiError("Missing id"))
		return
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError("Invalid id"))
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getRequest(w, r, id)
	case http.MethodDelete:
		s.deleteRequest(w, r, id)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, apiError("Method not allowed"))
	}
}

func (s *Server) getRequest(w http.ResponseWriter, r *http.Request, id int64) {
	req, err := s.store.Get(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, apiError("Not found"))
		return
	}
	writeJSON(w, http.StatusOK, req)
}

func (s *Server) deleteRequest(w http.ResponseWriter, r *http.Request, id int64) {
	if err := s.store.Delete(id); err != nil {
		slog.Error("delete request failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, apiError("Failed to delete request"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleResend POST /api/resend
func (s *Server) handleResend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, apiError("Method not allowed"))
		return
	}

	// 限制请求体大小
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)

	var body models.ResendRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError("Invalid JSON or body too large (max 10MB)"))
		return
	}

	// 输入校验
	if body.Method == "" {
		writeJSON(w, http.StatusBadRequest, apiError("Method is required"))
		return
	}
	if body.URL == "" {
		writeJSON(w, http.StatusBadRequest, apiError("URL is required"))
		return
	}

	// 执行重发
	resp, err := doResend(&body, s.insecure)
	if err != nil {
		slog.Warn("resend failed", "url", body.URL, "error", err)
		writeJSON(w, http.StatusBadGateway, apiError(err.Error()))
		return
	}

	// 保存到历史
	captured := &models.CapturedRequest{
		Method:     body.Method,
		URL:        body.URL,
		Host:       resp.Host,
		Path:       resp.Path,
		Protocol:   resp.Proto,
		IsHTTPS:    resp.IsHTTPS,
		ReqHeaders: body.Headers,
		ReqBody:    body.Body,
		StatusCode: resp.StatusCode,
		ResHeaders: resp.ResHeaders,
		ResBody:    resp.ResBody,
		DurationMs: resp.DurationMs,
		SizeBytes:  resp.SizeBytes,
		CapturedAt: time.Now(),
	}

	id, err := s.store.Save(captured)
	if err != nil {
		slog.Error("save resend result failed", "error", err)
	}
	resp.ID = id

	if s.hub != nil {
		s.hub.broadcast(captured)
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleClear POST /api/clear
func (s *Server) handleClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, apiError("Method not allowed"))
		return
	}
	if err := s.store.Clear(); err != nil {
		slog.Error("clear failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, apiError("Failed to clear"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
}

// handleStats GET /api/stats
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	total, errs, totalSize, err := s.store.Stats()
	if err != nil {
		slog.Error("stats failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, apiError("Failed to get stats"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total":      total,
		"errors":     errs,
		"total_size": totalSize,
	})
}

// handleProxyStatus GET /api/proxy/status
func (s *Server) handleProxyStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"running": true,
		"mode":    "proxy",
	})
}

// ========================================
// API Map
// ========================================

func (s *Server) handleAPIMap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, apiError("Method not allowed"))
		return
	}
	host := r.URL.Query().Get("host")
	if host == "" {
		writeJSON(w, http.StatusBadRequest, apiError("host required"))
		return
	}
	tree, err := s.store.GetAPIMap(host)
	if err != nil {
		slog.Error("apimap failed", "host", host, "error", err)
		writeJSON(w, http.StatusInternalServerError, apiError("Failed to build API map"))
		return
	}
	writeJSON(w, http.StatusOK, tree)
}

func (s *Server) handleAPIHosts(w http.ResponseWriter, r *http.Request) {
	search := r.URL.Query().Get("search")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	hosts, total, err := s.store.ListHosts(search, limit, offset)
	if err != nil {
		slog.Error("list hosts failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, apiError("Failed to list hosts"))
		return
	}
	if hosts == nil {
		hosts = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"data":   hosts,
		"total":  total,
		"offset": offset,
	})
}

func (s *Server) handleAPINotes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, apiError("Method not allowed"))
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)

	var body models.APINoteRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError("Invalid JSON"))
		return
	}
	if body.Host == "" {
		writeJSON(w, http.StatusBadRequest, apiError("host is required"))
		return
	}

	note := &models.APINote{
		Host:   body.Host,
		Path:   body.Path,
		Method: body.Method,
		Note:   body.Note,
	}
	id, err := s.store.SaveAPINote(note)
	if err != nil {
		slog.Error("save note failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, apiError("Failed to save note"))
		return
	}
	note.ID = id
	writeJSON(w, http.StatusOK, note)
}

func (s *Server) handleAPINoteByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeJSON(w, http.StatusMethodNotAllowed, apiError("Method not allowed"))
		return
	}
	idStr := r.URL.Path[len("/api/apimap/notes/"):]
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError("Invalid id"))
		return
	}
	if err := s.store.DeleteAPINote(id); err != nil {
		slog.Error("delete note failed", "id", id, "error", err)
		writeJSON(w, http.StatusInternalServerError, apiError("Failed to delete note"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ========================================
// Intercept
// ========================================

func (s *Server) handleInterceptMode(w http.ResponseWriter, r *http.Request) {
	if s.interceptor == nil {
		writeJSON(w, http.StatusServiceUnavailable, apiError("Interceptor not available"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]string{"mode": s.interceptor.GetMode()})
	case http.MethodPost:
		var body struct{ Mode string `json:"mode"` }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, apiError("Invalid JSON"))
			return
		}
		if body.Mode != "auto" && body.Mode != "manual" {
			writeJSON(w, http.StatusBadRequest, apiError("mode must be auto or manual"))
			return
		}
		s.interceptor.SetMode(body.Mode)
		if err := s.store.SetSetting("intercept_mode", body.Mode); err != nil {
			slog.Warn("save intercept mode failed", "error", err)
		}
		writeJSON(w, http.StatusOK, map[string]string{"mode": body.Mode})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, apiError("Method not allowed"))
	}
}

func (s *Server) handleInterceptPending(w http.ResponseWriter, r *http.Request) {
	if s.interceptor == nil {
		writeJSON(w, http.StatusOK, []interface{}{})
		return
	}
	writeJSON(w, http.StatusOK, s.interceptor.GetPending())
}

func (s *Server) handleInterceptAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, apiError("Method not allowed"))
		return
	}
	if s.interceptor == nil {
		writeJSON(w, http.StatusServiceUnavailable, apiError("Interceptor not available"))
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)

	var result models.InterceptResult
	if err := json.NewDecoder(r.Body).Decode(&result); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError("Invalid JSON"))
		return
	}
	if err := s.interceptor.Resolve(result.RequestID, result); err != nil {
		writeJSON(w, http.StatusNotFound, apiError(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "resolved"})
}

func (s *Server) handleInterceptRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rules, err := s.store.ListRules()
		if err != nil {
			slog.Error("list rules failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, apiError("Failed to list rules"))
			return
		}
		if rules == nil {
			rules = []models.InterceptRule{}
		}
		writeJSON(w, http.StatusOK, rules)
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)

		var rule models.InterceptRule
		if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
			writeJSON(w, http.StatusBadRequest, apiError("Invalid JSON"))
			return
		}
		if rule.Pattern == "" || (rule.Action != "allow" && rule.Action != "block") {
			writeJSON(w, http.StatusBadRequest, apiError("pattern required, action must be allow or block"))
			return
		}
		id, err := s.store.SaveRule(&rule)
		if err != nil {
			slog.Error("save rule failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, apiError("Failed to save rule"))
			return
		}
		rule.ID = id
		if s.interceptor != nil {
			rules, err := s.store.ListRules()
			if err == nil {
				s.interceptor.SetRules(rules)
			}
		}
		writeJSON(w, http.StatusCreated, rule)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, apiError("Method not allowed"))
	}
}

func (s *Server) handleInterceptRuleByID(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Path[len("/api/intercept/rules/"):]
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError("Invalid id"))
		return
	}
	switch r.Method {
	case http.MethodPut:
		var body struct{ Enabled bool `json:"enabled"` }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, apiError("Invalid JSON"))
			return
		}
		if err := s.store.UpdateRule(id, body.Enabled); err != nil {
			slog.Error("update rule failed", "id", id, "error", err)
			writeJSON(w, http.StatusInternalServerError, apiError(err.Error()))
			return
		}
		if s.interceptor != nil {
			rules, err := s.store.ListRules()
			if err == nil {
				s.interceptor.SetRules(rules)
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
	case http.MethodDelete:
		if err := s.store.DeleteRule(id); err != nil {
			slog.Error("delete rule failed", "id", id, "error", err)
			writeJSON(w, http.StatusInternalServerError, apiError(err.Error()))
			return
		}
		if s.interceptor != nil {
			rules, err := s.store.ListRules()
			if err == nil {
				s.interceptor.SetRules(rules)
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, apiError("Method not allowed"))
	}
}

// ========================================
// 网卡抓包
// ========================================

func (s *Server) handleCaptureStatus(w http.ResponseWriter, r *http.Request) {
	if s.captureEngine == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"running": false, "available": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"running":   s.captureEngine.IsRunning(),
		"available": true,
	})
}

func (s *Server) handleCaptureInterfaces(w http.ResponseWriter, r *http.Request) {
	ifaces, err := capture.ListInterfaces()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, ifaces)
}

func (s *Server) handleCaptureStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		Interface string `json:"interface"`
		BPF       string `json:"bpf"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	if body.Interface == "" {
		body.Interface = capture.DetectInterface()
	}
	if body.BPF == "" {
		body.BPF = "tcp port 80 or tcp port 443"
	}

	// 动态创建引擎（如果尚未创建）
	if s.captureEngine == nil {
		ce := capture.New(body.Interface, body.BPF, s.store, s)
		s.SetCaptureEngine(ce)
	}

	if err := s.captureEngine.Start(); err != nil {
		slog.Error("capture start failed", "iface", body.Interface, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
			"hint":  "可能需要 sudo 权限运行",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "started",
		"iface":  body.Interface,
		"bpf":    body.BPF,
	})
}

func (s *Server) handleCaptureStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.captureEngine == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "no engine"})
		return
	}
	s.captureEngine.Stop()
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

// ========================================
// 导出
// ========================================

func (s *Server) handleExportHAR(w http.ResponseWriter, r *http.Request) {
	limit := 500
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	items, _, err := s.store.List("", "", "", false, limit, 0)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apiError(err.Error()))
		return
	}

	entries := make([]map[string]interface{}, 0, len(items))
	for _, item := range items {
		req, err := s.store.Get(item.ID)
		if err != nil || req == nil {
			continue
		}
		entry := map[string]interface{}{
			"startedDateTime": req.CapturedAt.Format("2006-01-02T15:04:05.000Z"),
			"time":            req.DurationMs,
			"request": map[string]interface{}{
				"method":      req.Method,
				"url":         req.URL,
				"httpVersion": "HTTP/1.1",
				"headers":     toHARHeaders(req.ReqHeaders),
				"headersSize": -1,
				"bodySize":    len(req.ReqBody),
			},
			"response": map[string]interface{}{
				"status":      req.StatusCode,
				"statusText":  http.StatusText(req.StatusCode),
				"httpVersion": "HTTP/1.1",
				"headers":     toHARHeaders(req.ResHeaders),
				"headersSize": -1,
				"bodySize":    len(req.ResBody),
				"content": map[string]interface{}{
					"size": len(req.ResBody),
					"text": req.ResBody,
				},
			},
			"cache":      map[string]interface{}{},
			"timings":    map[string]interface{}{"send": 0, "wait": req.DurationMs, "receive": 0},
			"serverIPAddress": req.Host,
		}
		entries = append(entries, entry)
	}

	har := map[string]interface{}{
		"log": map[string]interface{}{
			"version": "1.2",
			"creator": map[string]string{
				"name":    "PacketLab",
				"version": "2.0",
			},
			"entries": entries,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", "attachment; filename=packetlab.har")
	json.NewEncoder(w).Encode(har)
}

func toHARHeaders(headers map[string]string) []map[string]string {
	var h []map[string]string
	for k, v := range headers {
		h = append(h, map[string]string{"name": k, "value": v})
	}
	return h
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		return origin == "" ||
			strings.HasPrefix(origin, "http://localhost") ||
			strings.HasPrefix(origin, "http://127.0.0.1") ||
			strings.HasPrefix(origin, "http://[::1]")
	},
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("ws upgrade failed", "error", err)
		return
	}

	client := &wsClient{
		hub:  s.hub,
		conn: conn,
		send: make(chan []byte, 64),
	}

	s.hub.register <- client

	go client.writePump()
	go client.readPump()
}

// ========================================
// Resend Logic
// ========================================

type resendResult struct {
	ID         int64             `json:"id"`
	StatusCode int               `json:"status_code"`
	Host       string            `json:"-"`
	Path       string            `json:"-"`
	Proto      string            `json:"-"`
	IsHTTPS    bool              `json:"-"`
	ResHeaders map[string]string `json:"res_headers"`
	ResBody    string            `json:"res_body"`
	DurationMs int64             `json:"duration_ms"`
	SizeBytes  int64             `json:"size_bytes"`
}

func doResend(req *models.ResendRequest, insecure bool) (*resendResult, error) {
	parsedURL, err := url.Parse(req.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme: %s", parsedURL.Scheme)
	}

	isHTTPS := parsedURL.Scheme == "https"
	startTime := time.Now()

	var bodyReader io.Reader
	if req.Body != "" {
		bodyReader = bytes.NewBufferString(req.Body)
	}

	httpReq, err := http.NewRequest(req.Method, req.URL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	if httpReq.Header.Get("User-Agent") == "" {
		httpReq.Header.Set("User-Agent", "PacketLab/2.0")
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure},
		},
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	// 限制读取响应体大小
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxRequestBodySize))
	if err != nil {
		slog.Warn("resend read response body failed", "url", req.URL, "error", err)
	}

	return &resendResult{
		StatusCode: resp.StatusCode,
		Host:       parsedURL.Host,
		Path:       parsedURL.Path,
		Proto:      resp.Proto,
		IsHTTPS:    isHTTPS,
		ResHeaders: FlattenHeaders(resp.Header),
		ResBody:    string(respBody),
		DurationMs: time.Since(startTime).Milliseconds(),
		SizeBytes:  int64(len(respBody)),
	}, nil
}

func FlattenHeaders(h http.Header) map[string]string {
	result := make(map[string]string)
	for k, v := range h {
		if len(v) > 0 {
			result[k] = v[0]
		}
	}
	return result
}

// ========================================
// Utilities
// ========================================

func apiError(msg string) map[string]string {
	return map[string]string{"error": msg}
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Warn("write JSON response failed", "error", err)
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Max-Age", "86400")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-XSS-Protection", "1; mode=block")
		next.ServeHTTP(w, r)
	})
}
