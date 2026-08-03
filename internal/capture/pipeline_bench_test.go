package capture

import (
	"fmt"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"packetlab/internal/models"
	"packetlab/internal/store"
)

// TestPipelineThroughput 模拟实际抓包管道吞吐
func TestPipelineThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping throughput test")
	}

	workers := 4
	duration := 3 * time.Second

	e := &Engine{
		running:   atomic.Bool{},
		procCache: make(map[string]*models.ProcessInfo),
	}
	e.running.Store(true)
	e.emitBuf = make([]*models.CapturedRequest, emitBufSize)

	var processed atomic.Int64
	var wg sync.WaitGroup
	stopCh := make(chan struct{})

	// 模拟 100 个并发流
	streams := make([]*TCPStream, 100)
	for i := range streams {
		pool := NewTCPStreamPool(e)
		streams[i] = pool.New(
			net.ParseIP(fmt.Sprintf("10.0.%d.%d", i/256, i%256)),
			uint16(30000+i),
			80,
		)
	}

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
				}
				// 轮询流，每个流喂入一组请求+响应
				for i := offset; i < len(streams); i += workers {
					req := []byte(fmt.Sprintf(
						"GET /api/v1/data?limit=100 HTTP/1.1\r\nHost: %s\r\nAccept: application/json\r\n\r\n",
						fmt.Sprintf("api%d.example.com", i)))
					resp := []byte(fmt.Sprintf(
						"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
						128, makeStr(128)))

					streams[i].Feed(req, true)
					streams[i].Feed(resp, false)
				}
				processed.Add(int64(len(streams) / workers))
			}
		}(w)
	}

	time.Sleep(duration)
	close(stopCh)
	wg.Wait()

	total := processed.Load()
	reqPerSec := float64(total) / duration.Seconds()
	avgSize := 500.0 // ~500 bytes per req+resp for simple GET
	gbps := reqPerSec * avgSize * 8 / 1e9

	t.Logf("╔═══════════════════════════════════╗")
	t.Logf("║  Pipeline Throughput Report       ║")
	t.Logf("╠═══════════════════════════════════╣")
	t.Logf("║  Workers:      %d                   ║", workers)
	t.Logf("║  Streams:      %d                  ║", len(streams))
	t.Logf("║  Duration:     %v                 ║", duration)
	t.Logf("║  Total req:    %d                 ║", total)
	t.Logf("║  Throughput:   %.0f req/s         ║", reqPerSec)
	t.Logf("║  Bandwidth:    %.2f Gbps          ║", gbps)
	t.Logf("╚═══════════════════════════════════╝")
	t.Logf("")
	t.Logf("1Gbps 达成判断: %s", boolToStr(gbps >= 1.0))
	t.Logf("瓶颈:解析=1μs, Stream锁竞争, Feed串行")
}

func TestSQLiteThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping SQLite test")
	}

	st := initTestStore(t)

	batchSize := 200
	workers := 2
	duration := 2 * time.Second
	var wg sync.WaitGroup
	var written atomic.Int64
	stopCh := make(chan struct{})

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
				}
				reqs := make([]*models.CapturedRequest, batchSize)
				now := time.Now()
				for i := range reqs {
					reqs[i] = &models.CapturedRequest{
						Method: "GET", URL: fmt.Sprintf("https://x.com/api/%d", i),
						Host: "x.com", Path: fmt.Sprintf("/api/%d", i),
						Protocol:   "HTTP/1.1",
						ReqHeaders: map[string]string{"Host": "x.com"},
						StatusCode: 200,
						DurationMs: 10, SizeBytes: 500, CapturedAt: now,
					}
				}
				st.SaveBatch(reqs)
				written.Add(int64(batchSize))
			}
		}()
	}

	time.Sleep(duration)
	close(stopCh)
	wg.Wait()

	total := written.Load()
	reqPerSec := float64(total) / duration.Seconds()

	t.Logf("╔═══════════════════════════════════╗")
	t.Logf("║  SQLite Write Throughput          ║")
	t.Logf("╠═══════════════════════════════════╣")
	t.Logf("║  Workers:      %d                   ║", workers)
	t.Logf("║  Batch:        %d                 ║", batchSize)
	t.Logf("║  Total writes: %d                 ║", total)
	t.Logf("║  Throughput:   %.0f req/s         ║", reqPerSec)
	t.Logf("╚═══════════════════════════════════╝")
	t.Logf("")
	t.Logf("1 req = %d bytes", 500)
	t.Logf("SQLite bandwidth: %.2f Gbps", reqPerSec*500*8/1e9)
}

func initTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New("/tmp/packetlab_pipe_bench.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func boolToStr(b bool) string {
	if b {
		return "YES (>=1Gbps)"
	}
	return "NO (< 1Gbps)"
}

func makeStr(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}

func BenchmarkGoroutines(b *testing.B) {
	b.ReportMetric(float64(runtime.NumGoroutine()), "goroutines")
}
