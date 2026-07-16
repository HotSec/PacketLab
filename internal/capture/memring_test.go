package capture

import (
	"sync"
	"testing"
	"time"

	"packetlab/internal/models"
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
