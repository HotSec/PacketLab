package proxy

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"packetlab/internal/api"
	"packetlab/internal/models"
	"packetlab/internal/store"

	"github.com/elazarl/goproxy"
)

// Interceptor 拦截控制器
type Interceptor struct {
	mu       sync.RWMutex
	mode     string // "auto" | "manual"
	rules    []models.InterceptRule
	pending  map[string]*pendingReq
	onNotify func(req *models.PendingRequest)
	timeout  time.Duration
	logCh    chan *models.InterceptLog
	wg       sync.WaitGroup
	stopOnce sync.Once
	closeMu  sync.RWMutex
	closed   bool
}

type pendingReq struct {
	req       *http.Request
	resp      *http.Response // kind=response 时的待审响应
	id        string
	result    chan models.InterceptResult
	timer     *time.Timer
	createdAt time.Time
	preview   string // 进入待审队列时快照的 body 预览（GetPending/通知共用，避免并发读流）
	kind      string // "request"（默认）| "response"
	// 快照字段：Resolve/GetPending 在条目出队后仍要读 URL/method/host，
	// kind=response 时 req 为 nil，不能从 req 上取
	method string
	url    string
	host   string
	path   string
}

// maxPending 待审队列上限：超过后新请求直接拒绝（防止无人审批时无限堆积）。
const maxPending = 512

