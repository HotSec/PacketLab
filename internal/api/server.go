package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"packetlab/internal/capture"
	"packetlab/internal/models"
	"packetlab/internal/store"

	"github.com/gorilla/websocket"
)

const maxRequestBodySize = 10 * 1024 * 1024

type Server struct {
	store         *store.Store
	hub           *wsHub
	mux           *http.ServeMux
	frontend      http.Handler
	resendSvc     *ResendService
	harSvc        *HARService
	rateLimiter   *rateLimiter
	captureEngine interface {
		IsRunning() bool
		Start() error
		Stop()
		GetMetrics() map[string]interface{}
	}
	interceptor interface {
		GetMode() string
		SetMode(mode string)
		GetPending() []models.PendingRequest
		Resolve(id string, result models.InterceptResult) error
		SetRules(rules []models.InterceptRule)
	}
}

func New(st *store.Store, frontendHandler http.Handler, insecure bool) *Server {
	hub := newWSHub()
	go hub.run()

	s := &Server{
		store:       st,
		hub:         hub,
		frontend:    frontendHandler,
		resendSvc:   NewResendService(st, hub, insecure),
		harSvc:      NewHARService(st),
		rateLimiter: newRateLimiter(120, time.Minute),
	}

	s.setupRoutes()
	return s
}

func (s *Server) setupRoutes() {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/ready", s.handleReady)

	mux.HandleFunc("/api/requests", s.handleListRequests)
	mux.HandleFunc("/api/requests/", s.handleRequestByID)
	mux.HandleFunc("/api/resend", s.handleResend)
	mux.HandleFunc("/api/clear", s.handleClear)
	mux.HandleFunc("/api/stats", s.handleStats)
	mux.HandleFunc("/api/proxy/status", s.handleProxyStatus)

	mux.HandleFunc("/api/apimap", s.handleAPIMap)
	mux.HandleFunc("/api/apimap/notes", s.handleAPINotes)
	mux.HandleFunc("/api/apimap/notes/", s.handleAPINoteByID)
	mux.HandleFunc("/api/apimap/hosts", s.handleAPIHosts)

	mux.HandleFunc("/api/intercept/mode", s.handleInterceptMode)
	mux.HandleFunc("/api/intercept/pending", s.handleInterceptPending)
	mux.HandleFunc("/api/intercept/action", s.handleInterceptAction)
	mux.HandleFunc("/api/intercept/rules", s.handleInterceptRules)
	mux.HandleFunc("/api/intercept/rules/", s.handleInterceptRuleByID)

	mux.HandleFunc("/api/intercept/logs", s.handleInterceptLogs)

	mux.HandleFunc("/api/capture/status", s.handleCaptureStatus)
	mux.HandleFunc("/api/capture/interfaces", s.handleCaptureInterfaces)
	mux.HandleFunc("/api/capture/start", s.handleCaptureStart)
	mux.HandleFunc("/api/capture/stop", s.handleCaptureStop)

	mux.HandleFunc("/api/export/har", s.handleExportHAR)

	mux.HandleFunc("/api/metrics", s.handleMetrics)

	mux.HandleFunc("/ws", s.handleWebSocket)

	if s.frontend != nil {
		mux.Handle("/", s.frontend)
	}

	s.mux = mux
}

func (s *Server) Handler() http.Handler {
	h := securityHeadersMiddleware(corsMiddleware(recoveryMiddleware(requestIDMiddleware(requestIDInjectorMiddleware(s.mux)))))
	return rateLimitMiddleware(s.rateLimiter)(h)
}

func (s *Server) SetCaptureEngine(ce interface {
	IsRunning() bool
	Start() error
	Stop()
	GetMetrics() map[string]interface{}
}) {
	s.captureEngine = ce
}

func (s *Server) SetInterceptor(interceptor interface {
	GetMode() string
	SetMode(mode string)
	GetPending() []models.PendingRequest
	Resolve(id string, result models.InterceptResult) error
	SetRules(rules []models.InterceptRule)
}) {
	s.interceptor = interceptor
}

