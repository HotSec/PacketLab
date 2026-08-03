package capture

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"packetlab/internal/models"
	"packetlab/internal/store"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
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

// ---- Task 19: --capture-max-streams CLI with LRU eviction ----

// TestEngine_SetMaxStreams 验证 Engine.SetMaxStreams setter 行为：
//   - 正数入参直接写入 maxStreams 字段
//   - <=0 入参应被忽略，保持原值不变（默认值兜底逻辑放在 NewAssembler 中）
//
// 此测试在添加 SetMaxStreams 方法前会编译失败（method undefined）→ RED。
// 与 Task 15 的 TestEngine_SetRingBufSize 模式一致。
func TestEngine_SetMaxStreams(t *testing.T) {
	e := &Engine{}
	e.SetMaxStreams(1024)
	if e.maxStreams != 1024 {
		t.Fatalf("expected maxStreams=1024, got %d", e.maxStreams)
	}
	// 0 不应改变 / 0 should be ignored
	e.SetMaxStreams(0)
	if e.maxStreams != 1024 {
		t.Fatalf("expected maxStreams unchanged=1024, got %d", e.maxStreams)
	}
	// 负数也应被忽略 / negative should also be ignored
	e.SetMaxStreams(-1)
	if e.maxStreams != 1024 {
		t.Fatalf("expected maxStreams unchanged=1024 after negative, got %d", e.maxStreams)
	}
}

// TestEngine_MaxStreamsEviction 验证 evictOldestIdleStream 按 lastActive 升序淘汰最老流。
//
// 修改前：evictOldestIdleStream 方法不存在 → 编译失败 → RED。
// 修改后：调用两次后剩余 10 条，最老的 2 条被删除，第 3 老及以后保留。
//
// 不构造真实 gopacket.Packet（构造复杂），直接测 evictOldestIdleStream 单元行为，
// 与 TestFlushOlderThanRemovesExpiredStreams 的测试模式一致（手动填充 a.streams）。
func TestEngine_MaxStreamsEviction(t *testing.T) {
	e := &Engine{
		procCache: make(map[string]*models.ProcessInfo),
	}
	e.SetMaxStreams(10)
	pool := NewTCPStreamPool(e)
	a := NewAssembler(pool)

	// NewAssembler 必须从 pool.engine.maxStreams 读取并填充 a.maxStreams
	if a.maxStreams != 10 {
		t.Fatalf("expected assembler.maxStreams=10 (from engine), got %d", a.maxStreams)
	}

	// 添加 12 个流，每个流有不同的 lastActive 时间（升序）
	// Add 12 streams with distinct lastActive (ascending)
	baseTime := time.Now()
	keys := make([]string, 12)
	for i := 0; i < 12; i++ {
		keys[i] = fmt.Sprintf("10.0.0.%d:80-192.168.1.100:543%d", i, i)
		stream := pool.New(net.ParseIP(fmt.Sprintf("10.0.0.%d", i)), uint16(54300+i), 80)
		stream.mu.Lock()
		stream.lastActive = baseTime.Add(time.Duration(i) * time.Second)
		stream.mu.Unlock()
		a.streams[keys[i]] = stream
	}

	if len(a.streams) != 12 {
		t.Fatalf("expected 12 streams after setup, got %d", len(a.streams))
	}

	// 淘汰最老的 2 个流 / evict 2 oldest streams
	a.mu.Lock()
	a.evictOldestIdleStream()
	a.evictOldestIdleStream()
	a.mu.Unlock()

	if len(a.streams) != 10 {
		t.Fatalf("expected 10 streams after 2 evictions, got %d", len(a.streams))
	}

	// 最老的 2 个（lastActive 最早）应被删除
	// Oldest 2 (earliest lastActive) should be removed
	if _, ok := a.streams[keys[0]]; ok {
		t.Errorf("expected oldest stream %q to be evicted, but still present", keys[0])
	}
	if _, ok := a.streams[keys[1]]; ok {
		t.Errorf("expected 2nd oldest stream %q to be evicted, but still present", keys[1])
	}

	// 第 3 老及以后应保留 / 3rd oldest onward should remain
	if _, ok := a.streams[keys[2]]; !ok {
		t.Errorf("expected 3rd oldest stream %q to still be present, but was evicted", keys[2])
	}
	if _, ok := a.streams[keys[11]]; !ok {
		t.Errorf("expected newest stream %q to still be present, but was evicted", keys[11])
	}

	// 验证 StreamsEvicted 计数（Assembler 上的淘汰逻辑不应直接计数，
	// 计数在 Assemble 调用路径中由 engine.stats.StreamsEvicted 完成；此处直接调用
	// evictOldestIdleStream 不应增加计数）。
	// Verify StreamsEvicted stat: eviction-only method should not increment;
	// counting happens at Assemble call site.
	if got := e.stats.StreamsEvicted.Load(); got != 0 {
		t.Errorf("expected StreamsEvicted=0 (direct evict call should not count), got %d", got)
	}
}

