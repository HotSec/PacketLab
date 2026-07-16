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

	s := New(0, st, nil, nil, nil, nil, 2, 64)
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