func (s *Server) BroadcastIntercept(req *models.PendingRequest) {
	s.hub.broadcastIntercept(req)
}

func (s *Server) BroadcastCapture(req *models.CapturedRequest) {
	s.hub.broadcast(req)
}

func (s *Server) BroadcastUpdate(req *models.CapturedRequest) {
	s.hub.BroadcastUpdate(req)
}

func (s *Server) Stop() {
	s.rateLimiter.stop()
	s.hub.Stop()
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
			"status": "error",
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

func (s *Server) handleListRequests(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAppError(w, ErrMethodNotAllowed())
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
		slog.Error("list requests failed", "error", err, "request_id", RequestIDFromContext(r.Context()))
		writeAppError(w, ErrInternal("Failed to list requests"))
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"data":  items,
		"total": total,
		"limit": limit,
	})
}

func (s *Server) handleRequestByID(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Path[len("/api/requests/"):]
	if idStr == "" {
		writeAppError(w, ErrValidation("Missing id"))
		return
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeAppError(w, ErrValidation("Invalid id"))
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getRequest(w, r, id)
	case http.MethodDelete:
		s.deleteRequest(w, r, id)
	default:
		writeAppError(w, ErrMethodNotAllowed())
	}
}

func (s *Server) getRequest(w http.ResponseWriter, r *http.Request, id int64) {
	req, err := s.store.Get(id)
	if err != nil {
		writeAppError(w, ErrNotFound("Request", strconv.FormatInt(id, 10)))
		return
	}
	writeJSON(w, http.StatusOK, req)
}

func (s *Server) deleteRequest(w http.ResponseWriter, r *http.Request, id int64) {
	if err := s.store.Delete(id); err != nil {
		slog.Error("delete request failed", "error", err, "request_id", RequestIDFromContext(r.Context()))
		writeAppError(w, ErrInternal("Failed to delete request"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleResend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAppError(w, ErrMethodNotAllowed())
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)

	var body models.ResendRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAppError(w, ErrValidation("Invalid JSON or body too large (max 10MB)"))
		return
	}

	if body.Method == "" {
		writeAppError(w, ErrValidation("Method is required"))
		return
	}
	if body.URL == "" {
		writeAppError(w, ErrValidation("URL is required"))
		return
	}

	result, err := s.resendSvc.Resend(&body)
	if err != nil {
		if appErr, ok := err.(*AppError); ok {
			slog.Warn("resend failed", "url", body.URL, "error", appErr.Message, "request_id", RequestIDFromContext(r.Context()))
			writeAppError(w, appErr)
		} else {
			slog.Warn("resend failed", "url", body.URL, "error", err, "request_id", RequestIDFromContext(r.Context()))
			writeAppError(w, ErrBadGateway(err.Error()))
		}
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAppError(w, ErrMethodNotAllowed())
		return
	}
	if err := s.store.Clear(); err != nil {
		slog.Error("clear failed", "error", err, "request_id", RequestIDFromContext(r.Context()))
		writeAppError(w, ErrInternal("Failed to clear"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	total, errs, totalSize, err := s.store.Stats()
	if err != nil {
		slog.Error("stats failed", "error", err, "request_id", RequestIDFromContext(r.Context()))
		writeAppError(w, ErrInternal("Failed to get stats"))
		return
	}
	if errs > 0 {
		slog.Warn("stats completed with errors", "errors", errs, "request_id", RequestIDFromContext(r.Context()))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total":      total,
		"errors":     errs,
		"total_size": totalSize,
	})
}

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
		writeAppError(w, ErrMethodNotAllowed())
		return
	}
	host := r.URL.Query().Get("host")
	if host == "" {
		writeAppError(w, ErrValidation("host required"))
		return
	}
	tree, err := s.store.GetAPIMap(host)
	if err != nil {
		slog.Error("apimap failed", "host", host, "error", err, "request_id", RequestIDFromContext(r.Context()))
		writeAppError(w, ErrInternal("Failed to build API map"))
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
		slog.Error("list hosts failed", "error", err, "request_id", RequestIDFromContext(r.Context()))
		writeAppError(w, ErrInternal("Failed to list hosts"))
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
		writeAppError(w, ErrMethodNotAllowed())
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)

	var body models.APINoteRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAppError(w, ErrValidation("Invalid JSON"))
		return
	}
	if body.Host == "" {
		writeAppError(w, ErrValidation("host is required"))
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
		slog.Error("save note failed", "error", err, "request_id", RequestIDFromContext(r.Context()))
		writeAppError(w, ErrInternal("Failed to save note"))
		return
	}
	note.ID = id
	writeJSON(w, http.StatusOK, note)
}

