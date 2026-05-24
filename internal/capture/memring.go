package capture

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"packetlab/internal/models"
	"packetlab/internal/store"
)

// MemRingBuffer 内存环形缓冲区，支撑 2.5Gbps 突发流量
type MemRingBuffer struct {
	buf     []Record
	mask    uint64
	head    atomic.Uint64
	tail    atomic.Uint64
	dropped atomic.Uint64
	mu      sync.Mutex
}

type Record struct {
	Req       models.CapturedRequest
	Timestamp time.Time
}

const ringSize = 128 * 1024 * 1024 // 128MB

// NewMemRingBuffer 创建环形缓冲区
func NewMemRingBuffer(entryCount int) *MemRingBuffer {
	if entryCount < 65536 {
		entryCount = 65536
	}
	// 对齐到 2 的幂
	size := 1
	for size < entryCount {
		size <<= 1
	}
	return &MemRingBuffer{
		buf:  make([]Record, size),
		mask: uint64(size - 1),
	}
}

// Push 写入一条记录（非阻塞）
func (r *MemRingBuffer) Push(req *models.CapturedRequest) {
	head := r.head.Load()
	next := (head + 1) & r.mask
	tail := r.tail.Load()

	if next == tail {
		r.tail.Store((tail + 1) & r.mask)
		d := r.dropped.Add(1)
		if d%100 == 1 {
			slog.Warn("ring buffer overflow, oldest record dropped", "total_dropped", d, "url", req.URL)
		}
	}
	if req != nil {
		r.buf[head] = Record{Req: *req, Timestamp: time.Now()}
	} else {
		r.buf[head] = Record{Timestamp: time.Now()}
	}
	r.head.Store(next)
}

// PopBatch 批量读取（tail 到 head 之间的所有条目）
func (r *MemRingBuffer) PopBatch() []Record {
	head := r.head.Load()
	tail := r.tail.Load()

	if head == tail {
		return nil
	}

	var n uint64
	if head > tail {
		n = head - tail
	} else {
		n = r.mask + 1 - tail + head
	}

	batch := make([]Record, n)
	for i := uint64(0); i < n; i++ {
		idx := (tail + i) & r.mask
		batch[i] = r.buf[idx]
	}
	r.tail.Store(head)
	return batch
}

// Usage 返回使用率 (0.0 - 1.0)
func (r *MemRingBuffer) Usage() float64 {
	head := r.head.Load()
	tail := r.tail.Load()
	if head >= tail {
		return float64(head-tail) / float64(r.mask+1)
	}
	return float64(r.mask+1-tail+head) / float64(r.mask+1)
}

// Dropped 丢弃计数
func (r *MemRingBuffer) Dropped() uint64 {
	return r.dropped.Load()
}

// AsyncWriterPool 后台异步写入池
type AsyncWriterPool struct {
	store    *store.Store
	buf      *MemRingBuffer
	engine   *Engine
	workers  int
	interval time.Duration
	stopCh   chan struct{}
	wg       sync.WaitGroup
	written  atomic.Uint64
}

// NewAsyncWriterPool 创建异步写入池
func NewAsyncWriterPool(st *store.Store, buf *MemRingBuffer, workers int, interval time.Duration) *AsyncWriterPool {
	if workers <= 0 {
		workers = 4
	}
	if interval <= 0 {
		interval = 30 * time.Millisecond
	}
	return &AsyncWriterPool{
		store:    st,
		buf:      buf,
		workers:  workers,
		interval: interval,
		stopCh:   make(chan struct{}),
	}
}

// Start 启动后台写入
func (p *AsyncWriterPool) Start() {
	for i := 0; i < p.workers; i++ {
		p.wg.Add(1)
		go p.loop()
	}
}

// Stop 停止并排空
func (p *AsyncWriterPool) Stop() {
	close(p.stopCh)
	p.wg.Wait()
	// 排空残余
	p.flushAll()
}

func (p *AsyncWriterPool) loop() {
	defer p.wg.Done()
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			p.flushAll()
		case <-p.stopCh:
			return
		}
	}
}

func (p *AsyncWriterPool) flushAll() {
	batch := p.buf.PopBatch()
	if len(batch) == 0 {
		return
	}
	reqs := make([]*models.CapturedRequest, len(batch))
	for i, r := range batch {
		copied := r.Req
		reqs[i] = &copied
	}
	for i := 0; i < len(reqs); i += 500 {
		end := i + 500
		if end > len(reqs) {
			end = len(reqs)
		}
		saved := false
		chunk := reqs[i:end]
		for attempt := 1; attempt <= 3; attempt++ {
			ids, err := p.store.SaveBatch(chunk)
			if err == nil {
				for j, id := range ids {
					if j < len(chunk) {
						chunk[j].ID = id
					}
				}
				p.written.Add(uint64(len(ids)))
				saved = true
				break
			}
			slog.Warn("async writer batch failed, retrying", "attempt", attempt, "error", err, "count", len(chunk))
		}
		if !saved {
			slog.Error("async writer batch failed after 3 retries, falling back to sync", "count", len(chunk))
			for _, req := range chunk {
				if id, err := p.store.Save(req); err == nil {
					req.ID = id
					p.written.Add(1)
				}
			}
		}
		for _, req := range chunk {
			if req.ID > 0 {
				p.engine.stats.HTTPFound.Add(1)
				if p.engine.hub != nil {
					p.engine.hub.BroadcastCapture(req)
				}
			}
		}
	}
}

// Written 已写入计数
func (p *AsyncWriterPool) Written() uint64 {
	return p.written.Load()
}