// NewInterceptor 创建拦截控制器。
// timeout <= 0 时使用默认值 15s。建议通过 --intercept-pending-timeout CLI 配置。
// NewInterceptor creates the interceptor. timeout <= 0 falls back to 15s default.
// Configure via --intercept-pending-timeout CLI flag.
func NewInterceptor(timeout time.Duration, onNotify func(req *models.PendingRequest), st *store.Store) *Interceptor {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	it := &Interceptor{
		mode:     "auto",
		pending:  make(map[string]*pendingReq),
		onNotify: onNotify,
		timeout:  timeout,
		logCh:    make(chan *models.InterceptLog, 1024),
	}
	if st != nil {
		it.startLogWriter(st)
	}
	return it
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

func (it *Interceptor) startLogWriter(st *store.Store) {
	it.wg.Add(1)
	go func() {
		defer it.wg.Done()
		for log := range it.logCh {
			if err := st.SaveInterceptLog(log); err != nil {
				slog.Warn("failed to save intercept log", "error", err)
			}
		}
	}()
}

// Stop 关闭拦截器：刷盘所有未写日志，等待 goroutine 退出。
// 幂等，可重复调用。
func (it *Interceptor) Stop() {
	it.stopOnce.Do(func() {
		it.closeMu.Lock()
		it.closed = true
		close(it.logCh)
		it.closeMu.Unlock()
		it.wg.Wait()
	})
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
		pr := models.PendingRequest{
			ID:        p.id,
			Method:    p.method,
			URL:       p.url,
			Host:      p.host,
			Path:      p.path,
			Body:      p.preview,
			Timestamp: p.createdAt,
			Age:       time.Since(p.createdAt).Seconds(),
			Kind:      p.kind,
		}
		if p.kind == "response" {
			pr.StatusCode = p.resp.StatusCode
			pr.Headers = api.FlattenHeaders(p.resp.Header)
		} else {
			pr.Kind = "request"
			pr.Headers = api.FlattenHeaders(p.req.Header)
		}
		list = append(list, pr)
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

	// 记录拦截日志（非阻塞）
	it.writeLog(&models.InterceptLog{
		Action:        result.Action,
		RequestURL:    p.url,
		RequestMethod: p.method,
		RequestHost:   p.host,
		RulePattern:   "",
		Mode:          "manual",
	})

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
			if matchRule(rule, req.Method, req.URL.Host, req.URL.Path) {
				if rule.Action == "block" {
					// 记录拦截日志（非阻塞）
					it.writeLog(&models.InterceptLog{
						Action:        "drop",
						RequestURL:    req.URL.String(),
						RequestMethod: req.Method,
						RequestHost:   req.URL.Host,
						RulePattern:   rule.Pattern,
						Mode:          "auto",
					})
					return nil, goproxy.NewResponse(req, "text/plain", 403, "Blocked by PacketLab rule: "+rule.Pattern)
				}
				// allow → 放行
				it.writeLog(&models.InterceptLog{
					Action:        "allow",
					RequestURL:    req.URL.String(),
					RequestMethod: req.Method,
					RequestHost:   req.URL.Host,
					RulePattern:   rule.Pattern,
					Mode:          "auto",
				})
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
	if len(it.pending) >= maxPending {
		it.mu.Unlock()
		timer.Stop()
		it.writeLog(&models.InterceptLog{
			Action:        "drop",
			RequestURL:    req.URL.String(),
			RequestMethod: req.Method,
			RequestHost:   req.URL.Host,
			RulePattern:   "",
			Mode:          "manual",
		})
		return nil, goproxy.NewResponse(req, "text/plain", 429,
			"PacketLab: pending queue full, request dropped")
	}
	pr := &pendingReq{
		req:       req,
		id:        id,
		result:    ch,
		createdAt: time.Now(),
		timer:     timer,
		preview:   readBody(req),
		kind:      "request",
		method:    req.Method,
		url:       req.URL.String(),
		host:      req.URL.Host,
		path:      req.URL.Path,
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
			Headers:   api.FlattenHeaders(req.Header),
			Body:      pr.preview,
			Timestamp: pr.createdAt,
			Kind:      "request",
		})
	}

	// 阻塞等待用户决定或超时 — pr 在锁内赋值，后续只读取 pr 自身字段（结果 channel 和 timer 安全）
	// handleResult 处理用户决定（allow/modify/drop），返回 goproxy 期望的 (req, resp)。
	// 提取为闭包以便 timer.C 分支复用：当 timer.C 与 ch 同时就绪时，select 可能选中
	// timer.C 分支，此时若不消费 ch 会丢失用户操作。
	handleResult := func(r models.InterceptResult) (*http.Request, *http.Response) {
		timer.Stop()
		switch r.Action {
		case "allow", "modify":
			modified := r.Method != "" || r.URL != "" || r.NewBody != "" || len(r.NewHeaders) > 0
			if modified {
				// 构建新请求：未提供的字段沿用原始请求（修复 http.NewRequest 空头导致
				// 丢失全部原始请求头的问题）；body 仅在提供了新内容时替换，
				// 否则沿用原始 body（仅改 header/method/URL 时不能丢请求体）。
				method := req.Method
				target := req.URL.String()
				if r.Method != "" {
					method = r.Method
				}
				if r.URL != "" {
					target = r.URL
				}
				bodyReader := io.Reader(req.Body)
				if r.NewBody != "" {
					bodyReader = strings.NewReader(r.NewBody)
				}
				newReq, err := http.NewRequest(method, target, bodyReader)
				if err != nil {
					return nil, goproxy.NewResponse(req, "text/plain", 400, "Invalid modified request")
				}
				// 保留原始请求的全部 header，再覆盖用户修改项
				newReq.Header = req.Header.Clone()
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
	}

	select {
	case r := <-ch:
		return handleResult(r)
	case <-timer.C:
		// 优先非阻塞检查 ch：用户 Resolve 与 timer.C 同时就绪时，timer.Stop() 返回 false
		// 但 timer.C 可能被 select 选中，导致用户操作被丢弃。先消费 ch 避免丢操作。
		// Prefer ch over timer.C: when both are ready, select may pick timer.C,
		// discarding the user's Resolve. Non-blocking check of ch first prevents
		// the loss. Resolve already deleted pending and wrote the log, so we
		// just reuse handleResult to process the action.
		select {
		case r := <-ch:
			return handleResult(r)
		default:
		}
		// ch 无就绪 → 超时自动放过 — 清理并放行
		it.mu.Lock()
		if _, ok := it.pending[id]; ok {
			delete(it.pending, id)
		}
		it.mu.Unlock()
		it.writeLog(&models.InterceptLog{
			Action:        "allow",
			RequestURL:    req.URL.String(),
			RequestMethod: req.Method,
			RequestHost:   req.URL.Host,
			RulePattern:   "",
			Mode:          "manual",
		})
		storeFunc(req)
		return req, nil
	}
}

// HandleResponse 处理响应（manual 模式阻塞等待用户决定；auto 模式直放行）。
// 用户可修改响应状态码/头/体（对应 InterceptResult.StatusCode/NewHeaders/NewBody），
// drop 则替换为 403。storeFunc(resp, bodyReplaced) 仅在用户实际修改时调用，
// 将最终响应同步回捕获记录（未修改时不重读 body，避免截断大响应多拉上游数据）。
// SSE 等流式响应不应经过此函数（预览会阻塞流），调用方负责过滤。
func (it *Interceptor) HandleResponse(resp *http.Response, ctx *goproxy.ProxyCtx, storeFunc func(*http.Response, bool)) *http.Response {
	it.mu.RLock()
	mode := it.mode
	it.mu.RUnlock()

	if mode != "manual" || resp == nil || resp.Request == nil {
		return resp
	}
	req := resp.Request
	reqURL := req.URL.String()

	id := fmt.Sprintf("res_%d", time.Now().UnixNano())
	ch := make(chan models.InterceptResult, 1)
	timer := time.NewTimer(it.timeout)

	it.mu.Lock()
	if len(it.pending) >= maxPending {
		it.mu.Unlock()
		timer.Stop()
		it.writeLog(&models.InterceptLog{
			Action:        "drop",
			RequestURL:    reqURL,
			RequestMethod: req.Method,
			RequestHost:   req.URL.Host,
			RulePattern:   "",
			Mode:          "manual",
		})
		return goproxy.NewResponse(req, "text/plain", 429,
			"PacketLab: pending queue full, response dropped")
	}
	pr := &pendingReq{
		resp:      resp,
		id:        id,
		result:    ch,
		createdAt: time.Now(),
		timer:     timer,
		preview:   readRespBody(resp),
		kind:      "response",
		method:    req.Method,
		url:       reqURL,
		host:      req.URL.Host,
		path:      req.URL.Path,
	}
	it.pending[id] = pr
	it.mu.Unlock()

	if it.onNotify != nil {
		it.onNotify(&models.PendingRequest{
			ID:         id,
			Kind:       "response",
			Method:     req.Method,
			URL:        reqURL,
			Host:       req.URL.Host,
			Path:       req.URL.Path,
			StatusCode: resp.StatusCode,
			Headers:    api.FlattenHeaders(resp.Header),
			Body:       pr.preview,
			Timestamp:  pr.createdAt,
		})
	}

	// 处理用户决定
	handleResult := func(r models.InterceptResult) *http.Response {
		timer.Stop()
		switch r.Action {
		case "allow", "modify":
			bodyReplaced := false
			if r.StatusCode > 0 {
				resp.StatusCode = r.StatusCode
				if text := http.StatusText(r.StatusCode); text != "" {
					resp.Status = fmt.Sprintf("%d %s", r.StatusCode, text)
				} else {
					resp.Status = fmt.Sprintf("%d", r.StatusCode)
				}
			}
			for k, v := range r.NewHeaders {
				resp.Header.Set(k, v)
			}
			if r.NewBody != "" {
				body := []byte(r.NewBody)
				resp.Body = io.NopCloser(bytes.NewReader(body))
				resp.ContentLength = int64(len(body))
				resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
				bodyReplaced = true
			}
			// 仅在实际修改时同步捕获记录：未修改的截断大响应若重读 body
			// 会从上游多拉数据（rest 为无界流）
			if r.StatusCode > 0 || len(r.NewHeaders) > 0 || bodyReplaced {
				storeFunc(resp, bodyReplaced)
			}
			return resp
		case "drop":
			return goproxy.NewResponse(req, "text/plain", 403, "Blocked by PacketLab")
		default:
			return resp
		}
	}

	select {
	case r := <-ch:
		return handleResult(r)
	case <-timer.C:
		// 与请求路径同：timer.C 被选中时先非阻塞消费 ch，防丢用户操作
		select {
		case r := <-ch:
			return handleResult(r)
		default:
		}
		it.mu.Lock()
		delete(it.pending, id)
		it.mu.Unlock()
		it.writeLog(&models.InterceptLog{
			Action:        "allow",
			RequestURL:    reqURL,
			RequestMethod: req.Method,
			RequestHost:   req.URL.Host,
			RulePattern:   "",
			Mode:          "manual",
		})
		return resp
	}
}

// writeLog 非阻塞写入拦截日志到 channel。
// 持读锁期间 channel 不会被 close，确保 send 安全。
func (it *Interceptor) writeLog(log *models.InterceptLog) {
	it.closeMu.RLock()
	defer it.closeMu.RUnlock()
	if it.closed {
		return
	}
	select {
	case it.logCh <- log:
	default:
		slog.Warn("intercept log channel full, dropping log",
			"action", log.Action, "url", log.RequestURL)
	}
}

// matchRule 简单通配匹配（host 大小写不敏感）。
// method 非空时额外校验请求方法（大小写不敏感）。
func matchRule(rule models.InterceptRule, method, host, path string) bool {
	// 方法过滤：规则指定了 method 时必须匹配（支持逗号分隔多个方法）
	if rule.Method != "" {
		m := strings.ToUpper(method)
		matched := false
		for _, rm := range strings.Split(rule.Method, ",") {
			if strings.ToUpper(strings.TrimSpace(rm)) == m {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	pattern := strings.ToLower(rule.Pattern)
	host = strings.ToLower(host)
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

// readBody 读取请求体预览（最多 maxBodyPreview 字节），不破坏转发流。
// 优先 GetBody（可重复读）；无 GetBody 时用 ReadFull 预览后把已读部分拼回
// MultiReader 恢复完整流（修复旧实现 64KB 截断导致转发丢数据的问题）。
const maxBodyPreview = 64 * 1024

func readBody(req *http.Request) string {
	if req.Body == nil {
		return ""
	}
	// 优先 GetBody（OnRequest 已设置，可重复读）
	if req.GetBody != nil {
		if body, err := req.GetBody(); err == nil {
			defer body.Close()
			raw, err := io.ReadAll(io.LimitReader(body, maxBodyPreview))
			if err == nil && len(raw) > 0 {
				return string(raw)
			}
		}
	}
	// fallback：预览后恢复完整流（不消费、不截断转发数据）
	raw := make([]byte, maxBodyPreview)
	n, _ := io.ReadFull(req.Body, raw)
	if n == 0 {
		return ""
	}
	preview := raw[:n]
	// 将已读部分拼回流首，恢复完整请求体
	req.Body = io.NopCloser(io.MultiReader(bytes.NewReader(preview), req.Body))
	return string(preview)
}

// readRespBody 读取响应体预览（最多 maxBodyPreview 字节），不破坏转发流。
// 与 readBody 的 fallback 路径同理：响应无 GetBody，预览后拼回 MultiReader。
// 注意：SSE 等流式响应不可经此函数（ReadFull 会阻塞等待填充），调用方负责过滤。
func readRespBody(resp *http.Response) string {
	if resp.Body == nil {
		return ""
	}
	raw := make([]byte, maxBodyPreview)
	n, _ := io.ReadFull(resp.Body, raw)
	if n == 0 {
		return ""
	}
	preview := raw[:n]
	resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(preview), resp.Body))
	return string(preview)
}