func (s *Server) handleAPINoteByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeAppError(w, ErrMethodNotAllowed())
		return
	}
	idStr := r.URL.Path[len("/api/apimap/notes/"):]
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeAppError(w, ErrValidation("Invalid id"))
		return
	}
	if err := s.store.DeleteAPINote(id); err != nil {
		slog.Error("delete note failed", "id", id, "error", err, "request_id", RequestIDFromContext(r.Context()))
		writeAppError(w, ErrInternal("Failed to delete note"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ========================================
// Intercept
// ========================================

func (s *Server) handleInterceptMode(w http.ResponseWriter, r *http.Request) {
	if s.interceptor == nil {
		writeAppError(w, ErrUnavailable("Interceptor not available"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]string{"mode": s.interceptor.GetMode()})
	case http.MethodPost:
		var body struct{ Mode string `json:"mode"` }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeAppError(w, ErrValidation("Invalid JSON"))
			return
		}
		if body.Mode != "auto" && body.Mode != "manual" {
			writeAppError(w, ErrValidation("mode must be auto or manual"))
			return
		}
		s.interceptor.SetMode(body.Mode)
		if err := s.store.SetSetting("intercept_mode", body.Mode); err != nil {
			slog.Warn("save intercept mode failed", "error", err)
		}
		writeJSON(w, http.StatusOK, map[string]string{"mode": body.Mode})
	default:
		writeAppError(w, ErrMethodNotAllowed())
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
		writeAppError(w, ErrMethodNotAllowed())
		return
	}
	if s.interceptor == nil {
		writeAppError(w, ErrUnavailable("Interceptor not available"))
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)

	var result models.InterceptResult
	if err := json.NewDecoder(r.Body).Decode(&result); err != nil {
		writeAppError(w, ErrValidation("Invalid JSON"))
		return
	}
	if err := s.interceptor.Resolve(result.RequestID, result); err != nil {
		writeAppError(w, ErrNotFound("Pending request", result.RequestID))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "resolved"})
}

func (s *Server) handleInterceptRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rules, err := s.store.ListRules()
		if err != nil {
			slog.Error("list rules failed", "error", err, "request_id", RequestIDFromContext(r.Context()))
			writeAppError(w, ErrInternal("Failed to list rules"))
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
			writeAppError(w, ErrValidation("Invalid JSON"))
			return
		}
		if rule.Pattern == "" || (rule.Action != "allow" && rule.Action != "block") {
			writeAppError(w, ErrValidation("pattern required, action must be allow or block"))
			return
		}
		id, err := s.store.SaveRule(&rule)
		if err != nil {
			slog.Error("save rule failed", "error", err, "request_id", RequestIDFromContext(r.Context()))
			writeAppError(w, ErrInternal("Failed to save rule"))
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
		writeAppError(w, ErrMethodNotAllowed())
	}
}

