package proxy

import (
	"log/slog"
	"sync"
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
	select {
	case bw.ch <- req:
	default:
		slog.Warn("batch channel full, fallback sync write")
		if id, err := bw.store.Save(req); err == nil {
			req.ID = id
			if bw.onSave != nil {
				bw.onSave(req)
			}
		}
	}
}

// Stop 停止批量写入器
func (bw *BatchWriter) Stop() {
	close(bw.stopCh)
	bw.wg.Wait()
	for {
		select {
		case req := <-bw.ch:
			bw.store.Save(req)
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
		ids, err := bw.store.SaveBatch(batch)
		if err != nil {
			slog.Error("batch write failed", "error", err, "count", n)
		} else {
			for i, req := range batch {
				if i < len(ids) {
					req.ID = ids[i]
				}
				if bw.onSave != nil {
					bw.onSave(req)
				}
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
