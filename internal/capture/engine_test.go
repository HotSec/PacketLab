package capture

import (
	"net"
	"sync"
	"testing"
	"time"

	"packetlab/internal/models"
	"packetlab/internal/store"
)

func TestDetermineDirection(t *testing.T) {
	tests := []struct {
		name     string
		srcPort  uint16
		dstPort  uint16
		expected bool
	}{
		{"dst is HTTP port 80", 54321, 80, true},
		{"dst is HTTPS port 443", 54321, 443, true},
		{"dst is 8080", 54321, 8080, true},
		{"dst is 8443", 54321, 8443, true},
		{"src is server port 80", 80, 54321, false},
		{"src is server port 443", 443, 54321, false},
		{"both ephemeral, src > dst", 60000, 50000, true},
		{"both ephemeral, src < dst", 50000, 60000, false},
		{"dst is well-known port 22", 54321, 22, true},
		{"dst is well-known port 25", 54321, 25, true},
		{"src is well-known port 22", 22, 54321, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := determineDirection(tt.srcPort, tt.dstPort)
			if result != tt.expected {
				t.Errorf("determineDirection(%d, %d) = %v, want %v", tt.srcPort, tt.dstPort, result, tt.expected)
			}
		})
	}
}

func TestIsLikelyServerPort(t *testing.T) {
	serverPorts := []uint16{80, 443, 8080, 8443, 3000, 5000, 8000, 8888, 9000, 9090}
	for _, port := range serverPorts {
		if !isLikelyServerPort(port) {
			t.Errorf("isLikelyServerPort(%d) = false, want true", port)
		}
	}
	wellKnown := []uint16{21, 22, 23, 25, 53, 110, 143, 993, 995}
	for _, port := range wellKnown {
		if !isLikelyServerPort(port) {
			t.Errorf("isLikelyServerPort(%d) = false, want true (well-known)", port)
		}
	}
	clientPorts := []uint16{50000, 60000, 32768, 12345}
	for _, port := range clientPorts {
		if isLikelyServerPort(port) {
			t.Errorf("isLikelyServerPort(%d) = true, want false (client port)", port)
		}
	}
}

func TestResponseCanHaveNoBody(t *testing.T) {
	tests := []struct {
		statusCode int
		expected   bool
	}{
		{100, true}, {101, true}, {199, true},
		{200, false}, {204, true}, {304, true},
		{301, false}, {404, false}, {500, false},
	}
	for _, tt := range tests {
		result := responseCanHaveNoBody(tt.statusCode)
		if result != tt.expected {
			t.Errorf("responseCanHaveNoBody(%d) = %v, want %v", tt.statusCode, result, tt.expected)
		}
	}
}

func TestParseStatusCodeFromHeader(t *testing.T) {
	tests := []struct {
		name     string
		header   string
		expected int
	}{
		{"200 OK", "HTTP/1.1 200 OK\r\n", 200},
		{"404 Not Found", "HTTP/1.1 404 Not Found\r\n", 404},
		{"301 Moved", "HTTP/1.1 301 Moved Permanently\r\n", 301},
		{"500 Error", "HTTP/1.1 500 Internal Server Error\r\n", 500},
		{"no reason", "HTTP/1.1 204 \r\n", 204},
		{"invalid header", "garbage", 0},
		{"empty", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseStatusCodeFromHeader([]byte(tt.header))
			if result != tt.expected {
				t.Errorf("parseStatusCodeFromHeader() = %d, want %d", result, tt.expected)
			}
		})
	}
}

func TestFindChunkedEnd(t *testing.T) {
	tests := []struct {
		name     string
		data     string
		expected int
	}{
		{"single chunk then end", "5\r\nhello\r\n0\r\n\r\n", len("5\r\nhello\r\n0\r\n\r\n")},
		{"two chunks then end", "5\r\nhello\r\n3\r\nbye\r\n0\r\n\r\n", len("5\r\nhello\r\n3\r\nbye\r\n0\r\n\r\n")},
		{"incomplete chunk", "5\r\nhel", -1},
		{"missing end marker", "5\r\nhello\r\n", -1},
		{"empty input", "", -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := findChunkedEnd([]byte(tt.data))
			if result != tt.expected {
				t.Errorf("findChunkedEnd() = %d, want %d", result, tt.expected)
			}
		})
	}
}

