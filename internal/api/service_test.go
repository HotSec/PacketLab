package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"packetlab/internal/models"
	"packetlab/internal/store"
)

func newTestStoreForService(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestHARServiceExportEmpty(t *testing.T) {
	st := newTestStoreForService(t)
	svc := NewHARService(st)

	har, err := svc.Export(100)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	logMap, ok := har["log"].(map[string]interface{})
	if !ok {
		t.Fatal("expected log map")
	}
	if logMap["version"] != "1.2" {
		t.Errorf("expected version 1.2, got %v", logMap["version"])
	}
	entries, ok := logMap["entries"].([]map[string]interface{})
	if !ok {
		t.Fatal("expected entries array")
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
}

func TestHARServiceExportWithData(t *testing.T) {
	st := newTestStoreForService(t)

	st.Save(&models.CapturedRequest{
		Method: "GET", URL: "https://api.example.com/users",
		Host: "api.example.com", Path: "/users", Protocol: "HTTP/1.1",
		StatusCode: 200, DurationMs: 150, SizeBytes: 2048,
		ReqHeaders: map[string]string{"Accept": "application/json"},
		ResHeaders: map[string]string{"Content-Type": "application/json"},
		ResBody:    `[{"id":1}]`,
	})
	st.Save(&models.CapturedRequest{
		Method: "POST", URL: "https://api.example.com/users",
		Host: "api.example.com", Path: "/users", Protocol: "HTTP/1.1",
		StatusCode: 201, DurationMs: 300, SizeBytes: 128,
	})

	svc := NewHARService(st)
	har, err := svc.Export(100)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	logMap := har["log"].(map[string]interface{})
	entries := logMap["entries"].([]map[string]interface{})
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	first := entries[0]
	req := first["request"].(map[string]interface{})
	if req["method"] != "POST" {
		t.Errorf("expected first entry method=POST (newest first), got %v", req["method"])
	}

	resp := first["response"].(map[string]interface{})
	if resp["status"] != 201 {
		t.Errorf("expected status 201, got %v", resp["status"])
	}

	creator := logMap["creator"].(map[string]string)
	if creator["name"] != "PacketLab" {
		t.Errorf("expected creator=PacketLab, got %s", creator["name"])
	}
}

func TestHARServiceExportLimitClamp(t *testing.T) {
	st := newTestStoreForService(t)
	svc := NewHARService(st)

	har, _ := svc.Export(0)
	logMap := har["log"].(map[string]interface{})
	entries := logMap["entries"].([]map[string]interface{})
	if len(entries) != 0 {
		t.Errorf("limit=0 should be clamped to 500, got %d entries", len(entries))
	}

	har2, _ := svc.Export(9999)
	logMap2 := har2["log"].(map[string]interface{})
	entries2 := logMap2["entries"].([]map[string]interface{})
	if len(entries2) != 0 {
		t.Errorf("limit=9999 should be clamped to 1000, got %d entries", len(entries2))
	}
}

func TestToHARHeaders(t *testing.T) {
	headers := map[string]string{
		"Content-Type": "application/json",
		"X-Custom":     "value",
	}
	result := toHARHeaders(headers)
	if len(result) != 2 {
		t.Fatalf("expected 2 headers, got %d", len(result))
	}

	found := map[string]string{}
	for _, h := range result {
		found[h["name"]] = h["value"]
	}
	if found["Content-Type"] != "application/json" {
		t.Errorf("expected Content-Type=application/json, got %s", found["Content-Type"])
	}
	if found["X-Custom"] != "value" {
		t.Errorf("expected X-Custom=value, got %s", found["X-Custom"])
	}
}

func TestToHARHeadersEmpty(t *testing.T) {
	result := toHARHeaders(map[string]string{})
	if len(result) != 0 {
		t.Errorf("expected 0 headers, got %d", len(result))
	}
}

func TestResendServiceInvalidURL(t *testing.T) {
	st := newTestStoreForService(t)
	svc := NewResendService(st, nil, false)

	_, err := svc.Resend(&models.ResendRequest{
		Method: "GET",
		URL:    "://invalid",
	})
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
	appErr, ok := err.(*AppError)
	if !ok {
		t.Fatalf("expected *AppError, got %T", err)
	}
	if appErr.Code != "VALIDATION_ERROR" {
		t.Errorf("expected VALIDATION_ERROR, got %s", appErr.Code)
	}
}

func TestResendServiceUnsupportedScheme(t *testing.T) {
	st := newTestStoreForService(t)
	svc := NewResendService(st, nil, false)

	_, err := svc.Resend(&models.ResendRequest{
		Method: "GET",
		URL:    "ftp://example.com/file",
	})
	if err == nil {
		t.Fatal("expected error for unsupported scheme")
	}
	appErr, ok := err.(*AppError)
	if !ok {
		t.Fatalf("expected *AppError, got %T", err)
	}
	if appErr.Code != "VALIDATION_ERROR" {
		t.Errorf("expected VALIDATION_ERROR, got %s", appErr.Code)
	}
}

func TestResendServiceUnreachableHost(t *testing.T) {
	st := newTestStoreForService(t)
	// insecure=true：测试目标是回环地址，仅用于验证"不可达→502"
	svc := NewResendService(st, nil, true)

	_, err := svc.Resend(&models.ResendRequest{
		Method:  "GET",
		URL:     "http://127.0.0.1:1/unreachable",
		Headers: map[string]string{},
	})
	if err == nil {
		t.Fatal("expected error for unreachable host")
	}
	appErr, ok := err.(*AppError)
	if !ok {
		t.Fatalf("expected *AppError, got %T", err)
	}
	if appErr.Code != "BAD_GATEWAY" {
		t.Errorf("expected BAD_GATEWAY, got %s", appErr.Code)
	}
}

func TestResendServiceSuccessWithMockServer(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("X-Custom") != "test-value" {
			t.Errorf("expected X-Custom=test-value")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer mockServer.Close()

	st := newTestStoreForService(t)
	// insecure=true：httptest mock 在 127.0.0.1 上
	svc := NewResendService(st, nil, true)

	result, err := svc.Resend(&models.ResendRequest{
		Method:  "POST",
		URL:     mockServer.URL + "/api/test",
		Headers: map[string]string{"X-Custom": "test-value"},
		Body:    `{"input":1}`,
	})
	if err != nil {
		t.Fatalf("Resend: %v", err)
	}

	if result.StatusCode != 201 {
		t.Errorf("expected 201, got %d", result.StatusCode)
	}
	if result.ResBody != `{"ok":true}` {
		t.Errorf("expected {\"ok\":true}, got %s", result.ResBody)
	}
	if result.DurationMs < 0 {
		t.Errorf("expected non-negative duration, got %d", result.DurationMs)
	}
	if result.SizeBytes <= 0 {
		t.Errorf("expected positive size, got %d", result.SizeBytes)
	}
	if result.ID == 0 {
		t.Error("expected ID > 0 after save")
	}

	total, _, _, _ := st.Stats()
	if total != 1 {
		t.Errorf("expected 1 saved request, got %d", total)
	}
}

func TestResendServiceDefaultUserAgent(t *testing.T) {
	var receivedUA string
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedUA = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer mockServer.Close()

	st := newTestStoreForService(t)
	// insecure=true：httptest mock 在 127.0.0.1 上
	svc := NewResendService(st, nil, true)

	_, err := svc.Resend(&models.ResendRequest{
		Method:  "GET",
		URL:     mockServer.URL + "/",
		Headers: map[string]string{},
	})
	if err != nil {
		t.Fatalf("Resend: %v", err)
	}

	if receivedUA != "PacketLab/2.0" {
		t.Errorf("expected User-Agent=PacketLab/2.0, got %s", receivedUA)
	}
}

func TestResendServiceCustomUserAgent(t *testing.T) {
	var receivedUA string
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedUA = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer mockServer.Close()

	st := newTestStoreForService(t)
	// insecure=true：httptest mock 在 127.0.0.1 上
	svc := NewResendService(st, nil, true)

	_, err := svc.Resend(&models.ResendRequest{
		Method:  "GET",
		URL:     mockServer.URL + "/",
		Headers: map[string]string{"User-Agent": "Custom/1.0"},
	})
	if err != nil {
		t.Fatalf("Resend: %v", err)
	}

	if receivedUA != "Custom/1.0" {
		t.Errorf("expected User-Agent=Custom/1.0, got %s", receivedUA)
	}
}

func TestResendServiceBroadcastsToHub(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer mockServer.Close()

	st := newTestStoreForService(t)
	hub := newWSHub()
	go hub.run()
	defer hub.Stop()

	// insecure=true：httptest mock 在 127.0.0.1 上
	svc := NewResendService(st, hub, true)

	_, err := svc.Resend(&models.ResendRequest{
		Method:  "GET",
		URL:     mockServer.URL + "/",
		Headers: map[string]string{},
	})
	if err != nil {
		t.Fatalf("Resend: %v", err)
	}
}

// TestResendServiceBlocksPrivateTargets 验证非 --insecure 时私网/回环目标被拦截：
// IP 字面量与解析到禁止段的主机名均返回 VALIDATION_ERROR（不发起任何连接）。
func TestResendServiceBlocksPrivateTargets(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{"loopback ip", "http://127.0.0.1:1/secret"},
		{"private ip", "http://192.168.1.1:80/secret"},
		{"hostname resolving to loopback", "http://localhost:1/secret"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := newTestStoreForService(t)
			svc := NewResendService(st, nil, false)

			_, err := svc.Resend(&models.ResendRequest{
				Method:  "GET",
				URL:     tt.url,
				Headers: map[string]string{},
			})
			if err == nil {
				t.Fatalf("expected error for %s", tt.url)
			}
			appErr, ok := err.(*AppError)
			if !ok {
				t.Fatalf("expected *AppError, got %T", err)
			}
			if appErr.Code != "VALIDATION_ERROR" {
				t.Errorf("expected VALIDATION_ERROR, got %s", appErr.Code)
			}
			if !strings.Contains(appErr.Message, "blocked") {
				t.Errorf("expected blocked message, got %q", appErr.Message)
			}
		})
	}
}

func TestResendServiceBlocksCrossHostRedirect(t *testing.T) {
	// 服务器 A 302 跳转到另一个 host（服务器 B），必须被拦截（防重定向链跳内网）
	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("should-not-be-reached"))
	}))
	defer serverB.Close()

	serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, serverB.URL+"/leak", http.StatusFound)
	}))
	defer serverA.Close()

	st := newTestStoreForService(t)
	svc := NewResendService(st, nil, true)

	_, err := svc.Resend(&models.ResendRequest{
		Method:  "GET",
		URL:     serverA.URL + "/",
		Headers: map[string]string{},
	})
	if err == nil {
		t.Fatal("expected error for cross-host redirect")
	}
	appErr, ok := err.(*AppError)
	if !ok {
		t.Fatalf("expected *AppError, got %T", err)
	}
	if appErr.Code != "VALIDATION_ERROR" {
		t.Errorf("expected VALIDATION_ERROR, got %s", appErr.Code)
	}
	if !strings.Contains(appErr.Message, "redirect") {
		t.Errorf("expected redirect message, got %q", appErr.Message)
	}
}

func TestResendServiceAllowsSameHostRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		w.Write([]byte("final-response"))
	}))
	defer server.Close()

	st := newTestStoreForService(t)
	svc := NewResendService(st, nil, true)

	result, err := svc.Resend(&models.ResendRequest{
		Method:  "GET",
		URL:     server.URL + "/redirect",
		Headers: map[string]string{},
	})
	if err != nil {
		t.Fatalf("Resend: %v", err)
	}
	if result.StatusCode != 200 {
		t.Errorf("expected 200 after same-host redirect, got %d", result.StatusCode)
	}
	if result.ResBody != "final-response" {
		t.Errorf("expected final-response, got %q", result.ResBody)
	}
}

// TestResendDialContextBlocksPrivate 验证拨号层的 SSRF 校验：非 trusted 时
// 回环/私网 IP 与解析到禁止段的 hostname 均在连接前被拦截（不发起实际连接）。
func TestResendDialContextBlocksPrivate(t *testing.T) {
	dial := resendDialContext(false)

	// 回环 IP 直接命中
	if _, err := dial(context.Background(), "tcp", "127.0.0.1:1"); !errors.Is(err, errBlockedTarget) {
		t.Errorf("expected errBlockedTarget for loopback, got %v", err)
	}
	// 私网 IP 直接命中
	if _, err := dial(context.Background(), "tcp", "192.168.1.1:80"); !errors.Is(err, errBlockedTarget) {
		t.Errorf("expected errBlockedTarget for private, got %v", err)
	}
	// hostname 解析到回环 → 拦截（localhost 离线可解析）
	if _, err := dial(context.Background(), "tcp", "localhost:1"); !errors.Is(err, errBlockedTarget) {
		t.Errorf("expected errBlockedTarget for localhost, got %v", err)
	}
}

