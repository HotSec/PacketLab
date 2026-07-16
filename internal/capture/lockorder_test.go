package capture

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"packetlab/internal/models"
)

// TestStreamPool_LockOrder 回归测试：HandleClose × FlushOlderThan 并发不得死锁。
//
// Bug 6 背景：
//   - FlushOlderThan 锁顺序：Assembler.mu → TCPStream.mu
//   - HandleClose    锁顺序：TCPStream.mu → Assembler.mu
//   两者相反，跨 worker 并发可能死锁。
//
// 本测试并发调用两者 1000 次，若 5 秒内未完成则判定死锁。
func TestStreamPool_LockOrder(t *testing.T) {
	pool := NewTCPStreamPool(&Engine{procCache: make(map[string]*models.ProcessInfo)})
	a := NewAssembler(pool)

	// 构造多个流并加入 assembler。
	// 使用 nonHTTP=true 跳过 emit 路径，聚焦锁顺序验证。
	const numStreams = 20
	streams := make([]*TCPStream, numStreams)
	keys := make([]string, numStreams)
	for i := range streams {
		streams[i] = pool.New(net.ParseIP("192.168.1.100"), uint16(10000+i), 80)
		streams[i].nonHTTP = true
		keys[i] = fmt.Sprintf("10.0.0.1:80-192.168.1.100:%d", 10000+i)
		a.streams[keys[i]] = streams[i]
	}

	const iterations = 1000
	done := make(chan struct{})

	go func() {
		var wg sync.WaitGroup
		wg.Add(2)

		// worker A: 反复调用 HandleClose 并重新加入 map（模拟连接关闭后被新连接复用）
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				idx := i % numStreams
				s := streams[idx]
				s.HandleClose(a)
				// 重新加入 map 以便下一轮可被 FlushOlderThan 看到
				a.mu.Lock()
				a.streams[keys[idx]] = s
				a.mu.Unlock()
			}
		}()

		// worker B: 反复调用 FlushOlderThan（模拟 GC 清理过期流）
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				// cutoff 设为未来 1 小时，确保所有流都会被遍历并尝试清理
				a.FlushOlderThan(time.Now().Add(1*time.Hour), nil)
			}
		}()

		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// 完成，无死锁
	case <-time.After(5 * time.Second):
		t.Fatal("HandleClose × FlushOlderThan 死锁（5s 超时），锁顺序相反")
	}
}
