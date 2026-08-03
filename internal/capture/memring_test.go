package capture

import (
	"strings"
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

// TestMemRingBuffer_ByteCap 验证字节上限：条目数未满但累计字节超限时同样
// 淘汰最老记录（防止写入方停滞时带大 body 的记录把驻留内存撑爆）。
func TestMemRingBuffer_ByteCap(t *testing.T) {
	ring := NewMemRingBuffer(64)
	ring.maxBytes = 6300

	body := strings.Repeat("x", 60) // recordSize = 60 + 2048 开销
	for i := 1; i <= 3; i++ {
		if !ring.Push(&models.CapturedRequest{URL: "http://cap.test", ResBody: body}) {
			t.Fatal("Push returned false")
		}
	}
	if ring.Dropped() != 1 {
		t.Errorf("Dropped = %d, want 1 (byte cap eviction)", ring.Dropped())
	}
	batch := ring.PopBatch()
	if len(batch) != 2 {
		t.Fatalf("expected 2 surviving records, got %d", len(batch))
	}
	if batch[0].Req.URL != "http://cap.test" || batch[0].Req.ResBody != body {
		t.Errorf("oldest surviving record does not match, got %+v", batch[0])
	}
}

// TestMemRingBuffer_PopBatchResetsBytes 验证 PopBatch 排空后字节账本复位：
// 旧实现 bytes 不归零，排空后继续 Push 会因残留字节误判超限而持续误淘汰
// （且空环淘汰路径会把已排空的 tail 槽计入账本）。
func TestMemRingBuffer_PopBatchResetsBytes(t *testing.T) {
	ring := NewMemRingBuffer(8)
	ring.maxBytes = 6300

	body := strings.Repeat("x", 60) // recordSize = 2108；2 条共 4216 < 6300 不触发淘汰
	for i := 0; i < 2; i++ {
		if !ring.Push(&models.CapturedRequest{URL: "http://cap.test", ResBody: body}) {
			t.Fatal("initial Push returned false")
		}
	}

	batch := ring.PopBatch()
	if len(batch) != 2 {
		t.Fatalf("expected 2 records, got %d", len(batch))
	}

	// 排空后再 Push 同尺寸记录：不得触发误淘汰（旧实现 bytes 残留 4216 > maxBytes 时误判）
	if !ring.Push(&models.CapturedRequest{URL: "http://cap.test", ResBody: body}) {
		t.Fatal("Push after drain returned false")
	}
	if ring.Dropped() != 0 {
		t.Errorf("Dropped = %d, want 0 (no eviction after drain)", ring.Dropped())
	}
	batch = ring.PopBatch()
	if len(batch) != 1 {
		t.Errorf("expected exactly 1 surviving record, got %d", len(batch))
	}
}

// TestMemRingBuffer_FullRingLedger 验证条数上限淘汰路径的字节账本正确性：
// 满环（next==tail）淘汰最老记录时必须归还其字节账，否则 bytes 无限膨胀，
// 会让后续 byte-cap 判定提前触发误淘汰（回归：旧守卫在满环时跳过归还）。
func TestMemRingBuffer_FullRingLedger(t *testing.T) {
	ring := NewMemRingBuffer(2)
	ring.maxBytes = 10 * 1024 * 1024 // 字节上限不参与，专测条数路径

	body := strings.Repeat("x", 60) // recordSize = 2108
	for i := 0; i < 5; i++ {
		if !ring.Push(&models.CapturedRequest{URL: "http://full.test", ResBody: body}) {
			t.Fatal("Push returned false")
		}
	}

	// 环容量 2，连续 5 次 Push 后仅 1 条存活，账本应只计 1 条
	if ring.bytes != 2108 {
		t.Errorf("bytes = %d, want 2108 (single survivor; ledger must not inflate)", ring.bytes)
	}
	if ring.Dropped() != 4 {
		t.Errorf("Dropped = %d, want 4", ring.Dropped())
	}

	batch := ring.PopBatch()
	if len(batch) != 1 {
		t.Fatalf("expected 1 surviving record, got %d", len(batch))
	}
	if ring.bytes != 0 {
		t.Errorf("bytes = %d after drain, want 0", ring.bytes)
	}
}

// TestMemRingBuffer_SingleRecordExceedsByteCap 验证单条记录超过字节上限时直接
// 丢弃（记入 dropped）：不写入环（避免 head==tail 判空导致记录不可见 + PopBatch
// 永久阻塞），且排空后再 Push 超大记录同样安全。
func TestMemRingBuffer_SingleRecordExceedsByteCap(t *testing.T) {
	ring := NewMemRingBuffer(8)
	ring.maxBytes = 1000

	body := strings.Repeat("x", 60) // recordSize = 2108 > 1000
	if !ring.Push(&models.CapturedRequest{URL: "http://oversize.test", ResBody: body}) {
		t.Fatal("Push returned false")
	}
	if ring.Dropped() != 1 {
		t.Errorf("Dropped = %d, want 1", ring.Dropped())
	}
	if ring.bytes != 0 {
		t.Errorf("bytes = %d, want 0 (oversized record must not be stored)", ring.bytes)
	}
	ring.Stop()
	if batch := ring.PopBatch(); len(batch) != 0 {
		t.Errorf("expected empty batch, got %d records", len(batch))
	}
}

// TestMemRingBuffer_ResizeAndOversize 覆盖真实路径：正常记录先填满再排空
// （PopBatch 复位账本），随后超大记录被丢弃而不破坏环状态。
func TestMemRingBuffer_ResizeAndOversize(t *testing.T) {
	ring := NewMemRingBuffer(4)
	ring.maxBytes = 6300

	normal := strings.Repeat("x", 60) // 2108
	for i := 0; i < 2; i++ {
		ring.Push(&models.CapturedRequest{URL: "http://ok.test", ResBody: normal})
	}
	if batch := ring.PopBatch(); len(batch) != 2 {
		t.Fatalf("expected 2 records, got %d", len(batch))
	}

	// 排空后超大记录：不得写入、不得下溢、不得阻塞后续正常 Push
	big := strings.Repeat("y", 9000) // recordSize > 6300
	if !ring.Push(&models.CapturedRequest{URL: "http://big.test", ResBody: big}) {
		t.Fatal("oversized Push returned false")
	}
	if ring.Dropped() != 1 {
		t.Errorf("Dropped = %d, want 1", ring.Dropped())
	}
	if !ring.Push(&models.CapturedRequest{URL: "http://ok2.test", ResBody: normal}) {
		t.Fatal("normal Push after oversized returned false")
	}
	if ring.Dropped() != 1 {
		t.Errorf("Dropped = %d, want still 1 (normal record must not be dropped)", ring.Dropped())
	}
	batch := ring.PopBatch()
	if len(batch) != 1 || batch[0].Req.URL != "http://ok2.test" {
		t.Fatalf("expected the normal record, got %d records", len(batch))
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

// TestMemRingBuffer_CustomSize 验证 NewMemRingBuffer(entryCount) 会向上取 2 的幂。
// 100 → 128；回归保护：若未来有人误改为固定大小，此测试 FAIL。
func TestMemRingBuffer_CustomSize(t *testing.T) {
	ring := NewMemRingBuffer(100) // 会向上取 128
	if cap(ring.buf) != 128 {
		t.Fatalf("expected size 128, got %d", cap(ring.buf))
	}
}

// TestEngine_SetRingBufSize 验证 Engine.SetRingBufSize setter 行为：
//   - 正数入参直接写入 ringBufSize 字段
//   - <=0 入参应被忽略，保持原值不变（默认值兜底逻辑放在 Start 路径中）
//
// 此测试在添加 SetRingBufSize 方法前会编译失败（method undefined）→ RED。
func TestEngine_SetRingBufSize(t *testing.T) {
	e := &Engine{}
	e.SetRingBufSize(1024)
	if e.ringBufSize != 1024 {
		t.Fatalf("expected ringBufSize=1024, got %d", e.ringBufSize)
	}
	// <=0 不应改变
	e.SetRingBufSize(0)
	if e.ringBufSize != 1024 {
		t.Fatalf("expected ringBufSize unchanged=1024, got %d", e.ringBufSize)
	}
	// 负数也应被忽略
	e.SetRingBufSize(-1)
	if e.ringBufSize != 1024 {
		t.Fatalf("expected ringBufSize unchanged=1024 after negative, got %d", e.ringBufSize)
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
