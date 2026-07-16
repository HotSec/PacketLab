package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"packetlab/internal/models"
	"packetlab/internal/store"

	"github.com/gorilla/websocket"
)

func newIntegrationServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	dbPath := dir + "/integration.db"
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, nil, false, nil)
}

func integrationRequest(t *testing.T, s *Server, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	handler := s.Handler()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func decodeAPIError(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode API error: %v", err)
	}
	return resp
}

func assertAPIError(t *testing.T, w *httptest.ResponseRecorder, expectedStatus int, expectedCode string) {
	t.Helper()
	if w.Code != expectedStatus {
		t.Errorf("expected status %d, got %d; body: %s", expectedStatus, w.Code, w.Body.String())
		return
	}
	resp := decodeAPIError(t, w)
	if code, ok := resp["code"].(string); !ok || code != expectedCode {
		t.Errorf("expected code=%s, got %v", expectedCode, resp["code"])
	}
	if _, ok := resp["message"]; !ok {
		t.Error("expected 'message' field in error response")
	}
}

func TestIntegration_RequestIDInResponse(t *testing.T) {
	s := newIntegrationServer(t)
	w := integrationRequest(t, s, "GET", "/health", "", nil)
	if rid := w.Header().Get("X-Request-ID"); rid == "" {
		t.Error("expected X-Request-ID header in response")
	}
}

func TestIntegration_RequestIDPropagated(t *testing.T) {
	s := newIntegrationServer(t)
	w := integrationRequest(t, s, "GET", "/health", "", map[string]string{
		"X-Request-ID": "custom-req-42",
	})
	if rid := w.Header().Get("X-Request-ID"); rid != "custom-req-42" {
		t.Errorf("expected X-Request-ID=custom-req-42, got %s", rid)
	}
}

func TestIntegration_ErrorFormatValidation(t *testing.T) {
	s := newIntegrationServer(t)

	t.Run("404 has code+message", func(t *testing.T) {
		w := integrationRequest(t, s, "GET", "/api/requests/99999", "", nil)
		assertAPIError(t, w, 404, "NOT_FOUND")
	})

	t.Run("400 validation error", func(t *testing.T) {
		w := integrationRequest(t, s, "POST", "/api/resend", `{"url":"https://example.com"}`, nil)
		assertAPIError(t, w, 400, "VALIDATION_ERROR")
	})

	t.Run("400 invalid ID", func(t *testing.T) {
		w := integrationRequest(t, s, "DELETE", "/api/requests/abc", "", nil)
		assertAPIError(t, w, 400, "VALIDATION_ERROR")
	})

	t.Run("405 method not allowed", func(t *testing.T) {
		w := integrationRequest(t, s, "POST", "/api/requests", "", nil)
		assertAPIError(t, w, 405, "METHOD_NOT_ALLOWED")
	})

	t.Run("503 service unavailable", func(t *testing.T) {
		w := integrationRequest(t, s, "GET", "/api/intercept/mode", "", nil)
		assertAPIError(t, w, 503, "SERVICE_UNAVAILABLE")
	})
}