func TestTruncateBuffer(t *testing.T) {
	small := []byte("hello")
	result := truncateBuffer(small, 256)
	if len(result) != len(small) {
		t.Errorf("truncateBuffer(small) should not truncate, got len=%d", len(result))
	}

	big := make([]byte, 3*1024*1024)
	copy(big, "GET / HTTP/1.1\r\nHost: test\r\n\r\n")
	result = truncateBuffer(big, 256*1024)
	if len(result) > 256*1024+4096 {
		t.Errorf("truncateBuffer(big) should be ~256KB, got len=%d", len(result))
	}
	if len(result) == 0 {
		t.Error("truncateBuffer should not return empty")
	}
}

func TestLooksLikeHTTP(t *testing.T) {
	tests := []struct {
		data     string
		expected bool
	}{
		{"GET / HTTP/1.1\r\n", true},
		{"POST /api HTTP/1.1\r\n", true},
		{"PUT /resource HTTP/1.1\r\n", true},
		{"DELETE /item HTTP/1.1\r\n", true},
		{"PATCH /item HTTP/1.1\r\n", true},
		{"HEAD / HTTP/1.1\r\n", true},
		{"OPTIONS * HTTP/1.1\r\n", true},
		{"CONNECT example.com:443 HTTP/1.1\r\n", true},
		{"TRACE / HTTP/1.1\r\n", true},
		{"HTTP/1.1 200 OK\r\n", true},
		{"\x16\x03\x01", false},
		{"random binary data", false},
		{"", true},
	}
	for _, tt := range tests {
		result := looksLikeHTTP([]byte(tt.data))
		if result != tt.expected {
			t.Errorf("looksLikeHTTP(%q) = %v, want %v", truncate(tt.data, 20), result, tt.expected)
		}
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func newTestEngine(t *testing.T) (*Engine, *store.Store) {
	t.Helper()
	dbPath := t.TempDir() + "/test.db"
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	e := &Engine{
		store:     st,
		procCache: make(map[string]*models.ProcessInfo),
	}
	e.ringBuf = NewMemRingBuffer(1024)
	e.writer = NewAsyncWriterPool(st, e.ringBuf, 1, 10*time.Millisecond)
	e.writer.engine = e
	e.writer.Start()
	return e, st
}

func TestStreamFeedAndExtractHTTP(t *testing.T) {
	e, st := newTestEngine(t)
	pool := NewTCPStreamPool(e)
	stream := pool.New(net.ParseIP("192.168.1.100"), 54321, 80)

	stream.Feed([]byte("GET /api/test HTTP/1.1\r\nHost: example.com\r\n\r\n"), true)
	stream.Feed([]byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 5\r\n\r\nhello"), false)

	e.writer.Stop()

	items, total, _ := st.List("", "", "", false, 10, 0)
	if total == 0 {
		t.Fatal("expected at least 1 request in DB")
	}
	req, _ := st.Get(items[0].ID)
	if req.Method != "GET" {
		t.Errorf("Method = %q, want GET", req.Method)
	}
	if req.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", req.StatusCode)
	}
	if req.ResBody != "hello" {
		t.Errorf("ResBody = %q, want %q", req.ResBody, "hello")
	}
}

func TestStreamFeedChunkedResponse(t *testing.T) {
	e, st := newTestEngine(t)
	pool := NewTCPStreamPool(e)
	stream := pool.New(net.ParseIP("192.168.1.100"), 54321, 80)

	stream.Feed([]byte("GET /chunked HTTP/1.1\r\nHost: chunk.test\r\n\r\n"), true)
	stream.Feed([]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n3\r\nbye\r\n0\r\n\r\n"), false)

	e.writer.Stop()

	items, total, _ := st.List("", "", "", false, 10, 0)
	if total == 0 {
		t.Fatal("expected chunked response in DB")
	}
	req, _ := st.Get(items[0].ID)
	if req.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", req.StatusCode)
	}
}

func TestStreamFeedNoContentLengthResponse(t *testing.T) {
	e, st := newTestEngine(t)
	pool := NewTCPStreamPool(e)
	stream := pool.New(net.ParseIP("192.168.1.100"), 54321, 80)

	stream.Feed([]byte("GET /nobody HTTP/1.1\r\nHost: test\r\n\r\n"), true)
	stream.Feed([]byte("HTTP/1.1 204 No Content\r\n\r\n"), false)

	e.writer.Stop()

	items, total, _ := st.List("", "", "", false, 10, 0)
	if total == 0 {
		t.Fatal("expected 204 response in DB")
	}
	req, _ := st.Get(items[0].ID)
	if req.StatusCode != 204 {
		t.Errorf("StatusCode = %d, want 204", req.StatusCode)
	}
}

func TestStreamFeedHTTP1xxSkipped(t *testing.T) {
	e, st := newTestEngine(t)
	pool := NewTCPStreamPool(e)
	stream := pool.New(net.ParseIP("192.168.1.100"), 54321, 80)

	stream.Feed([]byte("GET /100 HTTP/1.1\r\nHost: test\r\n\r\n"), true)
	stream.Feed([]byte("HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"), false)

	e.writer.Stop()

	items, total, _ := st.List("", "", "", false, 10, 0)
	if total == 0 {
		t.Fatal("expected request after 1xx skip")
	}
	req, _ := st.Get(items[0].ID)
	if req.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200 (1xx should be skipped)", req.StatusCode)
	}
}

func TestStreamFeedNonHTTPMarkedAndSkipped(t *testing.T) {
	e, _ := newTestEngine(t)
	pool := NewTCPStreamPool(e)
	stream := pool.New(net.ParseIP("192.168.1.100"), 54321, 80)

	tlsData := make([]byte, 44)
	tlsData[0] = 0x16
	tlsData[1] = 0x03
	tlsData[2] = 0x01
	stream.Feed(tlsData, true)

	if !stream.nonHTTP {
		t.Error("stream should be marked as nonHTTP after TLS data")
	}
}

func TestStreamFeed304Response(t *testing.T) {
	e, st := newTestEngine(t)
	pool := NewTCPStreamPool(e)
	stream := pool.New(net.ParseIP("192.168.1.100"), 54321, 80)

	stream.Feed([]byte("GET /cached HTTP/1.1\r\nHost: test\r\n\r\n"), true)
	stream.Feed([]byte("HTTP/1.1 304 Not Modified\r\n\r\n"), false)

	e.writer.Stop()

	items, total, _ := st.List("", "", "", false, 10, 0)
	if total == 0 {
		t.Fatal("expected 304 response in DB")
	}
	req, _ := st.Get(items[0].ID)
	if req.StatusCode != 304 {
		t.Errorf("StatusCode = %d, want 304", req.StatusCode)
	}
}

func TestFlushOlderThanRemovesExpiredStreams(t *testing.T) {
	pool := NewTCPStreamPool(&Engine{procCache: make(map[string]*models.ProcessInfo)})
	a := NewAssembler(pool)

	stream := pool.New(net.ParseIP("192.168.1.100"), 54321, 80)
	a.streams["10.0.0.1:80-192.168.1.100:54321"] = stream

	if len(a.streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(a.streams))
	}

	a.FlushOlderThan(time.Now().Add(-1*time.Hour), nil)
	if len(a.streams) != 1 {
		t.Errorf("stream should not be flushed (lastActive is after cutoff), got %d", len(a.streams))
	}

	a.FlushOlderThan(time.Now().Add(1*time.Hour), nil)
	if len(a.streams) != 0 {
		t.Errorf("stream should be flushed (lastActive is before cutoff), got %d", len(a.streams))
	}
}

