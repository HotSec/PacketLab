package capture

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"packetlab/internal/models"
	"packetlab/internal/store"
)

// MemRingBuffer 内存环形缓冲区，支撑 2.5Gbps 突发流量。
// 多生产者场景下用 sync.Mutex 保护 head/tail/buf，sync.Cond 唤醒阻塞的消费者。
// 旧的 lock-free（atomic head/tail + 普通 buf[i] 写）在多生产者下存在数据竞争：
// buf[head]=Record 不被 atomic 保护，且 tail 存在 lost update。
type MemRingBuffer struct {
	mu         sync.Mutex
	cv         *sync.Cond
	buf        []Record
	head, tail uint64
	mask       uint64
	dropped    atomic.Uint64
	stopped    bool
}

type Record struct {
	Req       models.CapturedRequest
	Timestamp time.Time
}

// NewMemRingBuffer 创建环形缓冲区。size 向上取 2 的幂。
func NewMemRingBuffer(entryCount int) *MemRingBuffer {
	if entryCount <= 0 {
		entryCount = 262144
	}
	// 对齐到 2 的幂
	size := 1
	for size < entryCount {
		size <<= 1
	}
	r := &MemRingBuffer{
		buf:  make([]Record, size),
		mask: uint64(size - 1),
	}
	r.cv = sync.NewCond(&r.mu)
	return r
}

// Push 写入一条记录。缓冲区满时淘汰最老记录。已 Stop 时返回 false。
func (r *MemRingBuffer) Push(req *models.CapturedRequest) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return false
	}
	next := (r.head + 1) & r.mask
	if next == r.tail {
		// 满，淘汰最老
		r.tail = (r.tail + 1) & r.mask
		d := r.dropped.Add(1)
		if d%100 == 1 {
			url := ""
			if req != nil {
				url = req.URL
			}
			slog.Warn("ring buffer overflow, oldest record dropped", "total_dropped", d, "url", url)
		}
	}
	if req != nil {
		r.buf[r.head] = Record{Req: *req, Timestamp: time.Now()}
	} else {
		r.buf[r.head] = Record{Timestamp: time.Now()}
	}
	r.head = next
	r.cv.Signal()
	return true
}

// PopBatch 批量读取（tail 到 head 之间的所有条目）。
// 缓冲区为空且未 Stop 时阻塞，直到有数据或被 Stop 唤醒。
func (r *MemRingBuffer) PopBatch() []Record {
	r.mu.Lock()
	defer r.mu.Unlock()

	for r.head == r.tail && !r.stopped {
		r.cv.Wait()
	}

	if r.head == r.tail {
		return nil // 已 Stop 且为空
	}

	var n uint64
	if r.head > r.tail {
		n = r.head - r.tail
	} else {
		n = r.mask + 1 - r.tail + r.head
	}

	batch := make([]Record, n)
	for i := uint64(0); i < n; i++ {
		idx := (r.tail + i) & r.mask
		batch[i] = r.buf[idx]
	}
	r.tail = r.head
	return batch
}

// Usage 返回使用率 (0.0 - 1.0)
func (r *MemRingBuffer) Usage() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var used uint64
	if r.head >= r.tail {
		used = r.head - r.tail
	} else {
		used = r.mask + 1 - r.tail + r.head
	}
	return float64(used) / float64(r.mask+1)
}

// Dropped 丢弃计数
func (r *MemRingBuffer) Dropped() uint64 {
	return r.dropped.Load()
}

// Stop 停止缓冲区，唤醒所有阻塞在 PopBatch 的消费者。
// Stop 后 Push 返回 false，PopBatch 立即返回剩余数据或 nil。
func (r *MemRingBuffer) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = true
	r.cv.Broadcast()
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
	select {
	case <-p.stopCh:
		return
	default:
	}
	close(p.stopCh)
	// 唤醒所有阻塞在 PopBatch 的 worker，避免 wg.Wait 死锁
	p.buf.Stop()
	p.wg.Wait()
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

// flushBackoffs SaveBatch 失败重试的指数退避间隔。
// slice 长度 = 重试次数（3 次）；最后一次失败不退避，直接进入 sync 兜底，
// 因此最后一个元素 200ms 不会被等待，实际退避总和 = 50 + 100 = 150ms。
// SQLite busy 时 3 次重试瞬间打满会加剧锁竞争，退避让锁有机会释放。
var flushBackoffs = []time.Duration{
	50 * time.Millisecond,
	100 * time.Millisecond,
	200 * time.Millisecond,
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
		var lastErr error
		for attempt := 0; attempt < len(flushBackoffs); attempt++ {
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
			lastErr = err
			slog.Warn("async writer batch failed, retrying",
				"attempt", attempt+1, "error", err, "count", len(chunk))
			// 最后一次不再退避，直接进入 sync 兜底
			if attempt < len(flushBackoffs)-1 {
				select {
				case <-p.stopCh:
					// Stop 信号到来：与规格计划代码的 return 不同，
					// 这里进入 sync 兜底保存当前 chunk 数据（避免丢数据），
					// 后续 chunk 也会立即 fallback。
					goto fallback
				case <-time.After(flushBackoffs[attempt]):
				}
			}
		}
	fallback:
		if !saved {
			slog.Error("async writer batch failed after all retries, falling back to sync",
				"error", lastErr, "count", len(chunk))
			for _, req := range chunk {
				if id, err := p.store.Save(req); err == nil {
					req.ID = id
					p.written.Add(1)
				}
			}
		}
		for _, req := range chunk {
			// HTTPFound 已在 emitNonBlocking 入口计数，这里不再重复 / HTTPFound already counted at emitNonBlocking entry, no double-count here
			if req.ID > 0 && p.engine.hub != nil {
				p.engine.hub.BroadcastCapture(req)
			}
		}
	}
}

// Written 已写入计数
func (p *AsyncWriterPool) Written() uint64 {
	return p.written.Load()
}
