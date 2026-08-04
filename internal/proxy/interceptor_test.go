package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"packetlab/internal/models"
	"packetlab/internal/store"
)

// ========================================
// NewInterceptor
// ========================================

func TestNewInterceptorDefaultMode(t *testing.T) {
	it := NewInterceptor(15*time.Second, nil, nil)
	if it.GetMode() != "auto" {
		t.Errorf("expected mode=auto, got %s", it.GetMode())
	}
}

func TestNewInterceptorTimeout(t *testing.T) {
	it := NewInterceptor(15*time.Second, nil, nil)
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
	it := NewInterceptor(15*time.Second, nil, nil)
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
	it := NewInterceptor(15*time.Second, nil, nil)
	pending := it.GetPending()
	if len(pending) != 0 {
		t.Errorf("expected 0 pending, got %d", len(pending))
	}
}

// ========================================
// Resolve — invalid id
// ========================================

func TestResolveInvalidID(t *testing.T) {
	it := NewInterceptor(15*time.Second, nil, nil)
	err := it.Resolve("nonexistent", models.InterceptResult{Action: "allow"})
	if err == nil {
		t.Error("expected error for invalid request id")
	}
}

// ========================================
// Handle — auto mode with block rule
// ========================================

func TestHandleAutoBlock(t *testing.T) {
	it := NewInterceptor(15*time.Second, nil, nil)
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
	it := NewInterceptor(15*time.Second, nil, nil)
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
	it := NewInterceptor(15*time.Second, nil, nil)
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

	it := NewInterceptor(15*time.Second, func(req *models.PendingRequest) {
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

	it := NewInterceptor(2*time.Second, func(req *models.PendingRequest) {
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
// Handle — manual modify: headers only must keep original body
// 回归测试：仅修改 header/method/URL 时不得丢弃请求体
// ========================================

func TestInterceptor_ManualModify_HeadersOnlyKeepsBody(t *testing.T) {
	it := NewInterceptor(2*time.Second, nil, nil)
	it.SetMode("manual")

	req := httptest.NewRequest("POST", "https://httpbin.org/post", strings.NewReader("original-body"))
	req.Header.Set("Content-Type", "text/plain")

	var resultReq *http.Request
	var resultResp *http.Response
	done := make(chan struct{})
	go func() {
		defer close(done)
		resultReq, resultResp = it.Handle(req, nil, func(r *http.Request) {})
	}()

	// 等待进入待审队列
	var n *models.PendingRequest
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p := it.GetPending(); len(p) > 0 {
			n = &p[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n == nil {
		t.Fatal("expected pending request")
	}

	// 仅修改 header（不提供 NewBody）
	if err := it.Resolve(n.ID, models.InterceptResult{
		Action:     "modify",
		RequestID:  n.ID,
		NewHeaders: map[string]string{"X-Added": "yes"},
	}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Handle did not return after resolve")
	}

	if resultResp != nil {
		t.Fatalf("expected forwarded request, got response %d", resultResp.StatusCode)
	}
	if resultReq == nil {
		t.Fatal("expected modified request")
	}
	if resultReq.Header.Get("X-Added") != "yes" {
		t.Errorf("expected X-Added=yes, got %q", resultReq.Header.Get("X-Added"))
	}
	if resultReq.Header.Get("Content-Type") != "text/plain" {
		t.Errorf("expected original header preserved, got %q", resultReq.Header.Get("Content-Type"))
	}
	body, err := io.ReadAll(resultReq.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "original-body" {
		t.Errorf("expected body preserved, got %q", string(body))
	}
}

// ========================================
// Handle — manual timeout auto-releases
// ========================================

func TestHandleManualTimeout(t *testing.T) {
	it := NewInterceptor(1*time.Second, nil, nil) // 1 second timeout
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

// ========================================
// HandleResponse — manual mode, response modify/drop/timeout
// ========================================

func newTestResponse(t *testing.T, status int, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest("GET", "https://example.com/api", nil)
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{"Content-Type": []string{"text/plain"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

// 等待响应进入待审队列并返回 PendingRequest（kind=response）
func waitForResponsePending(t *testing.T, it *Interceptor) *models.PendingRequest {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, p := range it.GetPending() {
			if p.Kind == "response" {
				return &p
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expected response pending entry within 2s")
	return nil
}

func TestInterceptor_ManualResponse_ModifyStatusHeadersBody(t *testing.T) {
	it := NewInterceptor(2*time.Second, nil, nil)
	it.SetMode("manual")

	resp := newTestResponse(t, 200, "original-body")

	var stored *http.Response
	var storedBodyReplaced bool
	done := make(chan struct{})
	var resultResp *http.Response
	go func() {
		defer close(done)
		resultResp = it.HandleResponse(resp, nil, func(r *http.Response, bodyReplaced bool) {
			stored = r
			storedBodyReplaced = bodyReplaced
		})
	}()

	n := waitForResponsePending(t, it)
	if n.StatusCode != 200 {
		t.Errorf("expected pending status_code=200, got %d", n.StatusCode)
	}
	if n.Body != "original-body" {
		t.Errorf("expected pending body preview, got %q", n.Body)
	}

	if err := it.Resolve(n.ID, models.InterceptResult{
		Action:     "modify",
		RequestID:  n.ID,
		StatusCode: 418,
		NewHeaders: map[string]string{"X-Injected": "yes"},
		NewBody:    "modified-body",
	}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HandleResponse did not return after resolve")
	}

	if resultResp == nil {
		t.Fatal("expected response")
	}
	if resultResp.StatusCode != 418 {
		t.Errorf("expected status 418, got %d", resultResp.StatusCode)
	}
	if !strings.HasPrefix(resultResp.Status, "418") {
		t.Errorf("expected Status to start with 418, got %q", resultResp.Status)
	}
	if resultResp.Header.Get("X-Injected") != "yes" {
		t.Errorf("expected X-Injected=yes, got %q", resultResp.Header.Get("X-Injected"))
	}
	if resultResp.Header.Get("Content-Length") != "13" {
		t.Errorf("expected Content-Length=13, got %q", resultResp.Header.Get("Content-Length"))
	}
	body, err := io.ReadAll(resultResp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "modified-body" {
		t.Errorf("expected modified-body, got %q", string(body))
	}
	if stored != resultResp || !storedBodyReplaced {
		t.Errorf("expected storeFunc called with bodyReplaced=true, got %v", storedBodyReplaced)
	}
}

// 回归：仅修改响应头（不改 body）时不得丢失响应体
func TestInterceptor_ManualResponse_HeadersOnlyKeepsBody(t *testing.T) {
	it := NewInterceptor(2*time.Second, nil, nil)
	it.SetMode("manual")

	resp := newTestResponse(t, 200, "keep-me")

	done := make(chan struct{})
	var resultResp *http.Response
	go func() {
		defer close(done)
		resultResp = it.HandleResponse(resp, nil, func(r *http.Response, bodyReplaced bool) {})
	}()

	n := waitForResponsePending(t, it)
	if err := it.Resolve(n.ID, models.InterceptResult{
		Action:     "modify",
		RequestID:  n.ID,
		NewHeaders: map[string]string{"X-Added": "1"},
	}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HandleResponse did not return")
	}

	if resultResp.Header.Get("X-Added") != "1" {
		t.Errorf("expected X-Added=1, got %q", resultResp.Header.Get("X-Added"))
	}
	body, err := io.ReadAll(resultResp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "keep-me" {
		t.Errorf("expected body preserved, got %q", string(body))
	}
}

func TestInterceptor_ManualResponse_Drop(t *testing.T) {
	it := NewInterceptor(2*time.Second, nil, nil)
	it.SetMode("manual")

	resp := newTestResponse(t, 200, "payload")

	done := make(chan struct{})
	var resultResp *http.Response
	go func() {
		defer close(done)
		resultResp = it.HandleResponse(resp, nil, func(r *http.Response, bodyReplaced bool) {})
	}()

	n := waitForResponsePending(t, it)
	if err := it.Resolve(n.ID, models.InterceptResult{Action: "drop", RequestID: n.ID}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HandleResponse did not return")
	}

	if resultResp == nil || resultResp.StatusCode != 403 {
		t.Fatalf("expected 403 after drop, got %v", resultResp)
	}
}

func TestInterceptor_ManualResponse_TimeoutAutoAllows(t *testing.T) {
	it := NewInterceptor(500*time.Millisecond, nil, nil)
	it.SetMode("manual")

	resp := newTestResponse(t, 201, "timeout-body")

	done := make(chan struct{})
	var resultResp *http.Response
	go func() {
		defer close(done)
		resultResp = it.HandleResponse(resp, nil, func(r *http.Response, bodyReplaced bool) {
			t.Error("storeFunc must not be called on timeout (no modification)")
		})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HandleResponse did not return after timeout")
	}

	if resultResp == nil || resultResp.StatusCode != 201 {
		t.Fatalf("expected original 201 after timeout, got %v", resultResp)
	}
	// 待审队列应已清理
	if n := len(it.GetPending()); n != 0 {
		t.Errorf("expected 0 pending after timeout, got %d", n)
	}
}

func TestInterceptor_AutoMode_ResponsePassesThrough(t *testing.T) {
	it := NewInterceptor(time.Second, nil, nil)
	it.SetMode("auto")

	resp := newTestResponse(t, 200, "body")
	resultResp := it.HandleResponse(resp, nil, func(r *http.Response, bodyReplaced bool) {
		t.Error("storeFunc must not be called in auto mode")
	})
	if resultResp != resp {
		t.Error("expected pass-through in auto mode")
	}
}

func TestGetPending_ResponseEntries(t *testing.T) {
	it := NewInterceptor(2*time.Second, nil, nil)
	it.SetMode("manual")

	resp := newTestResponse(t, 500, "server-error")
	done := make(chan struct{})
	go func() {
		defer close(done)
		it.HandleResponse(resp, nil, func(r *http.Response, bodyReplaced bool) {})
	}()

	n := waitForResponsePending(t, it)
	if n.Kind != "response" {
		t.Errorf("expected kind=response, got %q", n.Kind)
	}
	if n.StatusCode != 500 {
		t.Errorf("expected status_code=500, got %d", n.StatusCode)
	}
	if n.Headers["Content-Type"] != "text/plain" {
		t.Errorf("expected response headers snapshot, got %v", n.Headers)
	}

	if err := it.Resolve(n.ID, models.InterceptResult{Action: "allow", RequestID: n.ID}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HandleResponse did not return after resolve")
	}
}

// ========================================
// Stop — flush logs and idempotent
// ========================================

// newTestInterceptorStore 在临时目录创建一个真实 store 用于 interceptor 测试
func newTestInterceptorStore(t *testing.T) *store.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestInterceptor_Stop_FlushesLogs 注入 100 条 log，调用 Stop，断言全部落库
func TestInterceptor_Stop_FlushesLogs(t *testing.T) {
	st := newTestInterceptorStore(t)

	it := NewInterceptor(1*time.Second, func(*models.PendingRequest) {}, st)
	for i := 0; i < 100; i++ {
		it.logCh <- &models.InterceptLog{
			Action: "allow", RequestURL: "http://x", RequestHost: "x", Mode: "manual",
		}
	}

	it.Stop()

	// Stop 内部 wg.Wait，应已落库
	// 注：ListInterceptLogs 在 limit<=0 时默认 limit=50（store.go:931-933），
	// 因此用 total（第二返回值）验证真实落库条数，而非 len(logs)。
	_, total, err := st.ListInterceptLogs("", "", "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 100 {
		t.Fatalf("expected 100 logs, got %d", total)
	}
}

func TestInterceptor_Stop_Idempotent(t *testing.T) {
	st := newTestInterceptorStore(t)
	it := NewInterceptor(1*time.Second, func(*models.PendingRequest) {}, st)
	it.Stop()
	it.Stop() // 不 panic
}

// TestInterceptor_Stop_NoPanicOnConcurrentWriteLog 验证 Stop 后调用 writeLog 不会 panic。
// 这是 Bug 7 修复的 critical 部分：writeLog 加 RWMutex 保护，避免 send on closed channel。
func TestInterceptor_Stop_NoPanicOnConcurrentWriteLog(t *testing.T) {
	st := newTestInterceptorStore(t)
	it := NewInterceptor(1*time.Second, func(*models.PendingRequest) {}, st)

	// 启动一个 goroutine 持续调用 writeLog
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				it.writeLog(&models.InterceptLog{
					Action: "allow", RequestURL: "http://x", RequestHost: "x", Mode: "manual",
				})
			}
		}
	}()

	// 让 writeLog 跑一会
	time.Sleep(20 * time.Millisecond)

	// 调用 Stop，不应 panic
	it.Stop()

	// Stop 后再调用 writeLog，不应 panic（应被 closed 检查拦截）
	for i := 0; i < 100; i++ {
		it.writeLog(&models.InterceptLog{
			Action: "allow", RequestURL: "http://x", RequestHost: "x", Mode: "manual",
		})
	}

	close(stop)
	wg.Wait()
}

// TestInterceptor_PendingTimeout_Configurable 验证拦截器 pending 请求超时可通过 NewInterceptor 参数配置。
// V3-3：原 15s 硬编码，改为 CLI 可配（1s~10m）。
// 此测试用 100ms 短超时验证参数确实生效（非硬编码 15s）。
func TestInterceptor_PendingTimeout_Configurable(t *testing.T) {
	st := newTestInterceptorStore(t)

	it := NewInterceptor(100*time.Millisecond, func(*models.PendingRequest) {}, st)
	defer it.Stop()
	it.SetMode("manual") // manual 模式下请求进入 pending 队列，触发超时路径

	req, _ := http.NewRequest("POST", "http://x.example/", strings.NewReader("body"))
	start := time.Now()
	_, _ = it.Handle(req, nil, func(r *http.Request) {})
	elapsed := time.Since(start)

	// 100ms 超时允许 ±10ms 抖动；若硬编码 15s 则 elapsed 远大于 200ms
	if elapsed < 90*time.Millisecond || elapsed > 200*time.Millisecond {
		t.Fatalf("expected ~100ms timeout (configurable), got %v", elapsed)
	}
}