func TestFlushAllWithPendingEmitsRequest(t *testing.T) {
	e, st := newTestEngine(t)
	pool := NewTCPStreamPool(e)
	a := NewAssembler(pool)

	stream := pool.New(net.ParseIP("192.168.1.100"), 54321, 80)
	stream.Feed([]byte("GET /flush HTTP/1.1\r\nHost: flush.test\r\n\r\n"), true)
	stream.Feed([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"), false)
	a.streams["10.0.0.1:80-192.168.1.100:54321"] = stream

	a.FlushAllWithPending(e)

	e.writer.Stop()

	_, total, _ := st.List("", "", "", false, 10, 0)
	if total == 0 {
		t.Fatal("expected request after FlushAllWithPending")
	}
}

func TestHandleCloseRemovesStream(t *testing.T) {
	pool := NewTCPStreamPool(&Engine{procCache: make(map[string]*models.ProcessInfo)})
	a := NewAssembler(pool)

	stream := pool.New(net.ParseIP("192.168.1.100"), 54321, 80)
	stream.Feed([]byte("GET /close HTTP/1.1\r\nHost: close.test\r\n\r\n"), true)
	a.streams["10.0.0.1:80-192.168.1.100:54321"] = stream

	if len(a.streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(a.streams))
	}

	stream.HandleClose(a)

	if len(a.streams) != 0 {
		t.Errorf("stream should be removed after HandleClose, got %d", len(a.streams))
	}
}

func TestTryExtractHTTPOnCloseWithPartialResponse(t *testing.T) {
	e, st := newTestEngine(t)
	pool := NewTCPStreamPool(e)
	stream := pool.New(net.ParseIP("192.168.1.100"), 54321, 80)

	stream.Feed([]byte("GET /partial HTTP/1.1\r\nHost: partial.test\r\n\r\n"), true)
	stream.Feed([]byte("HTTP/1.1 200 OK\r\n\r\nsome body without content-length"), false)

	stream.mu.Lock()
	stream.tryExtractHTTPOnClose()
	stream.mu.Unlock()

	e.writer.Stop()

	items, total, _ := st.List("", "", "", false, 10, 0)
	if total == 0 {
		t.Fatal("expected request after tryExtractHTTPOnClose with partial response")
	}
	req, _ := st.Get(items[0].ID)
	if req.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", req.StatusCode)
	}
}

