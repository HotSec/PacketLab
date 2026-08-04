package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"packetlab/internal/models"
	"packetlab/internal/store"
)

// TestProxy_ForwardsFullRequestBody 验证代理转发完整的请求体到上游，
// 即使请求体超过 maxReqBodyKB 也不应被截断（仅存储侧截断）。
//
// Bug 1 复现：旧实现用 io.LimitReader 截断后替换 req.Body 转发，
// 导致 10MB POST 只到 2MB（实际为 maxReqBytes+1 字节）。
func TestProxy_ForwardsFullRequestBody(t *testing.T) {
	// 1. 上游服务器：读取完整 body 并记录长度
	var mu sync.Mutex
	var upstreamReceived int64
	upstreamCalled := make(chan struct{}, 16)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		upstreamReceived = int64(len(b))
		mu.Unlock()
		select {
		case upstreamCalled <- struct{}{}:
		default:
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "%d", len(b))
	}))
	defer upstream.Close()

	// 2. 创建代理（maxReqBodyKB=2，即 2MB；10MB body 远超此阈值）
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()

	s := New(0, st, nil, nil, nil, nil, 2, 64, false)
	// 用 httptest.NewServer 直接包裹 goproxy 处理器，省去端口管理
	proxySrv := httptest.NewServer(s.proxy)
	defer proxySrv.Close()
	defer s.Stop()

	// 3. 配置走代理的 HTTP 客户端
	proxyURL, _ := url.Parse(proxySrv.URL)
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
	}

	// 4. 构造 10MB 请求体并发送
	bodySize := 10 * 1024 * 1024
	body := bytes.Repeat([]byte("a"), bodySize)
	resp, err := client.Post(upstream.URL, "text/plain", bytes.NewReader(body))
	if err == nil && resp != nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	// bug 存在时代理会因 Content-Length 不匹配导致转发失败，err != nil
	// 但我们仍检查上游实际收到的字节数

	// 5. 等待上游被调用（或超时）
	select {
	case <-upstreamCalled:
	case <-time.After(3 * time.Second):
	}

	mu.Lock()
	received := upstreamReceived
	mu.Unlock()

	// 6. 断言上游收到完整 10MB（而非被截断为 maxReqBytes+1）
	if received != int64(bodySize) {
		t.Errorf("upstream received %d bytes, want %d (body was truncated by proxy)",
			received, int64(bodySize))
	}
}

// TestProxy_ForwardsFullResponseBody 验证代理转发完整的响应体给客户端，
// 即使响应体超过 maxResBodyKB 也不应被截断（仅存储侧截断）。
//
// Bug 1 响应侧复现：旧实现用 io.LimitReader(resp.Body, maxResBytes+1)
// 读取后用截断的 bodyBytes 替换 resp.Body 转发，导致 10MB 响应只到 4MB。
func TestProxy_ForwardsFullResponseBody(t *testing.T) {
	// 1. 上游服务器：返回 10MB body
	bodySize := 10 * 1024 * 1024
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", bodySize))
		w.WriteHeader(http.StatusOK)
		// 不能依赖 Write 后的 Content-Length 校验，直接写完整 body
		if _, err := w.Write(bytes.Repeat([]byte("a"), bodySize)); err != nil {
			t.Logf("upstream write: %v", err)
		}
	}))
	defer upstream.Close()

	// 2. 创建代理（maxResBodyKB=4，即 4MB；10MB body 远超此阈值）
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()

	s := New(0, st, nil, nil, nil, nil, 64, 4, false)
	proxySrv := httptest.NewServer(s.proxy)
	defer proxySrv.Close()
	defer s.Stop()

	// 3. 配置走代理的 HTTP 客户端
	proxyURL, _ := url.Parse(proxySrv.URL)
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
	}

	// 4. 发起请求并读取响应体
	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	defer resp.Body.Close()
	received, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}

	// 5. 断言客户端收到完整 10MB（而非被截断为 maxResBytes+1）
	if len(received) != bodySize {
		t.Errorf("client received %d bytes, want %d (response body was truncated by proxy)",
			len(received), bodySize)
	}
}

