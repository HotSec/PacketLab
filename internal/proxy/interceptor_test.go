package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"packetlab/internal/models"
)

// ========================================
// NewInterceptor
// ========================================

func TestNewInterceptorDefaultMode(t *testing.T) {
	it := NewInterceptor(15, nil, nil)
	if it.GetMode() != "auto" {
		t.Errorf("expected mode=auto, got %s", it.GetMode())
	}
}

func TestNewInterceptorTimeout(t *testing.T) {
	it := NewInterceptor(15, nil, nil)
	if it.timeout != 15*time.Second {
		t.Errorf("expected timeout=15s, got %v", it.timeout)
	}
}

func TestNewInterceptorZeroTimeout(t *testing.T) {
	it := NewInterceptor(0, nil, nil)
	if it.timeout != 15*time.Second {
		t.Errorf("expected default timeout=15s for zero input, got %v", it.timeout)
	}
}

// ========================================
// SetMode / GetMode
// ========================================

func TestSetGetMode(t *testing.T) {
	it := NewInterceptor(15, nil, nil)
	it.SetMode("manual")
	if it.GetMode() != "manual" {
		t.Errorf("expected mode=manual, got %s", it.GetMode())
	}
	it.SetMode("auto")
	if it.GetMode() != "auto" {
		t.Errorf("expected mode=auto, got %s", it.GetMode())
	}
}

// ========================================
// SetRules + matchRule
// ========================================

func TestMatchRuleExact(t *testing.T) {
	if !matchRule(models.InterceptRule{Pattern: "example.com"}, "GET", "example.com", "/") {
		t.Error("expected exact host match")
	}
	if matchRule(models.InterceptRule{Pattern: "example.com"}, "GET", "other.com", "/") {
		t.Error("expected no match for different host")
	}
}

func TestMatchRuleWildcard(t *testing.T) {
	if !matchRule(models.InterceptRule{Pattern: "*.example.com"}, "GET", "sub.example.com", "/api") {
		t.Error("expected wildcard match *.example.com → sub.example.com")
	}
	if matchRule(models.InterceptRule{Pattern: "*.example.com"}, "GET", "other.com", "/api") {
		t.Error("expected no match for wildcard on different host")
	}
}

func TestMatchRuleHostPathPrefix(t *testing.T) {
	if !matchRule(models.InterceptRule{Pattern: "example.com/api"}, "GET", "example.com", "/api/v1/users") {
		t.Error("expected host/path prefix match")
	}
	if matchRule(models.InterceptRule{Pattern: "example.com/api"}, "GET", "example.com", "/other") {
		t.Error("expected no match for different path")
	}
}

func TestMatchRuleCaseInsensitive(t *testing.T) {
	if !matchRule(models.InterceptRule{Pattern: "EXAMPLE.COM"}, "GET", "example.com", "/") {
		t.Error("expected case-insensitive host match")
	}
}

func TestMatchRuleMethod(t *testing.T) {
	// 规则限定 GET → GET 命中、POST 不命中
	if !matchRule(models.InterceptRule{Pattern: "example.com", Method: "GET"}, "GET", "example.com", "/") {
		t.Error("expected method GET to match")
	}
	if matchRule(models.InterceptRule{Pattern: "example.com", Method: "GET"}, "POST", "example.com", "/") {
		t.Error("expected method POST to NOT match GET-only rule")
	}
	// 方法大小写不敏感
	if !matchRule(models.InterceptRule{Pattern: "example.com", Method: "get"}, "GET", "example.com", "/") {
		t.Error("expected case-insensitive method match")
	}
	// 多方法（逗号分隔）
	if !matchRule(models.InterceptRule{Pattern: "example.com", Method: "GET,POST"}, "POST", "example.com", "/") {
		t.Error("expected POST to match GET,POST rule")
	}
	// 空 method → 所有方法都命中
	if !matchRule(models.InterceptRule{Pattern: "example.com", Method: ""}, "DELETE", "example.com", "/") {
		t.Error("expected empty method to match any")
	}
}

// ========================================
// GetPending
// ========================================

func TestGetPendingEmpty(t *testing.T) {
	it := NewInterceptor(15, nil, nil)
	pending := it.GetPending()
	if len(pending) != 0 {
		t.Errorf("expected 0 pending, got %d", len(pending))
	}
}

// ========================================
// Resolve — invalid id
// ========================================

func TestResolveInvalidID(t *testing.T) {
	it := NewInterceptor(15, nil, nil)
	err := it.Resolve("nonexistent", models.InterceptResult{Action: "allow"})
	if err == nil {
		t.Error("expected error for invalid request id")
	}
}

// ========================================
// Handle — auto mode with block rule
// ========================================

func TestHandleAutoBlock(t *testing.T) {
	it := NewInterceptor(15, nil, nil)
	it.SetMode("auto")
	it.SetRules([]models.InterceptRule{
		{Pattern: "*.blocked.com", Action: "block", Enabled: true},
	})

	req := httptest.NewRequest("GET", "https://evil.blocked.com/phish", nil)

	_, resp := it.Handle(req, nil, func(r *http.Request) {})
	if resp == nil {
		t.Error("expected blocked response, got nil")
	} else if resp.StatusCode != 403 {
		t.Errorf("expected 403, got %d", resp.StatusCode)
	}
}

// ========================================
// Handle — auto mode with allow rule (pass through)
// ========================================

