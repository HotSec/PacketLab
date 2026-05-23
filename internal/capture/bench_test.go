package capture

import (
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"packetlab/internal/models"
	"packetlab/internal/store"
)

func BenchmarkParseHTTPRequest(b *testing.B) {
	raw := []byte("GET /api/v1/users/12345/profile?fields=name,email,avatar HTTP/1.1\r\nHost: api.example.com\r\nContent-Type: application/json\r\nUser-Agent: PacketLab/1.0\r\nAccept: application/json\r\nContent-Length: 42\r\n\r\n" + `{"name":"test","value":12345}`)
	ip := net.ParseIP("192.168.1.100")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		parseHTTPRequest(raw, ip, 54321, 80, nil)
	}
}

func BenchmarkParseHTTPResponse(b *testing.B) {
	raw := []byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 128\r\nServer: nginx\r\n\r\n" + `{"data":[{"id":1}],"total":100}`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		parseHTTPResponse(raw)
	}
}

func BenchmarkFindHTTPMessageEnd(b *testing.B) {
	body := make([]byte, 2048)
	raw := []byte("POST /api/submit HTTP/1.1\r\nHost: example.com\r\nContent-Type: application/json\r\nContent-Length: 2048\r\n\r\n")
	raw = append(raw, body...)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		findHTTPMessageEnd(raw)
	}
}

func BenchmarkStreamFeed(b *testing.B) {
	httpReq := []byte("GET /api/test HTTP/1.1\r\nHost: bench.local\r\n\r\n")
	httpResp := []byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")

	e := &Engine{
		running:   atomic.Bool{},
		procCache: make(map[string]*models.ProcessInfo),
	}
	e.running.Store(true)
	pool := NewTCPStreamPool(e)
	stream := pool.New(net.ParseIP("10.0.0.1"), 12345, net.ParseIP("10.0.0.2"), 80)

	b.ResetTimer()
	b.SetBytes(int64(len(httpReq) + len(httpResp)))
	for i := 0; i < b.N; i++ {
		stream.Feed(httpReq, true)
		stream.Feed(httpResp, false)
	}
}

func BenchmarkStoreBatchWrite(b *testing.B) {
	st, err := store.New("/tmp/packetlab_bench.db")
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()

	batchSize := 200
	reqs := make([]*models.CapturedRequest, batchSize)
	now := time.Now()
	for i := range reqs {
		reqs[i] = &models.CapturedRequest{
			Method: "GET", URL: fmt.Sprintf("https://bench.example.com/api/%d", i),
			Host: "bench.example.com", Path: fmt.Sprintf("/api/%d", i),
			Protocol: "HTTP/1.1", ReqHeaders: map[string]string{"Host": "bench"},
			StatusCode: 200, ResHeaders: map[string]string{"Content-Type": "text"},
			DurationMs: 15, SizeBytes: 256, CapturedAt: now,
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		st.SaveBatch(reqs)
	}
}

func BenchmarkSafeTruncate(b *testing.B) {
	// 构造 512KB 含多个完整消息的 buffer
	msg := []byte("GET /api/x HTTP/1.1\r\nHost: bench\r\n\r\n")
	buf := make([]byte, 0, 512*1024)
	for len(buf) < 512*1024 {
		buf = append(buf, msg...)
	}
	s := &TCPStream{buf: buf}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.safeTruncate()
	}
}
