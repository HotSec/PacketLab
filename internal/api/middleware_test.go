package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRequestIDMiddlewareGeneratesID(t *testing.T) {
	handler := requestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := RequestIDFromContext(r.Context())
		if id == "" {
			t.Error("expected request ID in context")
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Header().Get("X-Request-ID") == "" {
		t.Error("expected X-Request-ID header")
	}
}

func TestRequestIDMiddlewarePreservesExisting(t *testing.T) {
	handler := requestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := RequestIDFromContext(r.Context())
		if id != "my-custom-id" {
			t.Errorf("expected my-custom-id, got %s", id)
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-Request-ID", "my-custom-id")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Header().Get("X-Request-ID") != "my-custom-id" {
		t.Errorf("expected X-Request-ID=my-custom-id, got %s", w.Header().Get("X-Request-ID"))
	}
}

func TestRequestIDFromContextEmpty(t *testing.T) {
	ctx := context.Background()
	if id := RequestIDFromContext(ctx); id != "" {
		t.Errorf("expected empty, got %s", id)
	}
}

func TestIsAllowedOrigin(t *testing.T) {
	tests := []struct {
		origin   string
		expected bool
	}{
		{"", false},
		{"http://localhost", true},
		{"http://localhost:8080", true},
		{"http://localhost:3001", true},
		{"http://127.0.0.1", true},
		{"http://127.0.0.1:9090", true},
		{"http://[::1]", true},
		{"http://[::1]:8080", true},
		{"http://evil.com", false},
		{"https://localhost", true}, // 本地开发常用 https，应放行
		{"https://localhost:8443", true},
		{"http://192.168.1.1", false},
		{"http://localhost.evil.com", false},
	}

	for _, tt := range tests {
		t.Run(tt.origin, func(t *testing.T) {
			if got := isAllowedOrigin(tt.origin, nil); got != tt.expected {
				t.Errorf("isAllowedOrigin(%q) = %v, want %v", tt.origin, got, tt.expected)
			}
		})
	}
}

func TestIsAllowedOrigin_EmptyRejected(t *testing.T) {
	if isAllowedOrigin("", nil) {
		t.Fatal("empty origin should be rejected")
	}
}

func TestIsAllowedOrigin_CustomWhitelist(t *testing.T) {
	allowed := []string{"https://my.app"}
	if !isAllowedOrigin("https://my.app", allowed) {
		t.Fatal("custom whitelist should allow")
	}
	if isAllowedOrigin("https://evil.com", allowed) {
		t.Fatal("non-whitelisted should reject")
	}
}

func TestIsAllowedOrigin_LocalhostDefault(t *testing.T) {
	if !isAllowedOrigin("http://localhost:9090", nil) {
		t.Fatal("localhost default should allow")
	}
	if !isAllowedOrigin("http://127.0.0.1:8080", nil) {
		t.Fatal("127.0.0.1 default should allow")
	}
}

func TestCORSMiddlewareAllowedOrigin(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := corsMiddleware(nil)(inner)

	req := httptest.NewRequest("GET", "/api/test", nil)
	req.Header.Set("Origin", "http://localhost:8080")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:8080" {
		t.Errorf("expected origin echoed, got %q", got)
	}
	if got := w.Header().Get("Vary"); got != "Origin" {
		t.Errorf("expected Vary=Origin, got %q", got)
	}
}

func TestCORSMiddlewareDisallowedOrigin(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := corsMiddleware(nil)(inner)

	req := httptest.NewRequest("GET", "/api/test", nil)
	req.Header.Set("Origin", "http://evil.com")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got == "http://evil.com" {
		t.Error("disallowed origin should not be echoed")
	}
}

func TestCORSMiddlewareOptions(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("OPTIONS should not reach handler")
	})
	handler := corsMiddleware(nil)(inner)

	req := httptest.NewRequest("OPTIONS", "/api/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d", w.Code)
	}
}

func TestSecurityHeadersMiddleware(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := securityHeadersMiddleware(inner)

	req := httptest.NewRequest("GET", "/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	expected := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"X-XSS-Protection":       "1; mode=block",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
	}
	for header, value := range expected {
		if got := w.Header().Get(header); got != value {
			t.Errorf("expected %s=%s, got %s", header, value, got)
		}
	}
}