func TestMemRingBufferUsage(t *testing.T) {
	buf := NewMemRingBuffer(1024)
	if u := buf.Usage(); u != 0.0 {
		t.Errorf("empty buffer Usage() = %f, want 0", u)
	}
	buf.Push(&models.CapturedRequest{URL: "http://test/1"})
	if u := buf.Usage(); u == 0.0 {
		t.Error("buffer with 1 item should have Usage() > 0")
	}
}

func TestMemRingBufferDropped(t *testing.T) {
	buf := NewMemRingBuffer(65536)
	if d := buf.Dropped(); d != 0 {
		t.Errorf("empty buffer Dropped() = %d, want 0", d)
	}
	for i := 0; i < 65536+100; i++ {
		buf.Push(&models.CapturedRequest{URL: "http://test/"})
	}
	if d := buf.Dropped(); d == 0 {
		t.Error("overflowed buffer should have Dropped() > 0")
	}
}

// TestEmitNonBlocking_IncrementsHTTPFoundImmediately_RingPath 验证 ring buffer 路径下
// emitNonBlocking 在入口立即计数 HTTPFound，不依赖后续 flushAll 补加。
// 修改前：emitNonBlocking 不计数 → 测试 FAIL
// 修改后：emitNonBlocking 入口计数 → 测试 PASS
func TestEmitNonBlocking_IncrementsHTTPFoundImmediately_RingPath(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	e := &Engine{
		store:     st,
		procCache: make(map[string]*models.ProcessInfo),
	}
	e.ringBuf = NewMemRingBuffer(1024)
	// 不启动 writer，避免后台 flushAll 干扰计数检查 / Don't start writer to avoid background flushAll interfering with count check

	before := e.stats.HTTPFound.Load()
	e.emitNonBlocking(&models.CapturedRequest{
		Method: "GET",
		URL:    "http://example.com/test",
	})
	after := e.stats.HTTPFound.Load()
	if after != before+1 {
		t.Fatalf("ring path: expected HTTPFound to increment by 1 immediately, got before=%d after=%d", before, after)
	}
}