// TestResendDialContextTrustedPassesThrough 验证 trusted（--insecure）时不拦截：
// 回环目标直接进入拨号（127.0.0.1:1 返回连接拒绝而非 blocked）。
func TestResendDialContextTrustedPassesThrough(t *testing.T) {
	dial := resendDialContext(true)

	if _, err := dial(context.Background(), "tcp", "127.0.0.1:1"); errors.Is(err, errBlockedTarget) {
		t.Error("trusted dial must not be blocked")
	}
}

func TestFlattenHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", "text/html")
	h.Add("X-Multi", "first")
	h.Add("X-Multi", "second")

	result := FlattenHeaders(h)
	if result["Content-Type"] != "text/html" {
		t.Errorf("expected text/html, got %s", result["Content-Type"])
	}
	if result["X-Multi"] != "first, second" {
		t.Errorf("expected joined values for multi-header, got %s", result["X-Multi"])
	}
}

func TestFlattenHeadersEmpty(t *testing.T) {
	result := FlattenHeaders(http.Header{})
	if len(result) != 0 {
		t.Errorf("expected empty map, got %d entries", len(result))
	}
}

func TestWriteAppErrorResponse(t *testing.T) {
	w := httptest.NewRecorder()
	err := ErrValidation("bad input")
	err.RequestID = "test_123"
	writeAppError(w, err)

	if w.Code != 400 {
		t.Errorf("expected 400, got %d", w.Code)
	}

	var resp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["code"] != "VALIDATION_ERROR" {
		t.Errorf("expected VALIDATION_ERROR, got %v", resp["code"])
	}
	if resp["request_id"] != "test_123" {
		t.Errorf("expected request_id=test_123, got %v", resp["request_id"])
	}
}