func (s *Server) handleInterceptRuleByID(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Path[len("/api/intercept/rules/"):]
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeAppError(w, ErrValidation("Invalid id"))
		return
	}
	switch r.Method {
	case http.MethodPut:
		var body struct{ Enabled bool `json:"enabled"` }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeAppError(w, ErrValidation("Invalid JSON"))
			return
		}
		if err := s.store.UpdateRule(id, body.Enabled); err != nil {
			slog.Error("update rule failed", "id", id, "error", err, "request_id", RequestIDFromContext(r.Context()))
			writeAppError(w, ErrInternal("Failed to update rule"))
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
			slog.Error("delete rule failed", "id", id, "error", err, "request_id", RequestIDFromContext(r.Context()))
			writeAppError(w, ErrInternal("Failed to delete rule"))
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
		writeAppError(w, ErrMethodNotAllowed())
	}
}

// ========================================
// 拦截日志
// ========================================

func (s *Server) handleInterceptLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAppError(w, ErrMethodNotAllowed())
		return
	}

	action := r.URL.Query().Get("action")
	since := r.URL.Query().Get("since")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	logs, total, err := s.store.ListInterceptLogs(action, since, limit, offset)
	if err != nil {
		slog.Error("list intercept logs failed", "error", err, "request_id", RequestIDFromContext(r.Context()))
		writeAppError(w, ErrInternal("Failed to list intercept logs"))
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"data":   logs,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
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
		writeAppError(w, ErrInternal(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, ifaces)
}

func (s *Server) handleCaptureStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAppError(w, ErrMethodNotAllowed())
		return
	}

	var body struct {
		Interface string `json:"interface"`
		BPF       string `json:"bpf"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		writeAppError(w, ErrValidation("Invalid JSON"))
		return
	}
	if body.Interface == "" {
		body.Interface = capture.DetectInterface()
	}
	if body.BPF == "" {
		body.BPF = "tcp"
	}

	if s.captureEngine != nil {
		s.captureEngine.Stop()
	}
	ce := capture.New(body.Interface, body.BPF, s.store, s)
	s.SetCaptureEngine(ce)

	if err := s.captureEngine.Start(); err != nil {
		slog.Error("capture start failed", "iface", body.Interface, "error", err, "request_id", RequestIDFromContext(r.Context()))
		writeAppError(w, ErrInternal(err.Error()))
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
		writeAppError(w, ErrMethodNotAllowed())
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

	har, err := s.harSvc.Export(limit)
	if err != nil {
		if appErr, ok := err.(*AppError); ok {
			writeAppError(w, appErr)
		} else {
			writeAppError(w, ErrInternal("Failed to export HAR"))
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", "attachment; filename=packetlab.har")
	json.NewEncoder(w).Encode(har)
}

// ========================================
// 监控指标
// ========================================

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	metrics := map[string]interface{}{
		"goroutines": runtime.NumGoroutine(),
		"memory": map[string]interface{}{
			"alloc_mb":    mem.Alloc / 1024 / 1024,
			"sys_mb":      mem.Sys / 1024 / 1024,
			"heap_mb":     mem.HeapInuse / 1024 / 1024,
			"gc_pause_ms": mem.PauseNs[(mem.NumGC+255)%256] / 1000000,
		},
	}
	if s.captureEngine != nil {
		metrics["capture"] = s.captureEngine.GetMetrics()
	}
	writeJSON(w, http.StatusOK, metrics)
}

// ========================================
// WebSocket
// ========================================

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		return isAllowedOrigin(origin)
	},
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("ws upgrade failed", "error", err)
		http.Error(w, "WebSocket upgrade failed", http.StatusBadRequest)
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
// Utilities
// ========================================

func FlattenHeaders(h http.Header) map[string]string {
	result := make(map[string]string)
	for k, v := range h {
		if len(v) > 0 {
			result[k] = v[0]
		}
	}
	return result
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	body, err := json.Marshal(data)
	if err != nil {
		slog.Warn("writeJSON marshal failed", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"code":"INTERNAL_ERROR","message":"internal error"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}