func TestRateLimiterAllowsWithinLimit(t *testing.T) {
	rl := newRateLimiter(3, time.Minute)

	for i := 0; i < 3; i++ {
		if !rl.allow("key1") {
			t.Errorf("request %d should be allowed", i+1)
		}
	}
}

func TestRateLimiterBlocksOverLimit(t *testing.T) {
	rl := newRateLimiter(2, time.Minute)

	rl.allow("key1")
	rl.allow("key1")
	if rl.allow("key1") {
		t.Error("third request should be blocked")
	}
}

func TestRateLimiterDifferentKeys(t *testing.T) {
	rl := newRateLimiter(1, time.Minute)

	if !rl.allow("key1") {
		t.Error("key1 first request should be allowed")
	}
	if !rl.allow("key2") {
		t.Error("key2 first request should be allowed (independent)")
	}
}

func TestRateLimiterWindowReset(t *testing.T) {
	rl := newRateLimiter(1, 50*time.Millisecond)

	if !rl.allow("key1") {
		t.Error("first request should be allowed")
	}
	if rl.allow("key1") {
		t.Error("second request should be blocked within window")
	}

	time.Sleep(60 * time.Millisecond)

	if !rl.allow("key1") {
		t.Error("request after window reset should be allowed")
	}
}

func TestRateLimitMiddlewareBlocks(t *testing.T) {
	rl := newRateLimiter(1, time.Minute)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := rateLimitMiddleware(rl)(inner)

	req1 := httptest.NewRequest("GET", "/test", nil)
	req1.RemoteAddr = "1.2.3.4:1234"
	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Errorf("first request should succeed, got %d", w1.Code)
	}

	req2 := httptest.NewRequest("GET", "/test", nil)
	req2.RemoteAddr = "1.2.3.4:1234"
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)
	if w2.Code != http.StatusTooManyRequests {
		t.Errorf("second request should be rate limited, got %d", w2.Code)
	}
}

func TestRecoveryMiddlewareNoPanic(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := recoveryMiddleware(inner)

	req := httptest.NewRequest("GET", "/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestRecoveryMiddlewareCatchesPanic(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	})
	handler := requestIDMiddleware(recoveryMiddleware(inner))

	req := httptest.NewRequest("GET", "/test", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

func TestCSRFMiddlewareBlocksStateChangingWithoutHeader(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := csrfMiddleware(inner)

	// POST 无自定义头 → 403
	req := httptest.NewRequest("POST", "/api/resend", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for POST without X-Requested-With, got %d", w.Code)
	}

	// POST 带自定义头 → 放行
	req2 := httptest.NewRequest("POST", "/api/resend", nil)
	req2.Header.Set("X-Requested-With", "XMLHttpRequest")
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Errorf("expected 200 for POST with X-Requested-With, got %d", w2.Code)
	}

	// GET 无头 → 放行（读操作不受 CSRF 影响）
	req3 := httptest.NewRequest("GET", "/api/requests", nil)
	w3 := httptest.NewRecorder()
	handler.ServeHTTP(w3, req3)
	if w3.Code != http.StatusOK {
		t.Errorf("expected 200 for GET without header, got %d", w3.Code)
	}
}

func TestAuthMiddlewareDefaultTokenRequired(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := authMiddleware("test-secret-token")(inner)

	// 无 token → 401
	req := httptest.NewRequest("GET", "/api/requests", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without token, got %d", w.Code)
	}

	// 正确 Bearer token → 放行
	req2 := httptest.NewRequest("GET", "/api/requests", nil)
	req2.Header.Set("Authorization", "Bearer test-secret-token")
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Errorf("expected 200 with Bearer token, got %d", w2.Code)
	}

	// 查询参数 token → 放行（WS 握手用）
	req3 := httptest.NewRequest("GET", "/ws?token=test-secret-token", nil)
	w3 := httptest.NewRecorder()
	handler.ServeHTTP(w3, req3)
	if w3.Code != http.StatusOK {
		t.Errorf("expected 200 with query token, got %d", w3.Code)
	}

	// 错误 token → 401
	req4 := httptest.NewRequest("GET", "/api/requests", nil)
	req4.Header.Set("Authorization", "Bearer wrong")
	w4 := httptest.NewRecorder()
	handler.ServeHTTP(w4, req4)
	if w4.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 with wrong token, got %d", w4.Code)
	}
}