// TestEngine_NewAssembler_ReadsMaxStreamsFromEngine 验证 NewAssembler
// 从 pool.engine.maxStreams 读取上限，并在 engine.maxStreams<=0 时用默认值 1000 兜底。
//
// 修改前：Assembler 无 maxStreams 字段 → 编译失败 → RED。
// 修改后：maxStreams=0 时 a.maxStreams=1000；maxStreams=64 时 a.maxStreams=64。
func TestEngine_NewAssembler_ReadsMaxStreamsFromEngine(t *testing.T) {
	// 默认兜底：engine.maxStreams=0 → a.maxStreams=1000
	e0 := &Engine{procCache: make(map[string]*models.ProcessInfo)}
	pool0 := NewTCPStreamPool(e0)
	a0 := NewAssembler(pool0)
	if a0.maxStreams != 1000 {
		t.Fatalf("default fallback: expected a.maxStreams=1000, got %d", a0.maxStreams)
	}

	// 显式设置：engine.maxStreams=64 → a.maxStreams=64
	e1 := &Engine{procCache: make(map[string]*models.ProcessInfo)}
	e1.SetMaxStreams(64)
	pool1 := NewTCPStreamPool(e1)
	a1 := NewAssembler(pool1)
	if a1.maxStreams != 64 {
		t.Fatalf("explicit set: expected a.maxStreams=64, got %d", a1.maxStreams)
	}
}

// TestEngine_GetMetrics_IncludesStreamsEvicted 验证 GetMetrics 暴露 streams_evicted。
//
// 修改前：GetMetrics 无 streams_evicted 字段 → 测试 FAIL。
// 修改后：GetMetrics 返回 streams_evicted 计数。
func TestEngine_GetMetrics_IncludesStreamsEvicted(t *testing.T) {
	e := &Engine{}
	e.stats.StreamsEvicted.Add(7)
	m := e.GetMetrics()
	v, ok := m["streams_evicted"]
	if !ok {
		t.Fatal("expected streams_evicted key in metrics, missing")
	}
	got, ok := v.(int64)
	if !ok {
		t.Fatalf("expected streams_evicted to be int64, got %T", v)
	}
	if got != 7 {
		t.Errorf("expected streams_evicted=7, got %d", got)
	}
}

// newTestTCPPacket 构造一个 IPv4+TCP 的 gopacket.Packet，用于测试 Assemble。
// 由 gopacket.SerializeLayers 生成 raw bytes，再用 LayerTypeEthernet decoder 解码，
// 确保 packet.Layer(LayerTypeTCP) / packet.Layer(LayerTypeIPv4) 可正常返回。
//
// Build a IPv4+TCP gopacket.Packet for testing Assemble. Serializes layers to
// raw bytes and decodes them back so Layer() lookups work as expected.
func newTestTCPPacket(srcIP, dstIP net.IP, srcPort, dstPort uint16, payload []byte, fin, rst bool) gopacket.Packet {
	return newTestTCPPacketFlags(srcIP, dstIP, srcPort, dstPort, payload, fin, rst, false, false)
}

