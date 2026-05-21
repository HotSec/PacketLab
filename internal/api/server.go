package api

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"packetlab/internal/models"
	"packetlab/internal/store"

	"github.com/gorilla/websocket"
)

// Server API 服务器
type Server struct {
	store       *store.Store
	hub         *wsHub
	mux         *http.ServeMux
	frontend    http.Handler
	interceptor interface {
		GetMode() string
		SetMode(mode string)
		GetPending() []models.PendingRequest
		Resolve(id string, result models.InterceptResult) error
		SetRules(rules []models.InterceptRule)
	}
}

// New 创建 API 服务器
func New(st *store.Store, frontendHandler http.Handler) *Server {
	hub := newWSHub()
	go hub.run()

	s := &Server{
		store:    st,
		hub:      hub,
		frontend: frontendHandler,
	}

	s.setupRoutes()
	return s
}

// setupRoutes 注册路由
func (s *Server) setupRoutes() {
	mux := http.NewServeMux()

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

	// WebSocket
	mux.HandleFunc("/ws", s.handleWebSocket)

	// 静态文件（SPA fallback）
	if s.frontend != nil {
		mux.Handle("/", s.frontend)
	}

	s.mux = mux
}

// Handler 返回 HTTP Handler（含 CORS）
func (s *Server) Handler() http.Handler {
	return corsMiddleware(s.mux)
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
// Handlers
// ========================================

// handleListRequests GET /api/requests
func (s *Server) handleListRequests(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
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

	items, total, err := s.store.List(method, search, host, errorOnly, limit, offset)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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
	// 提取 ID
	idStr := r.URL.Path[len("/api/requests/"):]
	if idStr == "" {
		http.Error(w, "Missing id", http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "Invalid id", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getRequest(w, r, id)
	case http.MethodDelete:
		s.deleteRequest(w, r, id)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) getRequest(w http.ResponseWriter, r *http.Request, id int64) {
	req, err := s.store.Get(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Not found"})
		return
	}
	writeJSON(w, http.StatusOK, req)
}

func (s *Server) deleteRequest(w http.ResponseWriter, r *http.Request, id int64) {
	if err := s.store.Delete(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleResend POST /api/resend
func (s *Server) handleResend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body models.ResendRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}

	// 执行重发
	resp, err := doResend(&body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
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

	id, _ := s.store.Save(captured)
	resp.ID = id

	// 广播
	if s.hub != nil {
		s.hub.broadcast(captured)
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleClear POST /api/clear
func (s *Server) handleClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := s.store.Clear(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
}

// handleStats GET /api/stats
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	total, errors, totalSize, err := s.store.Stats()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total":      total,
		"errors":     errors,
		"total_size": totalSize,
	})
}

// handleProxyStatus GET /api/proxy/status
func (s *Server) handleProxyStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"running": true,
		"port":    8080,
	})
}

// ========================================
// API Map
// ========================================

// handleAPIMap GET /api/apimap?host=...
func (s *Server) handleAPIMap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	host := r.URL.Query().Get("host")
	if host == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "host required"})
		return
	}
	tree, err := s.store.GetAPIMap(host)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, tree)
}

// handleAPIHosts GET /api/apimap/hosts?search=&limit=&offset=
func (s *Server) handleAPIHosts(w http.ResponseWriter, r *http.Request) {
	search := r.URL.Query().Get("search")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	hosts, total, err := s.store.ListHosts(search, limit, offset)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
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

// handleAPINotes POST /api/apimap/notes (save/update)
func (s *Server) handleAPINotes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body models.APINoteRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
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
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	note.ID = id
	writeJSON(w, http.StatusOK, note)
}

// handleAPINoteByID DELETE /api/apimap/notes/{id}
func (s *Server) handleAPINoteByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	idStr := r.URL.Path[len("/api/apimap/notes/"):]
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "Invalid id", http.StatusBadRequest)
		return
	}
	if err := s.store.DeleteAPINote(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ========================================
// Intercept
// ========================================

func (s *Server) handleInterceptMode(w http.ResponseWriter, r *http.Request) {
	if s.interceptor == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Interceptor not available"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]string{"mode": s.interceptor.GetMode()})
	case http.MethodPost:
		var body struct{ Mode string `json:"mode"` }
		json.NewDecoder(r.Body).Decode(&body)
		if body.Mode != "auto" && body.Mode != "manual" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "mode must be auto or manual"})
			return
		}
		s.interceptor.SetMode(body.Mode)
		s.store.SetSetting("intercept_mode", body.Mode)
		writeJSON(w, http.StatusOK, map[string]string{"mode": body.Mode})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
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
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.interceptor == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Interceptor not available"})
		return
	}
	var result models.InterceptResult
	if err := json.NewDecoder(r.Body).Decode(&result); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}
	if err := s.interceptor.Resolve(result.RequestID, result); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "resolved"})
}

func (s *Server) handleInterceptRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rules, err := s.store.ListRules()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if rules == nil { rules = []models.InterceptRule{} }
		writeJSON(w, http.StatusOK, rules)
	case http.MethodPost:
		var rule models.InterceptRule
		if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
			return
		}
		id, err := s.store.SaveRule(&rule)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		rule.ID = id
		if s.interceptor != nil {
			rules, _ := s.store.ListRules()
			s.interceptor.SetRules(rules)
		}
		writeJSON(w, http.StatusCreated, rule)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleInterceptRuleByID(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Path[len("/api/intercept/rules/"):]
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "Invalid id", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var body struct{ Enabled bool `json:"enabled"` }
		json.NewDecoder(r.Body).Decode(&body)
		s.store.UpdateRule(id, body.Enabled)
		if s.interceptor != nil {
			rules, _ := s.store.ListRules()
			s.interceptor.SetRules(rules)
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
	case http.MethodDelete:
		s.store.DeleteRule(id)
		if s.interceptor != nil {
			rules, _ := s.store.ListRules()
			s.interceptor.SetRules(rules)
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// ========================================
// WebSocket
// ========================================

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // 本地工具，允许所有来源
	},
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[ws] upgrade error: %v", err)
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

func doResend(req *models.ResendRequest) (*resendResult, error) {
	parsedURL, err := url.Parse(req.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}

	isHTTPS := parsedURL.Scheme == "https"
	startTime := time.Now()

	// 构建 HTTP 请求
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
		httpReq.Header.Set("User-Agent", "PacketLab/1.0")
	}

	// 发送请求
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	return &resendResult{
		StatusCode: resp.StatusCode,
		Host:       parsedURL.Host,
		Path:       parsedURL.Path,
		Proto:      resp.Proto,
		IsHTTPS:    isHTTPS,
		ResHeaders: flattenHeaders(resp.Header),
		ResBody:    string(respBody),
		DurationMs: time.Since(startTime).Milliseconds(),
		SizeBytes:  int64(len(respBody)),
	}, nil
}

func flattenHeaders(h http.Header) map[string]string {
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

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}