func TestHandleAutoAllow(t *testing.T) {
	it := NewInterceptor(15, nil, nil)
	it.SetMode("auto")
	it.SetRules([]models.InterceptRule{
		{Pattern: "*.allowed.com", Action: "allow", Enabled: true},
	})

	req := httptest.NewRequest("GET", "https://sub.allowed.com/api", nil)

	resultReq, resultResp := it.Handle(req, nil, func(r *http.Request) {})
	if resultResp != nil {
		t.Error("expected nil response for allow rule")
	}
	if resultReq != req {
		t.Error("expected request to pass through")
	}
}

// ========================================
// Handle — auto mode, no rule match
// ========================================

func TestHandleAutoNoMatch(t *testing.T) {
	it := NewInterceptor(15, nil, nil)
	it.SetMode("auto")
	it.SetRules([]models.InterceptRule{
		{Pattern: "*.example.com", Action: "block", Enabled: true},
	})

	req := httptest.NewRequest("GET", "https://other.com/", nil)

	resultReq, resultResp := it.Handle(req, nil, func(r *http.Request) {})
	if resultResp != nil {
		t.Error("expected nil response for no match")
	}
	if resultReq != req {
		t.Error("expected request to pass through")
	}
}

// ========================================
// Handle — manual mode, pushes to pending
// ========================================

func TestHandleManualPending(t *testing.T) {
	var mu sync.Mutex
	var notified *models.PendingRequest

	it := NewInterceptor(15, func(req *models.PendingRequest) {
		mu.Lock()
		notified = req
		mu.Unlock()
	}, nil)
	it.SetMode("manual")

	reqURL, _ := url.Parse("https://httpbin.org/post")
	req := &http.Request{
		Method: "POST",
		URL:    reqURL,
		Header: http.Header{"Content-Type": []string{"application/json"}},
	}

	// Start Handle in goroutine (it will block waiting)
	done := make(chan struct{})
	go func() {
		defer close(done)
		it.Handle(req, nil, func(r *http.Request) {})
	}()

	// Wait for notification
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	n := notified
	mu.Unlock()

	if n == nil {
		t.Fatal("expected notification")
	}
	if n.Method != "POST" {
		t.Errorf("expected POST, got %s", n.Method)
	}
	if n.Host != "httpbin.org" {
		t.Errorf("expected httpbin.org, got %s", n.Host)
	}

	// Resolve to unblock
	err := it.Resolve(n.ID, models.InterceptResult{Action: "allow", RequestID: n.ID})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// Wait for Handle to return
	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Fatal("Handle did not return after resolve")
	}

	mu.Lock()
	// Verify pending list is empty after resolve
	pending := it.GetPending()
	if len(pending) != 0 {
		t.Errorf("expected 0 pending after resolve, got %d", len(pending))
	}
	mu.Unlock()
}

// ========================================
// Handle — manual mode, pending request carries body
// Bug 4: readBody 在 GetBody 为 nil 时（未走 OnRequest 路径）应 fallback 到 req.Body
// ========================================

func TestInterceptor_ManualMode_PendingBody(t *testing.T) {
	var mu sync.Mutex
	var notified *models.PendingRequest

	it := NewInterceptor(2, func(req *models.PendingRequest) {
		mu.Lock()
		notified = req
		mu.Unlock()
	}, nil)
	it.SetMode("manual")

	// 直接构造 POST req with body —— 不经过 proxy.go 的 OnRequest，因此 GetBody 为 nil。
	// 这模拟了直接调用 Interceptor.Handle 的场景。
	req := httptest.NewRequest("POST", "https://httpbin.org/post", strings.NewReader("hello world"))
	req.Header.Set("Content-Type", "text/plain")

	// 在 goroutine 中调用 Handle（manual 模式会阻塞等待 Resolve）
	done := make(chan struct{})
	go func() {
		defer close(done)
		it.Handle(req, nil, func(r *http.Request) {})
	}()

	// 等待 onNotify 回调
	waitForNotify := func() *models.PendingRequest {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			n := notified
			mu.Unlock()
			if n != nil {
				return n
			}
			time.Sleep(10 * time.Millisecond)
		}
		return nil
	}

	n := waitForNotify()
	if n == nil {
		t.Fatal("expected notification within 2s")
	}

	// 断言 Body 非空且等于 "hello world"
	if n.Body != "hello world" {
		t.Errorf("expected Body=\"hello world\", got %q", n.Body)
	}

	// 同时验证 GetPending 返回的 Body 也非空
	pending := it.GetPending()
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending, got %d", len(pending))
	}
	if pending[0].Body != "hello world" {
		t.Errorf("GetPending Body: expected \"hello world\", got %q", pending[0].Body)
	}

	// Resolve 以解除 Handle 阻塞
	if err := it.Resolve(n.ID, models.InterceptResult{Action: "allow", RequestID: n.ID}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Fatal("Handle did not return after resolve")
	}
}

// ========================================
// Handle — manual timeout auto-releases
// ========================================

func TestHandleManualTimeout(t *testing.T) {
	it := NewInterceptor(1, nil, nil) // 1 second timeout
	it.SetMode("manual")

	reqURL, _ := url.Parse("https://httpbin.org/get")
	req := &http.Request{
		Method: "GET",
		URL:    reqURL,
		Header: http.Header{},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		it.Handle(req, nil, func(r *http.Request) {})
	}()

	// Wait for timeout (1s + buffer)
	select {
	case <-done:
		// OK — request auto-released via timeout
	case <-time.After(3 * time.Second):
		t.Fatal("Handle did not return after timeout")
	}
}