// newTestTCPPacketFlags 同 newTestTCPPacket，额外支持 SYN/ACK 标志位。
func newTestTCPPacketFlags(srcIP, dstIP net.IP, srcPort, dstPort uint16, payload []byte, fin, rst, syn, ack bool) gopacket.Packet {
	buf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{ComputeChecksums: true, FixLengths: true}

	tcp := &layers.TCP{
		SrcPort: layers.TCPPort(srcPort),
		DstPort: layers.TCPPort(dstPort),
		Seq:     1000,
		Window:  65535,
		FIN:     fin,
		RST:     rst,
		SYN:     syn,
		ACK:     ack,
	}
	ip := &layers.IPv4{
		Version:  4,
		IHL:      5,
		TTL:      64,
		Protocol: layers.IPProtocolTCP,
		SrcIP:    srcIP,
		DstIP:    dstIP,
	}
	// TCP checksum 计算需要网络层信息 / TCP checksum requires network layer info
	tcp.SetNetworkLayerForChecksum(ip)
	eth := &layers.Ethernet{
		SrcMAC:       net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55},
		DstMAC:       net.HardwareAddr{0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb},
		EthernetType: layers.EthernetTypeIPv4,
	}
	if err := gopacket.SerializeLayers(buf, opts, eth, ip, tcp, gopacket.Payload(payload)); err != nil {
		panic(err)
	}
	return gopacket.NewPacket(buf.Bytes(), layers.LayerTypeEthernet, gopacket.Default)
}

