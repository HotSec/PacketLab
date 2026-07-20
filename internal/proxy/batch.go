package proxy

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"packetlab/internal/models"
	"packetlab/internal/store"
)

// BatchWriter 批量写入器，多 worker 高吞吐
type BatchWriter struct {
	store     *store.Store
	ch        chan *models.CapturedRequest
	onSave    func(req *models.CapturedRequest)
	wg        sync.WaitGroup
	stopCh    chan struct{}
	batchSize int
	interval  time.Duration
	workers   int
	stopped   int32 // atomic; 非 0 表示已 Stop，Enqueue 走 sync 路径避免 channel 滞留
}

// NewBatchWriter 创建批量写入器
func NewBatchWriter(st *store.Store, onSave func(req *models.CapturedRequest), batchSize int, flushInterval time.Duration) *BatchWriter {
	if batchSize <= 0 {
		batchSize = 500
	}
	if flushInterval <= 0 {
		flushInterval = 50 * time.Millisecond
	}
	workers := 2

	bw := &BatchWriter{
		store:     st,
		ch:        make(chan *models.CapturedRequest, 16384),
		onSave:    onSave,
		stopCh:    make(chan struct{}),
		batchSize: batchSize,
		interval:  flushInterval,
		workers:   workers,
	}
	for i := 0; i < workers; i++ {
		bw.wg.Add(1)
		go bw.loop()
	}
	return bw
}

// Enqueue 入队一条请求
func (bw *BatchWriter) Enqueue(req *models.CapturedRequest) {
	// Stop 后 worker 已退出，channel 无人消费；走 sync 路径避免 req 滞留 channel
	if atomic.LoadInt32(&bw.stopped) != 0 {
		bw.saveSync(req, "stopped")
		return
	}
	select {
	case bw.ch <- req:
	default:
		slog.Warn("batch channel full, fallback sync write")
		bw.saveSync(req, "channel full")
	}
}

// saveSync 同步保存兜底，失败时记录日志（避免错误被静默吞掉）。
func (bw *BatchWriter) saveSync(req *models.CapturedRequest, reason string) {
	id, err := bw.store.Save(req)
	if err != nil {
		slog.Error("sync save fallback failed", "url", req.URL, "reason", reason, "error", err)
		return
	}
	req.ID = id
	if bw.onSave != nil {
		bw.onSave(req)
	}
}

// Stop 停止批量写入器
func (bw *BatchWriter) Stop() {
	// CAS 防止重复 Stop；stopped=1 后 Enqueue 走 sync 路径，避免与 drain 竞争 channel
	if !atomic.CompareAndSwapInt32(&bw.stopped, 0, 1) {
		return
	}
	close(bw.stopCh)
	bw.wg.Wait()
	// drain channel：worker 已退出，安全消费残留 req
	for {
		select {
		case req := <-bw.ch:
			bw.saveSync(req, "drain")
		default:
			return
		}
	}
}

func (bw *BatchWriter) loop() {
	defer bw.wg.Done()

	batch := make([]*models.CapturedRequest, 0, bw.batchSize)
	ticker := time.NewTicker(bw.interval)
	defer ticker.Stop()

	flush := func() {
		n := len(batch)
		if n == 0 {
			return
		}

		saved := false
		for attempt := 1; attempt <= 3; attempt++ {
			ids, err := bw.store.SaveBatch(batch)
			if err == nil {
				for i, req := range batch {
					if i < len(ids) {
						req.ID = ids[i]
					}
					if bw.onSave != nil {
						bw.onSave(req)
					}
				}
				saved = true
				break
			}
			slog.Warn("batch write failed, retrying", "attempt", attempt, "error", err, "count", n)
		}

		if !saved {
			slog.Error("batch write failed after 3 retries, falling back to sync", "count", n)
			for _, req := range batch {
				bw.saveSync(req, "loop fallback")
			}
		}
		batch = batch[:0]
	}

	for {
		select {
		case req := <-bw.ch:
			batch = append(batch, req)
			if len(batch) >= bw.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-bw.stopCh:
			for {
				select {
				case req := <-bw.ch:
					batch = append(batch, req)
				default:
					flush()
					return
				}
			}
		}
	}
}