// TestEmitNonBlocking_IncrementsHTTPFoundImmediately_EmitBufPath 验证 emitBuf fallback 路径下
// emitNonBlocking 在入口立即计数 HTTPFound。
// 修改前：emitNonBlocking 不计数 → 测试 FAIL
// 修改后：emitNonBlocking 入口计数 → 测试 PASS
func TestEmitNonBlocking_IncrementsHTTPFoundImmediately_EmitBufPath(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	e := &Engine{
		store:     st,
		procCache: make(map[string]*models.ProcessInfo),
	}
	// ringBuf=nil，走 emitBuf fallback 路径 / ringBuf=nil, takes emitBuf fallback path

	before := e.stats.HTTPFound.Load()
	e.emitNonBlocking(&models.CapturedRequest{
		Method: "GET",
		URL:    "http://example.com/test",
	})
	after := e.stats.HTTPFound.Load()
	if after != before+1 {
		t.Fatalf("emitBuf path: expected HTTPFound to increment by 1 immediately, got before=%d after=%d", before, after)
	}
}

// TestEmitNonBlocking_NoDoubleCountAfterFlushAll 验证 emitNonBlocking 入口计数 + flushAll 不再计数，
// 避免每条记录被计数两次。
// 修改前：emitNonBlocking 不计数（HTTPFound=0），flushAll 计数（HTTPFound=1）→ immediate 检查 FAIL
// 修改后：emitNonBlocking 计数（HTTPFound=1），flushAll 不计数（HTTPFound=1）→ 全部 PASS
// 该测试同时防御未来回归：若 emitNonBlocking 和 flushAll 都计数，final 检查会 FAIL（HTTPFound=2）
func TestEmitNonBlocking_NoDoubleCountAfterFlushAll(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	e := &Engine{
		store:     st,
		procCache: make(map[string]*models.ProcessInfo),
	}
	e.ringBuf = NewMemRingBuffer(1024)
	// 长间隔 + 不 Start，避免后台 worker 自动 tick 调用 flushAll / long interval + don't Start, avoid background worker auto-ticking flushAll
	e.writer = NewAsyncWriterPool(st, e.ringBuf, 1, 1*time.Hour)
	e.writer.engine = e

	before := e.stats.HTTPFound.Load()
	e.emitNonBlocking(&models.CapturedRequest{
		Method: "GET",
		URL:    "http://example.com/test",
	})

	// 立即计数检查 / immediate count check
	immediate := e.stats.HTTPFound.Load()
	if immediate != before+1 {
		t.Fatalf("expected HTTPFound=%d immediately after emitNonBlocking, got %d", before+1, immediate)
	}

	// 手动调用 flushAll，不应再增加 HTTPFound / manually call flushAll, should NOT increment HTTPFound
	e.writer.flushAll()
	final := e.stats.HTTPFound.Load()
	if final != before+1 {
		t.Fatalf("expected HTTPFound to remain %d after flushAll (no double-count), got %d", before+1, final)
	}
}

// ---- Task 17: packetLoop / Stop 并发安全回归保护测试 ----
// These tests verify concurrent safety of Stop and packetLoop cleanup.
// Tests 1-3 are regression protection (behavior that should already hold);
// Test 4 (ReleasesLockBeforeBlockingOps) is a true RED test for the
// lock-holding-during-blocking bug and fails on the pre-fix Stop code.

// TestEngine_Stop_WithoutStart_NoPanic 验证未调用 Start 直接 Stop 不 panic。
// 回归保护：handle/writer 为 nil 时 Stop 必须安全。
func TestEngine_Stop_WithoutStart_NoPanic(t *testing.T) {
	e := &Engine{
		stopCh: make(chan struct{}),
	}
	// 不调用 Start，直接 Stop / Stop without prior Start
	if panicked := panics(func() { e.Stop() }); panicked {
		t.Fatal("Stop() panicked when called without Start")
	}
}

// TestEngine_Stop_Idempotent 验证 Stop 多次调用不 panic。
// 回归保护：running.Swap(false) 必须保证幂等。
func TestEngine_Stop_Idempotent(t *testing.T) {
	e := &Engine{
		stopCh: make(chan struct{}),
	}
	e.running.Store(true)
	if panicked := panics(func() {
		e.Stop()
		e.Stop() // 第二次不应 panic / second call must not panic
		e.Stop() // 第三次也不应 panic / third call must not panic
	}); panicked {
		t.Fatal("multiple Stop() calls panicked")
	}
}

// TestEngine_Stop_SetsRunningFalse 验证 Stop 后 running 为 false。
// 回归保护：running.Swap(false) 必须将状态置为 false。
func TestEngine_Stop_SetsRunningFalse(t *testing.T) {
	e := &Engine{
		stopCh: make(chan struct{}),
	}
	e.running.Store(true)
	e.Stop()
	if e.running.Load() {
		t.Fatal("expected running=false after Stop")
	}
}