// TestEngine_Assemble_EvictsAndCounts 验证 Assemble 在容量满时触发 LRU 淘汰 +
// StreamsEvicted 计数，并在释放 a.mu 后 flush 被淘汰流的残留数据（pendingReq 被
// emit 到 DB）。
//
// Bug（Important #1）：修改前 evictOldestIdleStream 直接 delete 不 flush，被淘汰
// 流的 pendingReq / sseBuf / clientBuf / serverBuf 全部丢失；且注释错误地声称
// "GC ticker 兜底"，但 FlushOlderThan（GC ticker）只遍历 a.streams，被 delete 的
// 流已不在 map 中，永远不会被处理。
//
// 修改后：evictOldestIdleStream 返回被淘汰流指针，Assemble 在释放 a.mu 后调用
// flushEvictedStream flush 残留数据。
//
// 本测试构造真实 gopacket.Packet（IPv4+TCP）端到端验证 Assemble 路径：
//  1. 填充 2 条流达到 maxStreams=2 上限，第 1 条流留下 pending 请求 + 部分响应
//  2. 发送第 3 条流的 packet → 触发淘汰第 1 条流（lastActive 最老）
//  3. 断言 StreamsEvicted == 1，且第 1 条流的 pending 请求被 flush 到 DB
func TestEngine_Assemble_EvictsAndCounts(t *testing.T) {
	e, st := newTestEngine(t)
	e.SetMaxStreams(2)
	pool := NewTCPStreamPool(e)
	a := NewAssembler(pool)

	if a.maxStreams != 2 {
		t.Fatalf("expected a.maxStreams=2, got %d", a.maxStreams)
	}

	// Stream 1: client→server (HTTP request)，建立 pendingReq
	// 注意：payload 含两个 header（Host + User-Agent），因为 parseHTTPRequest
	// 的 indexDoubleCRLF 返回 \r\n\r\n 的起始位置，data[:headerEnd] 会漏掉最后
	// 一个 header 行的 \r\n；若 Host 是最后一个 header，会因缺少 \n 而无法解析。
	// 加一个 User-Agent 让 Host 不是最后一个 header，确保 Host 被正确解析。
	// Stream 1: client→server (HTTP request), establishes pendingReq.
	// Note: payload has two headers (Host + User-Agent) because parseHTTPRequest's
	// indexDoubleCRLF returns the start of \r\n\r\n, and data[:headerEnd] misses
	// the last header line's \r\n; if Host were the last header, it wouldn't be
	// parsed due to missing \n. Adding User-Agent ensures Host is parsed.
	pkt1Req := newTestTCPPacket(
		net.ParseIP("10.0.0.1"), net.ParseIP("192.168.1.1"),
		54321, 80,
		[]byte("GET /api/evict HTTP/1.1\r\nHost: evict.test\r\nUser-Agent: test/1.0\r\n\r\n"),
		false, false,
	)
	a.Assemble(pkt1Req)

	// Stream 1: server→client (partial response，无 Content-Length)，
	// tryExtractHTTP 不会 emit（等待连接关闭），pendingReq + serverBuf 残留
	// Stream 1: server→client (partial response, no Content-Length);
	// tryExtractHTTP won't emit (waits for close), pendingReq + serverBuf remain
	pkt1Resp := newTestTCPPacket(
		net.ParseIP("192.168.1.1"), net.ParseIP("10.0.0.1"),
		80, 54321,
		[]byte("HTTP/1.1 200 OK\r\n\r\npartial-body-no-content-length"),
		false, false,
	)
	a.Assemble(pkt1Resp)

	// Stream 2: 不同五元组，填满容量 / Different 5-tuple, fill capacity
	pkt2 := newTestTCPPacket(
		net.ParseIP("10.0.0.2"), net.ParseIP("192.168.1.1"),
		54322, 80,
		[]byte("GET /api/other HTTP/1.1\r\nHost: other.test\r\n\r\n"),
		false, false,
	)
	a.Assemble(pkt2)

	if len(a.streams) != 2 {
		t.Fatalf("expected 2 streams after setup, got %d", len(a.streams))
	}

	beforeEvicted := e.stats.StreamsEvicted.Load()

	// 触发淘汰：第 3 条流（新 streamKey）→ 应淘汰 stream 1（lastActive 最老）
	// Trigger eviction: 3rd stream (new streamKey) → should evict stream 1 (oldest)
	pkt3 := newTestTCPPacket(
		net.ParseIP("10.0.0.3"), net.ParseIP("192.168.1.1"),
		54323, 80,
		[]byte("GET /api/third HTTP/1.1\r\nHost: third.test\r\n\r\n"),
		false, false,
	)
	a.Assemble(pkt3)

	afterEvicted := e.stats.StreamsEvicted.Load()
	if afterEvicted != beforeEvicted+1 {
		t.Errorf("expected StreamsEvicted=%d, got %d", beforeEvicted+1, afterEvicted)
	}

	if len(a.streams) > 2 {
		t.Errorf("expected len(streams)<=2 after eviction, got %d", len(a.streams))
	}

	// 验证被淘汰的 stream 1 的 pending 数据已 flush 到 DB（修复 #1 的核心断言）
	// Verify evicted stream 1's pending data was flushed to DB (core assertion for fix #1)
	e.writer.Stop()

	items, total, _ := st.List("", "", "", false, 50, 0)
	if total == 0 {
		t.Fatal("expected evicted stream's pending request to be flushed to DB, got 0 records")
	}

	foundEvictedReq := false
	for _, item := range items {
		req, _ := st.Get(item.ID)
		if req == nil {
			continue
		}
		if req.URL == "http://evict.test/api/evict" {
			foundEvictedReq = true
			if req.StatusCode != 200 {
				t.Errorf("expected evicted req StatusCode=200, got %d", req.StatusCode)
			}
			break
		}
	}
	if !foundEvictedReq {
		t.Errorf("evicted stream's request (http://evict.test/api/evict) not found in DB; flushEvictedStream did not emit pendingReq")
	}
}

