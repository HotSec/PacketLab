package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"packetlab/internal/models"
	"packetlab/internal/store"
)

// helper: create test server with in-memory SQLite
func newTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, nil, false, nil)
}

// helper: execute HTTP request against server
func testRequest(t *testing.T, s *Server, method, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	handler := s.Handler()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func decodeJSON[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var result T
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	return result
}

// ========================================
// Health Checks
// ========================================

func TestHealthEndpoint(t *testing.T) {
	s := newTestServer(t)
	w := testRequest(t, s, "GET", "/health", "")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestReadyEndpoint(t *testing.T) {
	s := newTestServer(t)
	w := testRequest(t, s, "GET", "/ready", "")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

// ========================================
// CORS
// ========================================

func TestCORSPreflight(t *testing.T) {
	s := newTestServer(t)

	t.Run("localhost origin allowed", func(t *testing.T) {
		req := httptest.NewRequest("OPTIONS", "/api/requests", nil)
		req.Header.Set("Origin", "http://localhost:8080")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusNoContent {
			t.Errorf("expected 204 for OPTIONS, got %d", w.Code)
		}
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:8080" {
			t.Errorf("expected Access-Control-Allow-Origin: http://localhost:8080, got %q", got)
		}
	})

	t.Run("no origin returns empty", func(t *testing.T) {
		req := httptest.NewRequest("OPTIONS", "/api/requests", nil)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusNoContent {
			t.Errorf("expected 204 for OPTIONS, got %d", w.Code)
		}
	})

	t.Run("disallowed origin not echoed", func(t *testing.T) {
		req := httptest.NewRequest("OPTIONS", "/api/requests", nil)
		req.Header.Set("Origin", "http://evil.com")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusNoContent {
			t.Errorf("expected 204 for OPTIONS, got %d", w.Code)
		}
		if got := w.Header().Get("Access-Control-Allow-Origin"); got == "http://evil.com" {
			t.Error("disallowed origin should not be echoed")
		}
	})
}

// ========================================
// Security Headers
// ========================================

func TestSecurityHeaders(t *testing.T) {
	s := newTestServer(t)
	w := testRequest(t, s, "GET", "/health", "")
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("expected X-Content-Type-Options: nosniff")
	}
}

// ========================================
// Requests CRUD
// ========================================

func TestListRequestsDefault(t *testing.T) {
	s := newTestServer(t)
	w := testRequest(t, s, "GET", "/api/requests", "")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestListRequestsWithFilter(t *testing.T) {
	s := newTestServer(t)
	w := testRequest(t, s, "GET", "/api/requests?method=GET&limit=10", "")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestListRequestsErrorOnly(t *testing.T) {
	s := newTestServer(t)
	w := testRequest(t, s, "GET", "/api/requests?error_only=true", "")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestListRequestsInvalidLimit(t *testing.T) {
	s := newTestServer(t)
	// limit should be clamped
	w := testRequest(t, s, "GET", "/api/requests?limit=9999", "")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestGetRequestNotFound(t *testing.T) {
	s := newTestServer(t)
	w := testRequest(t, s, "GET", "/api/requests/99999", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestDeleteRequestInvalidID(t *testing.T) {
	s := newTestServer(t)
	w := testRequest(t, s, "DELETE", "/api/requests/abc", "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// ========================================
// Resend
// ========================================

func TestResendMissingMethod(t *testing.T) {
	s := newTestServer(t)
	w := testRequest(t, s, "POST", "/api/resend", `{"url":"https://example.com"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestResendMissingURL(t *testing.T) {
	s := newTestServer(t)
	w := testRequest(t, s, "POST", "/api/resend", `{"method":"GET"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// ========================================
// Clear
// ========================================

func TestClearSuccess(t *testing.T) {
	s := newTestServer(t)
	w := testRequest(t, s, "POST", "/api/clear", "")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

// ========================================
// Stats
// ========================================

func TestStatsEndpoint(t *testing.T) {
	s := newTestServer(t)
	w := testRequest(t, s, "GET", "/api/stats", "")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

// ========================================
// API Map
// ========================================

func TestAPIMapMissingHost(t *testing.T) {
	s := newTestServer(t)
	w := testRequest(t, s, "GET", "/api/apimap", "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestAPIMapNotesMissingHost(t *testing.T) {
	s := newTestServer(t)
	w := testRequest(t, s, "POST", "/api/apimap/notes", `{"path":"/api","method":"GET","note":"test"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// ========================================
// Intercept Mode
// ========================================

func TestInterceptModeNotReady(t *testing.T) {
	s := newTestServer(t)
	w := testRequest(t, s, "GET", "/api/intercept/mode", "")
	// interceptor not set → 503
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestInterceptModePostInvalid(t *testing.T) {
	s := newTestServer(t)
	w := testRequest(t, s, "POST", "/api/intercept/mode", `{"mode":"invalid"}`)
	// interceptor not set → 503
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

// ========================================
// Intercept Rules
// ========================================

func TestInterceptRulesList(t *testing.T) {
	s := newTestServer(t)
	w := testRequest(t, s, "GET", "/api/intercept/rules", "")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestInterceptRulesCreateMissingPattern(t *testing.T) {
	s := newTestServer(t)
	w := testRequest(t, s, "POST", "/api/intercept/rules", `{"action":"block"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestInterceptRulesCreateInvalidAction(t *testing.T) {
	s := newTestServer(t)
	w := testRequest(t, s, "POST", "/api/intercept/rules", `{"pattern":"*.example.com","action":"invalid"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestInterceptRuleByIDInvalid(t *testing.T) {
	s := newTestServer(t)
	w := testRequest(t, s, "DELETE", "/api/intercept/rules/abc", "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// ========================================
// Intercept Action
// ========================================

func TestInterceptActionNotReady(t *testing.T) {
	s := newTestServer(t)
	w := testRequest(t, s, "POST", "/api/intercept/action", `{"request_id":"req_1","action":"allow"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

// ========================================
// Capture Status
// ========================================

func TestCaptureStatusNotReady(t *testing.T) {
	s := newTestServer(t)
	w := testRequest(t, s, "GET", "/api/capture/status", "")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

// ========================================
// Export HAR
// ========================================

func TestExportHAR(t *testing.T) {
	s := newTestServer(t)
	w := testRequest(t, s, "GET", "/api/export/har", "")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

// ========================================
// Metrics
// ========================================

func TestMetricsEndpoint(t *testing.T) {
	s := newTestServer(t)
	w := testRequest(t, s, "GET", "/api/metrics", "")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

// ========================================
// Intercept Logs (P1 — handler test)
// ========================================

func TestInterceptLogsEndpoint(t *testing.T) {
	s := newTestServer(t)
	w := testRequest(t, s, "GET", "/api/intercept/logs", "")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := resp["data"]; !ok {
		t.Error("expected 'data' field in response")
	}
}

func TestInterceptLogsFilterByAction(t *testing.T) {
	s := newTestServer(t)
	w := testRequest(t, s, "GET", "/api/intercept/logs?action=drop&limit=10", "")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestInterceptLogsInvalidLimit(t *testing.T) {
	s := newTestServer(t)
	w := testRequest(t, s, "GET", "/api/intercept/logs?limit=-1", "")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

// ========================================
// Proxy Status
// ========================================

func TestProxyStatus(t *testing.T) {
	s := newTestServer(t)
	w := testRequest(t, s, "GET", "/api/proxy/status", "")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

// ========================================
// InterceptLog CRUD — with seeded data
// ========================================

func TestInterceptLogsWithData(t *testing.T) {
	s := newTestServer(t)

	// Seed data via store directly
	log := &models.InterceptLog{
		Action:        "drop",
		RequestURL:    "https://evil.com/phish",
		RequestMethod: "GET",
		RequestHost:   "evil.com",
		RulePattern:   "*.evil.com",
		Mode:          "auto",
	}
	if err := s.store.SaveInterceptLog(log); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	w := testRequest(t, s, "GET", "/api/intercept/logs?action=drop", "")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	data, ok := resp["data"].([]interface{})
	if !ok {
		t.Fatal("expected data array")
	}
	if len(data) != 1 {
		t.Errorf("expected 1 log, got %d", len(data))
	}
}

func TestInterceptLogsFilterByAllow(t *testing.T) {
	s := newTestServer(t)

	s.store.SaveInterceptLog(&models.InterceptLog{
		Action: "allow", RequestURL: "https://safe.com/", RequestMethod: "GET",
		RequestHost: "safe.com", Mode: "auto",
	})

	w := testRequest(t, s, "GET", "/api/intercept/logs?action=allow", "")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

// ========================================
// Request Detail — seed + get
// ========================================

func TestGetRequestAfterSave(t *testing.T) {
	s := newTestServer(t)

	id, err := s.store.Save(&models.CapturedRequest{
		Method: "GET", URL: "https://hello.com/", Host: "hello.com",
		Path: "/", Protocol: "HTTP/1.1",
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	w := testRequest(t, s, "GET", "/api/requests/"+fmt.Sprintf("%d", id), "")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestHandleListRequests_LimitClamp(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, 50},    // 0 → 默认 50
		{1, 1},
		{200, 200}, // 上限
		{201, 200}, // 超过 → clamp 200
		{1000, 200},
		{-1, 50},
	}
	for _, c := range cases {
		got := clampLimit(c.in)
		if got != c.want {
			t.Errorf("clampLimit(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// ========================================
// WebSocket Upgrade 失败处理 / WebSocket Upgrade failure handling
// ========================================

// TestHandleWebSocket_UpgradeFailure_NoDoubleWrite 验证：当 gorilla/websocket
// 的 Upgrade 失败时（此处通过未设置 Origin 头触发 s.upgrader.CheckOrigin 拒绝——
// isAllowedOrigin 对空 Origin 返回 false），handleWebSocket 不应再次调用 http.Error
// 写响应——因为 gorilla/websocket 在 CheckOrigin 失败时已通过 returnError →
// http.Error 写入 403 响应，再次写入会导致 superfluous WriteHeader 警告并产生重复响应体。
func TestHandleWebSocket_UpgradeFailure_NoDoubleWrite(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Sec-WebSocket-Version", "13")

	rr := httptest.NewRecorder()

	// 未设置 Origin 头，s.upgrader.CheckOrigin 调用 isAllowedOrigin("", nil) 返回 false，
	// gorilla/websocket 在 CheckOrigin 失败时已通过 returnError → http.Error 写入 403 响应。
	// 调用不应 panic。
	srv.handleWebSocket(rr, req)

	// Upgrade 失败时应已写入错误响应（Code != 0）
	if rr.Code == 0 {
		t.Fatalf("expected Upgrade to have written an error response, got Code=0")
	}

	// gorilla/websocket CheckOrigin 失败时写入 403；
	// 若 handleWebSocket 误再次调用 WriteHeader，会覆盖状态码导致回归。
	if rr.Code != http.StatusForbidden {
		t.Errorf("expected status %d from Upgrade's returnError, got %d", http.StatusForbidden, rr.Code)
	}

	// 修复后：handleWebSocket 不应再调用 http.Error，因此 body 不应包含
	// "WebSocket upgrade failed"（http.Error 写入的冗余消息）。
	body := rr.Body.String()
	if strings.Contains(body, "WebSocket upgrade failed") {
		t.Errorf("response body should not contain redundant http.Error message "+
			"(Upgrade already wrote the response), got: %q", body)
	}
}
