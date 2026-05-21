package proxy

import (
	"log"
	"sync"
	"time"

	"packetlab/internal/models"
	"packetlab/internal/store"
)

// BatchWriter 批量写入器，高流量下聚合写入减少 SQLite 锁竞争
type BatchWriter struct {
	store    *store.Store
	ch       chan *models.CapturedRequest
	onSave   func(req *models.CapturedRequest)
	wg       sync.WaitGroup
	stopCh   chan struct{}
	batchSize int
	interval  time.Duration
}

// NewBatchWriter 创建批量写入器
func NewBatchWriter(st *store.Store, onSave func(req *models.CapturedRequest), batchSize int, flushInterval time.Duration) *BatchWriter {
	if batchSize <= 0 {
		batchSize = 50
	}
	if flushInterval <= 0 {
		flushInterval = 200 * time.Millisecond
	}

	bw := &BatchWriter{
		store:     st,
		ch:        make(chan *models.CapturedRequest, 2048),
		onSave:    onSave,
		stopCh:    make(chan struct{}),
		batchSize: batchSize,
		interval:  flushInterval,
	}
	bw.wg.Add(1)
	go bw.loop()
	return bw
}

// Enqueue 入队一条请求
func (bw *BatchWriter) Enqueue(req *models.CapturedRequest) {
	select {
	case bw.ch <- req:
	default:
		// 通道满则丢弃（大流量保护）
		log.Printf("[batch] 写入通道满，丢弃请求 %s %s", req.Method, req.URL)
	}
}

// Stop 停止批量写入器
func (bw *BatchWriter) Stop() {
	close(bw.stopCh)
	bw.wg.Wait()
}

func (bw *BatchWriter) loop() {
	defer bw.wg.Done()

	batch := make([]*models.CapturedRequest, 0, bw.batchSize)
	ticker := time.NewTicker(bw.interval)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		ids, err := bw.store.SaveBatch(batch)
		if err != nil {
			log.Printf("[batch] 批量写入失败: %v", err)
		} else {
			for i, req := range batch {
				if i < len(ids) {
					req.ID = ids[i]
				}
				if bw.onSave != nil {
					bw.onSave(req)
				}
			}
			if len(ids) > 1 {
				log.Printf("[batch] 批量写入 %d 条", len(ids))
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
			// 排空剩余
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
