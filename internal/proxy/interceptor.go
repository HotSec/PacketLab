package proxy

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"packetlab/internal/models"

	"github.com/elazarl/goproxy"
)

// Interceptor 拦截控制器
type Interceptor struct {
	mu       sync.RWMutex
	mode     string            // "auto" | "manual"
	rules    []models.InterceptRule
	pending  map[string]*pendingReq
	onNotify func(req *models.PendingRequest)
	timeout  time.Duration
}

type pendingReq struct {
	req       *http.Request
	id        string
	result    chan models.InterceptResult
	timer     *time.Timer
	createdAt time.Time
}

// NewInterceptor 创建拦截控制器
func NewInterceptor(timeoutSec int, onNotify func(req *models.PendingRequest)) *Interceptor {
	if timeoutSec <= 0 {
		timeoutSec = 15
	}
	return &Interceptor{
		mode:     "auto",
		pending:  make(map[string]*pendingReq),
		onNotify: onNotify,
		timeout:  time.Duration(timeoutSec) * time.Second,
	}
}

// SetMode 设置拦截模式
func (it *Interceptor) SetMode(mode string) {
	it.mu.Lock()
	defer it.mu.Unlock()
	it.mode = mode
}

// GetMode 获取当前模式
func (it *Interceptor) GetMode() string {
	it.mu.RLock()
	defer it.mu.RUnlock()
	return it.mode
}

// SetRules 设置规则列表
func (it *Interceptor) SetRules(rules []models.InterceptRule) {
	it.mu.Lock()
	defer it.mu.Unlock()
	it.rules = rules
}

// GetPending 获取待审列表
func (it *Interceptor) GetPending() []models.PendingRequest {
	it.mu.RLock()
	defer it.mu.RUnlock()
	var list []models.PendingRequest
	for _, p := range it.pending {
		list = append(list, models.PendingRequest{
			ID:        p.id,
			Method:    p.req.Method,
			URL:       p.req.URL.String(),
			Host:      p.req.URL.Host,
			Path:      p.req.URL.Path,
			Headers:   flattenHeaders(p.req.Header),
			Body:      readBody(p.req),
			Timestamp: p.createdAt,
			Age:       time.Since(p.createdAt).Seconds(),
		})
	}
	return list
}

// Resolve 处理用户操作
func (it *Interceptor) Resolve(id string, result models.InterceptResult) error {
	it.mu.Lock()
	p, ok := it.pending[id]
	if !ok {
		it.mu.Unlock()
		return fmt.Errorf("request not found: %s", id)
	}
	delete(it.pending, id)
	it.mu.Unlock()

	p.timer.Stop()
	result.RequestID = id
	p.result <- result
	return nil
}

// Handle 处理请求（在 goproxy OnRequest 中调用）
// 返回 nil 表示拦截已处理（请求被转发或丢弃），非 nil 表示直接放行
func (it *Interceptor) Handle(req *http.Request, ctx *goproxy.ProxyCtx, storeFunc func(*http.Request)) (*http.Request, *http.Response) {
	it.mu.RLock()
	mode := it.mode
	rules := it.rules
	it.mu.RUnlock()

	if mode == "auto" {
		// 规则引擎
		for _, rule := range rules {
			if !rule.Enabled {
				continue
			}
			if matchRule(rule.Pattern, req.URL.Host, req.URL.Path) {
				if rule.Action == "block" {
					return nil, goproxy.NewResponse(req, "text/plain", 403, "Blocked by PacketLab rule: "+rule.Pattern)
				}
				// allow → 放行
				break
			}
		}
		return req, nil // 放行
	}

	// manual 模式：推入待审队列
	id := fmt.Sprintf("req_%d", time.Now().UnixNano())
	ch := make(chan models.InterceptResult, 1)
	timer := time.NewTimer(it.timeout)

	it.mu.Lock()
	pr := &pendingReq{
		req:       req,
		id:        id,
		result:    ch,
		createdAt: time.Now(),
		timer:     timer,
	}
	it.pending[id] = pr
	it.mu.Unlock()

	// 通知前端（锁外操作，避免阻塞）
	if it.onNotify != nil {
		it.onNotify(&models.PendingRequest{
			ID:        id,
			Method:    req.Method,
			URL:       req.URL.String(),
			Host:      req.URL.Host,
			Path:      req.URL.Path,
			Headers:   flattenHeaders(req.Header),
			Body:      readBody(req),
			Timestamp: pr.createdAt,
		})
	}

	// 阻塞等待用户决定或超时 — pr 在锁内赋值，后续只读取 pr 自身字段（结果 channel 和 timer 安全）
	select {
	case r := <-ch:
		timer.Stop()
		switch r.Action {
		case "allow", "modify":
			// 有修改内容时构建新请求，否则原样转发
			if r.Method != "" || r.URL != "" {
				newReq, err := http.NewRequest(r.Method, r.URL, strings.NewReader(r.NewBody))
				if err != nil {
					return nil, goproxy.NewResponse(req, "text/plain", 400, "Invalid modified request")
				}
				for k, v := range r.NewHeaders {
					newReq.Header.Set(k, v)
				}
				if newReq.Header.Get("User-Agent") == "" {
					newReq.Header.Set("User-Agent", "PacketLab/1.0")
				}
				storeFunc(newReq)
				return newReq, nil
			}
			// 无修改，原样转发
			storeFunc(req)
			return req, nil
		case "drop":
			return nil, goproxy.NewResponse(req, "text/plain", 403, "Blocked by PacketLab")
		default:
			return req, nil
		}
	case <-timer.C:
		// 超时自动放过 — 清理并放行
		it.mu.Lock()
		if _, ok := it.pending[id]; ok {
			delete(it.pending, id)
		}
		it.mu.Unlock()
		storeFunc(req)
		return req, nil
	}
}

// matchRule 简单通配匹配
func matchRule(pattern, host, path string) bool {
	// 精确 host 匹配
	if pattern == host {
		return true
	}
	// 后缀匹配 *.example.com
	if strings.HasPrefix(pattern, "*.") && strings.HasSuffix(host, pattern[1:]) {
		return true
	}
	// host/path 前缀匹配
	if idx := strings.Index(pattern, "/"); idx > 0 {
		hp := pattern[:idx]
		pp := pattern[idx:]
		if (hp == host || (strings.HasPrefix(hp, "*.") && strings.HasSuffix(host, hp[1:]))) &&
			strings.HasPrefix(path, pp) {
			return true
		}
	}
	return false
}

func readBody(req *http.Request) string {
	if req.Body == nil {
		return ""
	}
	if req.GetBody != nil {
		if body, err := req.GetBody(); err == nil {
			defer body.Close()
			buf := make([]byte, 8192)
			n, _ := body.Read(buf)
			return string(buf[:n])
		}
	}
	return ""
}