// TestMatchSkipHost 验证 shouldMITM 在匹配跳过列表前正确去除端口后缀。
//
// Bug 3 复现：HandleConnectFunc 收到的 host 形如 "abc.wns.windows.com:443"，
// 旧实现直接对带端口的 host 做 HasSuffix(".wns.windows.com")，末尾是 ":443"，
// 永远匹配不上，导致本应跳过 MITM 的主机被错误地解密。
// TestProxy_ManualResponseModify 验证手动拦截响应的端到端流：上游响应进入待审
// 队列 → 用户修改状态码/body → 客户端收到修改后的响应，且捕获记录同步。
func TestProxy_ManualResponseModify(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("server-body"))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()

	it := NewInterceptor(5*time.Second, nil, nil)
	it.SetMode("manual")
	defer it.Stop()

	s := New(0, st, nil, nil, nil, it, 64, 64, false)
	proxySrv := httptest.NewServer(s.proxy)
	defer proxySrv.Close()
	defer s.Stop()

	proxyURL, _ := url.Parse(proxySrv.URL)
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
	}

	type clientResult struct {
		status int
		body   string
	}
	resultCh := make(chan clientResult, 1)
	go func() {
		resp, err := client.Get(upstream.URL + "/api")
		if err != nil {
			resultCh <- clientResult{status: -1, body: err.Error()}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		resultCh <- clientResult{status: resp.StatusCode, body: string(b)}
	}()

	// 手动模式：请求先进入待审队列，放行后才走到响应拦截。
	// 循环：request 待审一律 allow，直到 response 待审出现。
	var pendingID string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && pendingID == "" {
		for _, p := range it.GetPending() {
			if p.Kind == models.PendingKindResponse {
				pendingID = p.ID
			} else if p.Kind == models.PendingKindRequest {
				if err := it.Resolve(p.ID, models.InterceptResult{Action: "allow", RequestID: p.ID}); err != nil {
					t.Fatalf("resolve request pending: %v", err)
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pendingID == "" {
		t.Fatal("expected response pending entry")
	}

	if err := it.Resolve(pendingID, models.InterceptResult{
		Action:     "modify",
		RequestID:  pendingID,
		StatusCode: 502,
		NewBody:    "injected",
		NewHeaders: map[string]string{"X-Modified": "true"},
	}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	select {
	case res := <-resultCh:
		if res.status != 502 {
			t.Errorf("expected client status 502, got %d (body=%q)", res.status, res.body)
		}
		if res.body != "injected" {
			t.Errorf("expected client body injected, got %q", res.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client request did not complete")
	}
}

func TestMatchSkipHost(t *testing.T) {
	cases := []struct {
		host     string
		wantSkip bool // true 表示应跳过 MITM（shouldMITM 返回 false）
	}{
		{"abc.wns.windows.com:443", true},
		{"abc.wns.windows.com", true},
		{"cdn.push.apple.com:443", true},
		{"api.example.com:443", false},
		{"push.microsoft.com:443", false},
		{"1.2.3.4:8080", false},
		{"ntp.msn.cn:443", true},        // 完整匹配（无前导点的条目）
		{"x.wns.windows.com:80", true},  // 非标准端口也应匹配
		{"xwns.windows.com:443", false}, // 前缀不对（不含 .wns.windows.com）
	}
	for _, c := range cases {
		got := shouldMITM(c.host)
		// shouldMITM 返回 true 表示应 MITM；skip 时返回 false
		if got == c.wantSkip {
			t.Errorf("shouldMITM(%q) = %v, want MITM=%v (skip=%v)",
				c.host, got, !c.wantSkip, c.wantSkip)
		}
	}
}
