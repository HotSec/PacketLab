package proxy

import (
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"packetlab/internal/models"
	"packetlab/internal/store"
)

// TestBatchWriter_ErrorNotCallsOnSave 验证 SaveBatch 持续失败时 onSave 回调不被调用。
//
// Bug 2 回归保护：构造一个已关闭的 store 使 SaveBatch 与 sync Save 均失败，
// 确认 BatchWriter 不会触发 onSave（避免前端收到 DB 不存在 ID 的"新请求"）。
// 覆盖两条路径：
//  1. flush 内 SaveBatch 重试 3 次失败 → 进入 sync Save fallback
//  2. sync Save fallback 也失败 → 仅 log，不调用 onSave
func TestBatchWriter_ErrorNotCallsOnSave(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	// 关闭 store 使所有写操作（SaveBatch / Save）均失败
	st.Close()

	var onSaveCalls int32
	bw := NewBatchWriter(st, func(req *models.CapturedRequest) {
		atomic.AddInt32(&onSaveCalls, 1)
	}, 10, 20*time.Millisecond)

	// 入队 2 条请求，触发 batch flush
	bw.Enqueue(&models.CapturedRequest{
		Method: "GET", URL: "http://a", Host: "a", Path: "/", Protocol: "HTTP/1.1",
		CapturedAt: time.Now(),
	})
	bw.Enqueue(&models.CapturedRequest{
		Method: "POST", URL: "http://b", Host: "b", Path: "/", Protocol: "HTTP/1.1",
		CapturedAt: time.Now(),
	})

	// Stop 会等待所有 worker 退出，并 flush 剩余 batch（含上述 2 条请求）
	bw.Stop()

	if n := atomic.LoadInt32(&onSaveCalls); n != 0 {
		t.Errorf("expected 0 onSave calls when SaveBatch fails, got %d", n)
	}
}

// TestBatchWriter_SuccessCallsOnSave 验证 SaveBatch 成功时 onSave 被调用且 ID 被正确回填。
// 作为正向用例，与 TestBatchWriter_ErrorNotCallsOnSave 形成对照，确保回调路径本身可用。
func TestBatchWriter_SuccessCallsOnSave(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()

	var onSaveCalls int32
	bw := NewBatchWriter(st, func(req *models.CapturedRequest) {
		atomic.AddInt32(&onSaveCalls, 1)
	}, 10, 20*time.Millisecond)

	bw.Enqueue(&models.CapturedRequest{
		Method: "GET", URL: "http://a", Host: "a", Path: "/", Protocol: "HTTP/1.1",
		CapturedAt: time.Now(),
	})

	bw.Stop()

	if n := atomic.LoadInt32(&onSaveCalls); n != 1 {
		t.Errorf("expected 1 onSave call on success, got %d", n)
	}
}

// TestBatchWriter_OnSaveCalledWithCorrectID 验证 onSave 回调收到的请求 ID 被正确回填。
// BatchWriter 成功保存后应将 DB 分配的 ID 写回 req.ID，供前端引用。
func TestBatchWriter_OnSaveCalledWithCorrectID(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	st, err := store.New(dbPath)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()

	var savedID int64
	bw := NewBatchWriter(st, func(req *models.CapturedRequest) {
		savedID = req.ID
	}, 10, 20*time.Millisecond)
	// NewBatchWriter 内部已启动 worker goroutine，无需单独 Start。

	req := &models.CapturedRequest{
		Method: "GET", URL: "http://test.example/",
		Host: "test.example", Path: "/", Protocol: "HTTP/1.1",
		CapturedAt: time.Now(),
	}
	bw.Enqueue(req)
	bw.Stop() // 等待 flush 完成

	if savedID == 0 {
		t.Fatal("expected onSave called with non-zero ID")
	}
	// 验证 DB 中确实存在该记录
	got, err := st.Get(savedID)
	if err != nil {
		t.Fatalf("st.Get(%d): %v", savedID, err)
	}
	if got.URL != "http://test.example/" {
		t.Errorf("DB URL = %q, want http://test.example/", got.URL)
	}
}