func TestIntegration_RequestsCRUDRoundTrip(t *testing.T) {
	s := newIntegrationServer(t)

	id, err := s.store.Save(&models.CapturedRequest{
		Method:     "GET",
		URL:        "https://api.test.com/endpoint",
		Host:       "api.test.com",
		Path:       "/endpoint",
		Protocol:   "HTTP/1.1",
		IsHTTPS:    true,
		ReqHeaders: map[string]string{"Authorization": "Bearer token"},
		ReqBody:    `{"query":"test"}`,
		StatusCode: 200,
		ResHeaders: map[string]string{"Content-Type": "application/json"},
		ResBody:    `{"result":"ok"}`,
		DurationMs: 42,
		SizeBytes:  128,
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	t.Run("list includes saved request", func(t *testing.T) {
		w := integrationRequest(t, s, "GET", "/api/requests?limit=10", "", nil)
		if w.Code != 200 {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		var resp map[string]interface{}
		json.NewDecoder(w.Body).Decode(&resp)
		if total, ok := resp["total"].(float64); !ok || int(total) < 1 {
			t.Errorf("expected total >= 1, got %v", resp["total"])
		}
	})

	t.Run("get by ID returns full detail", func(t *testing.T) {
		w := integrationRequest(t, s, "GET", fmt.Sprintf("/api/requests/%d", id), "", nil)
		if w.Code != 200 {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		var req models.CapturedRequest
		json.NewDecoder(w.Body).Decode(&req)
		if req.Method != "GET" {
			t.Errorf("expected GET, got %s", req.Method)
		}
		if req.ReqHeaders["Authorization"] != "Bearer token" {
			t.Errorf("expected Authorization header, got %v", req.ReqHeaders)
		}
		if req.ResBody != `{"result":"ok"}` {
			t.Errorf("unexpected ResBody: %s", req.ResBody)
		}
	})

	t.Run("delete removes request", func(t *testing.T) {
		w := integrationRequest(t, s, "DELETE", fmt.Sprintf("/api/requests/%d", id), "", nil)
		if w.Code != 200 {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		w2 := integrationRequest(t, s, "GET", fmt.Sprintf("/api/requests/%d", id), "", nil)
		assertAPIError(t, w2, 404, "NOT_FOUND")
	})
}

func TestIntegration_ClearRemovesAll(t *testing.T) {
	s := newIntegrationServer(t)

	for i := 0; i < 5; i++ {
		s.store.Save(&models.CapturedRequest{
			Method: "GET", URL: "https://example.com/", Host: "example.com",
			Path: "/", Protocol: "HTTP/1.1",
		})
	}

	w := integrationRequest(t, s, "POST", "/api/clear", "", nil)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	w2 := integrationRequest(t, s, "GET", "/api/stats", "", nil)
	var stats map[string]interface{}
	json.NewDecoder(w2.Body).Decode(&stats)
	if total, ok := stats["total"].(float64); !ok || int(total) != 0 {
		t.Errorf("expected total=0 after clear, got %v", stats["total"])
	}
}

func TestIntegration_StatsReflectData(t *testing.T) {
	s := newIntegrationServer(t)

	s.store.Save(&models.CapturedRequest{
		Method: "GET", URL: "https://ok.com/", Host: "ok.com",
		Path: "/", Protocol: "HTTP/1.1", StatusCode: 200, SizeBytes: 100,
	})
	s.store.Save(&models.CapturedRequest{
		Method: "GET", URL: "https://err.com/", Host: "err.com",
		Path: "/", Protocol: "HTTP/1.1", StatusCode: 500, SizeBytes: 50,
	})

	w := integrationRequest(t, s, "GET", "/api/stats", "", nil)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var stats map[string]interface{}
	json.NewDecoder(w.Body).Decode(&stats)
	if total := int(stats["total"].(float64)); total != 2 {
		t.Errorf("expected total=2, got %d", total)
	}
	if errs := int(stats["errors"].(float64)); errs != 1 {
		t.Errorf("expected errors=1, got %d", errs)
	}
}

func TestIntegration_APIMapWithHost(t *testing.T) {
	s := newIntegrationServer(t)

	s.store.Save(&models.CapturedRequest{
		Method: "GET", URL: "https://api.test.com/users", Host: "api.test.com",
		Path: "/users", Protocol: "HTTP/1.1", StatusCode: 200,
	})
	s.store.Save(&models.CapturedRequest{
		Method: "POST", URL: "https://api.test.com/users", Host: "api.test.com",
		Path: "/users", Protocol: "HTTP/1.1", StatusCode: 201,
	})

	w := integrationRequest(t, s, "GET", "/api/apimap?host=api.test.com", "", nil)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestIntegration_APINotesCRUD(t *testing.T) {
	s := newIntegrationServer(t)

	w := integrationRequest(t, s, "POST", "/api/apimap/notes", `{"host":"test.com","path":"/api","method":"GET","note":"test note"}`, nil)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var note models.APINote
	json.NewDecoder(w.Body).Decode(&note)
	if note.Host != "test.com" {
		t.Errorf("expected host=test.com, got %s", note.Host)
	}
	if note.ID == 0 {
		t.Error("expected ID > 0")
	}

	w2 := integrationRequest(t, s, "DELETE", fmt.Sprintf("/api/apimap/notes/%d", note.ID), "", nil)
	if w2.Code != 200 {
		t.Errorf("expected 200 on delete, got %d", w2.Code)
	}
}

func TestIntegration_InterceptRulesCRUD(t *testing.T) {
	s := newIntegrationServer(t)

	w := integrationRequest(t, s, "POST", "/api/intercept/rules", `{"pattern":"*.block.com","action":"block"}`, nil)
	if w.Code != 201 {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var rule models.InterceptRule
	json.NewDecoder(w.Body).Decode(&rule)
	if rule.Pattern != "*.block.com" {
		t.Errorf("expected pattern=*.block.com, got %s", rule.Pattern)
	}

	w2 := integrationRequest(t, s, "PUT", fmt.Sprintf("/api/intercept/rules/%d", rule.ID), `{"enabled":false}`, nil)
	if w2.Code != 200 {
		t.Errorf("expected 200 on update, got %d", w2.Code)
	}

	w3 := integrationRequest(t, s, "DELETE", fmt.Sprintf("/api/intercept/rules/%d", rule.ID), "", nil)
	if w3.Code != 200 {
		t.Errorf("expected 200 on delete, got %d", w3.Code)
	}
}

func TestIntegration_InterceptLogsFilter(t *testing.T) {
	s := newIntegrationServer(t)

	s.store.SaveInterceptLog(&models.InterceptLog{
		Action: "allow", RequestURL: "https://safe.com/", RequestMethod: "GET",
		RequestHost: "safe.com", Mode: "auto",
	})
	s.store.SaveInterceptLog(&models.InterceptLog{
		Action: "drop", RequestURL: "https://evil.com/", RequestMethod: "GET",
		RequestHost: "evil.com", RulePattern: "*.evil.com", Mode: "auto",
	})

	w := integrationRequest(t, s, "GET", "/api/intercept/logs?action=drop", "", nil)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	data := resp["data"].([]interface{})
	if len(data) != 1 {
		t.Errorf("expected 1 drop log, got %d", len(data))
	}
}

func TestIntegration_ExportHARWithContent(t *testing.T) {
	s := newIntegrationServer(t)

	s.store.Save(&models.CapturedRequest{
		Method: "GET", URL: "https://har-test.com/api", Host: "har-test.com",
		Path: "/api", Protocol: "HTTP/1.1", StatusCode: 200,
		ReqHeaders: map[string]string{"Accept": "application/json"},
		ResHeaders: map[string]string{"Content-Type": "application/json"},
		ResBody:    `{"data":1}`, DurationMs: 50, SizeBytes: 10,
	})

	w := integrationRequest(t, s, "GET", "/api/export/har", "", nil)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected application/json, got %s", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "packetlab.har") {
		t.Errorf("expected Content-Disposition with filename, got %s", cd)
	}

	var har map[string]interface{}
	json.NewDecoder(w.Body).Decode(&har)
	logMap := har["log"].(map[string]interface{})
	entries := logMap["entries"].([]interface{})
	if len(entries) != 1 {
		t.Errorf("expected 1 HAR entry, got %d", len(entries))
	}
}

func TestIntegration_MetricsEndpoint(t *testing.T) {
	s := newIntegrationServer(t)
	w := integrationRequest(t, s, "GET", "/api/metrics", "", nil)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var metrics map[string]interface{}
	json.NewDecoder(w.Body).Decode(&metrics)
	if _, ok := metrics["goroutines"]; !ok {
		t.Error("expected goroutines field")
	}
	if mem, ok := metrics["memory"].(map[string]interface{}); !ok {
		t.Error("expected memory map")
	} else {
		if _, ok := mem["alloc_mb"]; !ok {
			t.Error("expected alloc_mb in memory")
		}
	}
}

func TestIntegration_ProxyStatus(t *testing.T) {
	s := newIntegrationServer(t)
	w := integrationRequest(t, s, "GET", "/api/proxy/status", "", nil)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["running"] != true {
		t.Error("expected running=true")
	}
}

func TestIntegration_CaptureStatusNoEngine(t *testing.T) {
	s := newIntegrationServer(t)
	w := integrationRequest(t, s, "GET", "/api/capture/status", "", nil)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["running"] != false {
		t.Error("expected running=false")
	}
	if resp["available"] != false {
		t.Error("expected available=false")
	}
}

func TestIntegration_CaptureStartStopMethodCheck(t *testing.T) {
	s := newIntegrationServer(t)

	w := integrationRequest(t, s, "GET", "/api/capture/start", "", nil)
	assertAPIError(t, w, 405, "METHOD_NOT_ALLOWED")

	w2 := integrationRequest(t, s, "GET", "/api/capture/stop", "", nil)
	assertAPIError(t, w2, 405, "METHOD_NOT_ALLOWED")
}

func TestIntegration_ReadyEndpointDBCheck(t *testing.T) {
	s := newIntegrationServer(t)
	w := integrationRequest(t, s, "GET", "/ready", "", nil)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "ok" {
		t.Errorf("expected status=ok, got %v", resp["status"])
	}
}

func TestIntegration_ResendInvalidJSON(t *testing.T) {
	s := newIntegrationServer(t)
	w := integrationRequest(t, s, "POST", "/api/resend", "not json", nil)
	assertAPIError(t, w, 400, "VALIDATION_ERROR")
}

func TestIntegration_APIHostsEndpoint(t *testing.T) {
	s := newIntegrationServer(t)

	s.store.Save(&models.CapturedRequest{Method: "GET", URL: "https://a.com/", Host: "a.com", Path: "/", Protocol: "HTTP/1.1"})
	s.store.Save(&models.CapturedRequest{Method: "GET", URL: "https://b.com/", Host: "b.com", Path: "/", Protocol: "HTTP/1.1"})

	w := integrationRequest(t, s, "GET", "/api/apimap/hosts", "", nil)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	data, ok := resp["data"].([]interface{})
	if !ok {
		t.Fatal("expected data array")
	}
	if len(data) < 2 {
		t.Errorf("expected at least 2 hosts, got %d", len(data))
	}
}

func TestIntegration_InterceptActionNotReady(t *testing.T) {
	s := newIntegrationServer(t)
	w := integrationRequest(t, s, "POST", "/api/intercept/action", `{"request_id":"req_1","action":"allow"}`, nil)
	assertAPIError(t, w, 503, "SERVICE_UNAVAILABLE")
}

func TestIntegration_InterceptPendingEmpty(t *testing.T) {
	s := newIntegrationServer(t)
	w := integrationRequest(t, s, "GET", "/api/intercept/pending", "", nil)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestIntegration_CORSHeadersOnGET(t *testing.T) {
	s := newIntegrationServer(t)
	w := integrationRequest(t, s, "GET", "/health", "", map[string]string{
		"Origin": "http://localhost:3000",
	})
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Errorf("expected CORS origin echoed, got %q", got)
	}
}

func TestIntegration_SecurityHeadersPresent(t *testing.T) {
	s := newIntegrationServer(t)
	w := integrationRequest(t, s, "GET", "/health", "", nil)

	checks := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"X-XSS-Protection":       "1; mode=block",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
	}
	for header, expected := range checks {
		if got := w.Header().Get(header); got != expected {
			t.Errorf("expected %s=%s, got %s", header, expected, got)
		}
	}
}

func TestIntegration_PanicRecovery(t *testing.T) {
	s := newIntegrationServer(t)
	handler := s.Handler()

	panicHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("integration test panic")
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/test-panic", panicHandler)
	wrapped := securityHeadersMiddleware(corsMiddleware(nil)(recoveryMiddleware(requestIDMiddleware(mux))))

	req := httptest.NewRequest("GET", "/test-panic", nil)
	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, req)

	if w.Code != 500 {
		t.Errorf("expected 500 after panic, got %d", w.Code)
	}
	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["code"] != "INTERNAL_ERROR" {
		t.Errorf("expected INTERNAL_ERROR, got %v", resp["code"])
	}

	_ = handler
}

func TestIntegration_RequestIDInErrorResponse(t *testing.T) {
	s := newIntegrationServer(t)
	w := integrationRequest(t, s, "GET", "/api/requests/99999", "", map[string]string{
		"X-Request-ID": "trace-abc-123",
	})
	rid := w.Header().Get("X-Request-ID")
	if rid != "trace-abc-123" {
		t.Errorf("expected X-Request-ID=trace-abc-123, got %s", rid)
	}

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["request_id"] != "trace-abc-123" {
		t.Errorf("expected request_id in body, got %v", resp["request_id"])
	}
}

func TestIntegration_ListRequestsWithHostFilter(t *testing.T) {
	s := newIntegrationServer(t)

	s.store.Save(&models.CapturedRequest{Method: "GET", URL: "https://target.com/", Host: "target.com", Path: "/", Protocol: "HTTP/1.1"})
	s.store.Save(&models.CapturedRequest{Method: "GET", URL: "https://other.com/", Host: "other.com", Path: "/", Protocol: "HTTP/1.1"})

	w := integrationRequest(t, s, "GET", "/api/requests?host=target.com", "", nil)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	if total := int(resp["total"].(float64)); total != 1 {
		t.Errorf("expected total=1 for host filter, got %d", total)
	}
}

func TestIntegration_ResendWithMockServer(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Response-Header", "value")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"mock":true}`))
	}))
	defer mockServer.Close()

	s := newIntegrationServer(t)
	w := integrationRequest(t, s, "POST", "/api/resend", fmt.Sprintf(
		`{"method":"GET","url":"%s/api","headers":{"X-Test":"1"}}`, mockServer.URL,
	), nil)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var result ResendResult
	json.NewDecoder(w.Body).Decode(&result)
	if result.StatusCode != 200 {
		t.Errorf("expected status_code=200, got %d", result.StatusCode)
	}
	if result.ResBody != `{"mock":true}` {
		t.Errorf("unexpected res_body: %s", result.ResBody)
	}
	if result.ID == 0 {
		t.Error("expected ID > 0 (saved to store)")
	}
	if result.DurationMs < 0 {
		t.Errorf("expected non-negative duration, got %d", result.DurationMs)
	}
}

func TestIntegration_APINoteMissingHostReturns400(t *testing.T) {
	s := newIntegrationServer(t)
	w := integrationRequest(t, s, "POST", "/api/apimap/notes", `{"path":"/api","method":"GET","note":"no host"}`, nil)
	assertAPIError(t, w, 400, "VALIDATION_ERROR")
}

func TestIntegration_APIMapMissingHostReturns400(t *testing.T) {
	s := newIntegrationServer(t)
	w := integrationRequest(t, s, "GET", "/api/apimap", "", nil)
	assertAPIError(t, w, 400, "VALIDATION_ERROR")
}

func TestIntegration_InterceptRulesInvalidJSON(t *testing.T) {
	s := newIntegrationServer(t)
	w := integrationRequest(t, s, "POST", "/api/intercept/rules", "not json", nil)
	assertAPIError(t, w, 400, "VALIDATION_ERROR")
}

func TestIntegration_InterceptLogsPagination(t *testing.T) {
	s := newIntegrationServer(t)

	for i := 0; i < 5; i++ {
		s.store.SaveInterceptLog(&models.InterceptLog{
			Action: "allow", RequestURL: "https://example.com/",
			RequestMethod: "GET", RequestHost: "example.com", Mode: "auto",
		})
	}

	w := integrationRequest(t, s, "GET", "/api/intercept/logs?limit=2&offset=0", "", nil)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	data := resp["data"].([]interface{})
	if len(data) != 2 {
		t.Errorf("expected 2 logs with limit=2, got %d", len(data))
	}
	if total := int(resp["total"].(float64)); total != 5 {
		t.Errorf("expected total=5, got %d", total)
	}
}

func TestIntegration_WebSocketBroadcast(t *testing.T) {
	s := newIntegrationServer(t)

	server := httptest.NewServer(s.Handler())
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Skipf("websocket dial failed: %v", err)
	}
	defer ws.Close()

	ws.SetReadDeadline(time.Now().Add(3 * time.Second))

	s.store.Save(&models.CapturedRequest{
		Method: "GET", URL: "https://ws-test.com/", Host: "ws-test.com",
		Path: "/", Protocol: "HTTP/1.1", StatusCode: 200,
	})
	s.BroadcastCapture(&models.CapturedRequest{
		Method: "GET", URL: "https://ws-test.com/broadcast", Host: "ws-test.com",
		Path: "/broadcast", Protocol: "HTTP/1.1", StatusCode: 200,
	})

	_, msg, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read ws message: %v", err)
	}

	var payload map[string]interface{}
	json.Unmarshal(msg, &payload)
	if payload["type"] != "new_request" {
		t.Errorf("expected type=new_request, got %v", payload["type"])
	}
}
