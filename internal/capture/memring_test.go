package capture

import (
	"sync"
	"testing"
	"time"

	"packetlab/internal/models"
	"packetlab/internal/store"
)

// TestMemRingBuffer_Concurrent 验证多生产者场景下无数据竞争。
// 旧实现（atomic head/tail + 普通 buf[i] 写）在 -race 下必报错。
func TestMemRingBuffer_Concurrent(t *testing.T) {
	ring := NewMemRingBuffer(1024)
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				ring.Push(&models.CapturedRequest{
					URL:    "http://test",
					Method: "GET",
					Path:   "/",
				})
			}
		}(i)
	}

	done := make(chan struct{})
	go func() {
		for {
			batch := ring.PopBatch()
			if len(batch) == 0 {
				select {
				case <-done:
					return
				case <-time.After(time.Millisecond):
					continue
				}
			}
			select {
			case <-done:
				return
			default:
			}
		}
	}()

	wg.Wait()
	ring.Stop()
	close(done)
}

// TestMemRingBuffer_PushAfterStop 验证 Stop 后 Push 返回 false。
func TestMemRingBuffer_PushAfterStop(t *testing.T) {
	ring := NewMemRingBuffer(64)
	ring.Stop()
	if ring.Push(&models.CapturedRequest{URL: "http://test"}) {
		t.Fatal("expected Push to return false after Stop")
	}
}

// TestMemRingBuffer_Overwrite 验证缓冲区满时淘汰最老记录并计数。
func TestMemRingBuffer_Overwrite(t *testing.T) {
	ring := NewMemRingBuffer(8)
	for i := 0; i < 16; i++ {
		ring.Push(&models.CapturedRequest{URL: "http://test"})
	}
	if ring.Dropped() == 0 {
		t.Fatal("expected overwrites > 0")
	}
}

// TestMemRingBuffer_PopBatchBlocked 验证空缓冲区时 PopBatch 阻塞，
// 在 Push 后被 cond.Signal 唤醒并返回该条记录。
func TestMemRingBuffer_PopBatchBlocked(t *testing.T) {
	ring := NewMemRingBuffer(64)
	started := make(chan struct{})
	done := make(chan []Record, 1)

	go func() {
		close(started)
		batch := ring.PopBatch()
		done <- batch
	}()

	<-started
	time.Sleep(50 * time.Millisecond)
	ring.Push(&models.CapturedRequest{URL: "http://test/42"})

	select {
	case batch := <-done:
		if len(batch) != 1 || batch[0].Req.URL != "http://test/42" {
			t.Fatalf("unexpected batch: %v", batch)
		}
	case <-time.After(time.Second):
		t.Fatal("PopBatch did not unblock after Push")
	}
	ring.Stop()
}

// TestMemRingBuffer_PopBatchDrainsAfterStop 验证 Stop 后 PopBatch 仍能取出残留数据。
func TestMemRingBuffer_PopBatchDrainsAfterStop(t *testing.T) {
	ring := NewMemRingBuffer(64)
	ring.Push(&models.CapturedRequest{URL: "http://test/1"})
	ring.Push(&models.CapturedRequest{URL: "http://test/2"})

	ring.Stop()
	batch := ring.PopBatch()
	if len(batch) != 2 {
		t.Fatalf("expected 2 records after Stop, got %d", len(batch))
	}
	if batch[0].Req.URL != "http://test/1" || batch[1].Req.URL != "http://test/2" {
		t.Fatalf("unexpected order: %v", batch)
	}

	// 再 Pop 应为 nil
	if b := ring.PopBatch(); len(b) != 0 {
		t.Fatalf("expected nil after drain, got %d", len(b))
	}
}

// newClosedStore 创建一个 DB 已关闭的 store，用于让 SaveBatch / Save 必失败。
// 返回的 store 无需再次 Close（已关闭）。
func newClosedStore(t *testing.T) *store.Store {
	t.Helper()
	dbPath := t.TempDir() + "/closed.db"
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}
	return st
}

// TestAsyncWriterPool_FlushAll_BackoffBetweenRetries 验证 SaveBatch 失败时
// 3 次重试之间存在指数退避（50/100ms），总耗时 >= 100ms。
//
// 修改前（无退避）：3 次重试瞬间完成，耗时 < 50ms → 测试 FAIL。
// 修改后（有退避）：50+100ms 退避，耗时 >= 150ms → 测试 PASS。
func TestAsyncWriterPool_FlushAll_BackoffBetweenRetries(t *testing.T) {
	st := newClosedStore(t)

	buf := NewMemRingBuffer(64)
	buf.Push(&models.CapturedRequest{
		URL: "http://backoff.test", Method: "GET",
		Host: "backoff.test", Path: "/", Protocol: "HTTP/1.1",
	})

	e := &Engine{
		store:     st,
		procCache: make(map[string]*models.ProcessInfo),
	}
	p := NewAsyncWriterPool(st, buf, 1, 10*time.Millisecond)
	p.engine = e

	start := time.Now()
	p.flushAll()
	elapsed := time.Since(start)

	// 退避总和 50+100=150ms（最后一次失败不退避）。
	// 阈值 100ms 清晰区分「无退避 ~0ms」与「有退避 ~150ms」。
	if elapsed < 100*time.Millisecond {
		t.Fatalf("expected >= 100ms with exponential backoff (50+100ms), got %v", elapsed)
	}
}

// TestAsyncWriterPool_FlushAll_StopChEarlyExit 验证 stopCh 关闭时
// flushAll 不会在退避上阻塞过久（应 < 100ms，远小于完整退避 150ms）。
//
// 这是 stopCh 早退路径的回归保护：修改后若有人误删 select 中的 stopCh 分支，
// 此测试将因 elapsed >= 100ms 而 FAIL。
func TestAsyncWriterPool_FlushAll_StopChEarlyExit(t *testing.T) {
	st := newClosedStore(t)

	buf := NewMemRingBuffer(64)
	buf.Push(&models.CapturedRequest{
		URL: "http://stop.test", Method: "GET",
		Host: "stop.test", Path: "/", Protocol: "HTTP/1.1",
	})

	e := &Engine{
		store:     st,
		procCache: make(map[string]*models.ProcessInfo),
	}
	p := NewAsyncWriterPool(st, buf, 1, 10*time.Millisecond)
	p.engine = e
	close(p.stopCh) // 模拟 Stop 信号已发出

	start := time.Now()
	p.flushAll()
	elapsed := time.Since(start)

	// stopCh 关闭后，第一次退避即早退，应远小于完整 150ms 退避。
	if elapsed >= 100*time.Millisecond {
		t.Fatalf("expected early exit < 100ms when stopCh closed, got %v", elapsed)
	}
}