// TestEngine_Stop_ReleasesLockBeforeBlockingOps 是 Task 17 的核心 RED 测试。
//
// Bug：修改前 Stop 用 `defer e.mu.Unlock()`，导致 flushEmitBuf/writer.Stop 等
// 阻塞操作在持锁期间执行。若 flushEmitBuf → bulkEmit → hub.BroadcastCapture 阻塞
// （例如 WebSocket 写入慢），其他需要 e.mu 的操作（如 Start、并发 Stop）会被阻塞。
//
// 修改后：Stop 在 handle 清理后立即释放 e.mu，再执行阻塞操作。
//
// 本测试用 blockingHub 让 BroadcastCapture 阻塞，验证 e.mu 在阻塞期间可被获取：
//   - 修改前：e.mu 在 flushEmitBuf 期间仍被持有 → 第二个 goroutine 超时 → FAIL
//   - 修改后：e.mu 已释放 → 第二个 goroutine 立即获取 → PASS
func TestEngine_Stop_ReleasesLockBeforeBlockingOps(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	blockHub := make(chan struct{})
	hub := &blockingHub{block: blockHub, broadcasted: make(chan struct{})}

	e := &Engine{
		store:     st,
		hub:       hub,
		stopCh:    make(chan struct{}),
		procCache: make(map[string]*models.ProcessInfo),
		// ringBuf=nil → flushEmitBuf 走 emitBuf → bulkEmit → hub.BroadcastCapture 路径
	}

	// 预填一条 emit 记录，使 flushEmitBuf 调用 bulkEmit → hub.BroadcastCapture（阻塞）
	e.emitMu.Lock()
	e.emitBuf = make([]*models.CapturedRequest, emitBufSize)
	e.emitBuf[0] = &models.CapturedRequest{Method: "GET", URL: "http://test"}
	e.emitHead = 1
	e.emitMu.Unlock()

	e.running.Store(true)

	// goroutine 1: 调用 Stop（会阻塞在 flushEmitBuf → hub.BroadcastCapture）
	stopDone := make(chan struct{})
	go func() {
		e.Stop()
		close(stopDone)
	}()

	// 等待 Stop 进入 flushEmitBuf 阻塞 / wait for Stop to reach blocking flushEmitBuf
	// hub.broadcasted 在 BroadcastCapture 被调用后关闭
	select {
	case <-hub.broadcasted:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not reach flushEmitBuf within 2s (hub.BroadcastCapture not called)")
	}

	// 此时 Stop 应已释放 e.mu（fix）或仍持有 e.mu（bug）
	// 尝试在另一个 goroutine 获取 e.mu
	acquired := make(chan struct{})
	go func() {
		e.mu.Lock()
		close(acquired)
		e.mu.Unlock()
	}()

	select {
	case <-acquired:
		// e.mu 已释放，fix 生效 / lock released before blocking op, fix works
	case <-time.After(1 * time.Second):
		t.Fatal("e.mu still held during flushEmitBuf (Stop holds lock during blocking operation)")
	}

	// 解除 hub 阻塞，让 Stop 完成 / unblock hub so Stop can complete
	close(blockHub)

	select {
	case <-stopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not complete after unblocking hub")
	}
}

// blockingHub 是测试用 hub，BroadcastCapture 会阻塞直到 block channel 被关闭。
// broadcasted 在 BroadcastCapture 被调用时关闭，用于测试同步。
type blockingHub struct {
	block       chan struct{}
	broadcasted chan struct{}
	captured    []*models.CapturedRequest
	once        sync.Once
}

func (h *blockingHub) BroadcastCapture(req *models.CapturedRequest) {
	h.once.Do(func() { close(h.broadcasted) })
	h.captured = append(h.captured, req)
	<-h.block // 阻塞直到 block 被关闭 / block until block channel is closed
}

func (h *blockingHub) BroadcastUpdate(req *models.CapturedRequest) {
	h.captured = append(h.captured, req)
}

// panics 执行 fn，返回是否 panic。
func panics(fn func()) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
		}
	}()
	fn()
	return
}