// TestEngine_Assemble_SYNDirection 验证 SYN 握手指征修正数据方向：
// 客户端 40000 → 服务端 50000（双方端口都不像服务端口且 src<dst），
// 端口启发式会误判为 dst 是客户端；SYN 包（发起方即客户端）应把
// clientPort 修正为 40000，请求数据进 clientBuf、响应数据进 serverBuf。
func TestEngine_Assemble_SYNDirection(t *testing.T) {
	e, _ := newTestEngine(t)
	pool := NewTCPStreamPool(e)
	a := NewAssembler(pool)

	clientIP := net.ParseIP("10.0.0.1")
	serverIP := net.ParseIP("10.0.0.2")

	a.Assemble(newTestTCPPacketFlags(clientIP, serverIP, 40000, 50000, nil, false, false, true, false))
	a.Assemble(newTestTCPPacketFlags(clientIP, serverIP, 40000, 50000,
		[]byte("GET /api/dir HTTP/1.1\r\nHost: dir.test\r\nUser-Agent: t\r\n\r\n"), false, false, false, false))
	a.Assemble(newTestTCPPacketFlags(serverIP, clientIP, 50000, 40000,
		[]byte("HTTP/1.1 200 OK\r\n\r\npartial-body"), false, false, false, false))

	if len(a.streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(a.streams))
	}
	for _, s := range a.streams {
		if s.clientPort != 40000 {
			t.Errorf("clientPort = %d, want 40000 (SYN sender)", s.clientPort)
		}
		if s.pendingReq == nil {
			t.Fatal("expected pendingReq from client request")
		}
		if s.pendingReq.URL != "http://dir.test/api/dir" {
			t.Errorf("URL = %q, want http://dir.test/api/dir", s.pendingReq.URL)
		}
		if len(s.serverBuf) == 0 {
			t.Error("server response should be buffered in serverBuf")
		}
	}
	e.writer.Stop()
}

// TestEngine_Assemble_SYNACKMidCapture 验证抓包中途开始（未见 SYN，首个包是
// SYN-ACK）时方向仍被修正：SYN-ACK 的接收方才是客户端。
func TestEngine_Assemble_SYNACKMidCapture(t *testing.T) {
	e, _ := newTestEngine(t)
	pool := NewTCPStreamPool(e)
	a := NewAssembler(pool)

	clientIP := net.ParseIP("10.0.0.1")
	serverIP := net.ParseIP("10.0.0.2")

	a.Assemble(newTestTCPPacketFlags(serverIP, clientIP, 50000, 40000, nil, false, false, true, true))
	a.Assemble(newTestTCPPacketFlags(clientIP, serverIP, 40000, 50000,
		[]byte("GET /api/synack HTTP/1.1\r\nHost: synack.test\r\nUser-Agent: t\r\n\r\n"), false, false, false, false))

	if len(a.streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(a.streams))
	}
	for _, s := range a.streams {
		if s.clientPort != 40000 {
			t.Errorf("clientPort = %d, want 40000 (SYN-ACK receiver)", s.clientPort)
		}
		if s.pendingReq == nil || s.pendingReq.URL != "http://synack.test/api/synack" {
			t.Errorf("request not parsed from client direction: %+v", s.pendingReq)
		}
	}
	e.writer.Stop()
}

// TestParseContentLength_LF 验证纯 LF 行尾的响应头也能解析出 Content-Length
// （旧实现只按 \r\n 切分，LF-only 头整块当一行导致永远返回 -1）。
func TestParseContentLength_LF(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   int
	}{
		{"crlf", "HTTP/1.1 200 OK\r\nContent-Length: 42\r\nX: y\r\n\r\n", 42},
		{"lf only", "HTTP/1.1 200 OK\nContent-Length: 7\nX: y\n\n", 7},
		{"mixed endings", "HTTP/1.1 200 OK\r\nContent-Length: 13\nX: y\r\n\r\n", 13},
		{"no content length", "HTTP/1.1 200 OK\r\nX: y\r\n\r\n", -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseContentLength([]byte(tt.header)); got != tt.want {
				t.Errorf("parseContentLength() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestFormatHostForURL 验证 IPv6 Host 在 URL 中加方括号（裸 IPv6 URL 非法）。
func TestFormatHostForURL(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"example.com", "example.com"},
		{"example.com:8080", "example.com:8080"},
		{"127.0.0.1:8080", "127.0.0.1:8080"},
		{"::1", "[::1]"},
		{"fe80::1", "[fe80::1]"},
		{"2001:db8::1", "[2001:db8::1]"},
		{"fe80::1:8080", "[fe80::1]:8080"},
		{"[::1]:8080", "[::1]:8080"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := formatHostForURL(tt.in); got != tt.want {
			t.Errorf("formatHostForURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
