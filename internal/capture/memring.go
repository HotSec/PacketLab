package capture

import (
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
	Req       *models.CapturedRequest
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

	// buffer 满？覆盖最旧条目
	if next == tail {
		r.tail.Store((tail + 1) & r.mask)
		r.dropped.Add(1)
	}
	r.buf[head] = Record{Req: req, Timestamp: time.Now()}
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
		reqs[i] = r.Req
	}
	// 分批写入（每批 500 条）
	for i := 0; i < len(reqs); i += 500 {
		end := i + 500
		if end > len(reqs) {
			end = len(reqs)
		}
		if _, err := p.store.SaveBatch(reqs[i:end]); err == nil {
			p.written.Add(uint64(end - i))
		}
	}
}

// Written 已写入计数
func (p *AsyncWriterPool) Written() uint64 {
	return p.written.Load()
}
