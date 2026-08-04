package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"time"

	"packetlab/internal/models"
	"packetlab/internal/store"
)

type ResendService struct {
	store     *store.Store
	hub       *wsHub
	insecure  bool
	transport *http.Transport
}

func NewResendService(st *store.Store, hub *wsHub, insecure bool) *ResendService {
	transport := &http.Transport{
		TLSClientConfig:        &tls.Config{InsecureSkipVerify: insecure},
		MaxIdleConns:           100,
		MaxIdleConnsPerHost:    10,
		IdleConnTimeout:        90 * time.Second,
		ResponseHeaderTimeout:  30 * time.Second,
		MaxResponseHeaderBytes: 64 * 1024,
	}
	// DialContext 在建立连接时重新解析并校验目标 IP：拨号用的正是校验过的解析
	// 结果，封死 check-then-dial 的 DNS rebinding 窗口（外层 checkResendTarget
	// 仅用于快速失败与清晰报错，不承担安全职责）。
	transport.DialContext = resendDialContext(insecure)
	return &ResendService{
		store:     st,
		hub:       hub,
		insecure:  insecure,
		transport: transport,
	}
}

// resendDialContext 返回重发专用的拨号函数：非 trusted（未开启 --insecure）时
// 在拨号处解析主机名、校验全部解析结果；任一命中禁止地址段则返回 errBlockedTarget
// （复用重试循环的哨兵判断）。校验通过后直接拨已校验的 IP，避免内核二次解析引入
// TOCTOU。已校验 IP 直接命中检查；trusted 时原样透传。
func resendDialContext(trusted bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: 10 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if !trusted {
			ip, err := resolveAllowedIP(ctx, host)
			if err != nil {
				// errBlockedTarget 直接透传（重试循环的哨兵判断）
				return nil, err
			}
			// 拨已校验的 IP（TLS SNI 由 http.Transport 按请求 URL host 设置，
			// 不受拨号地址影响）
			return d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		}
		return d.DialContext(ctx, network, addr)
	}
}

type ResendResult struct {
	ID         int64             `json:"id"`
	StatusCode int               `json:"status_code"`
	ResHeaders map[string]string `json:"res_headers"`
	ResBody    string            `json:"res_body"`
	DurationMs int64             `json:"duration_ms"`
	SizeBytes  int64             `json:"size_bytes"`
}

// errBlockedTarget 目标地址被 SSRF 防护拦截的哨兵错误（跳过无意义重试）。
var errBlockedTarget = errors.New("resend target not allowed")

// Resend 重发请求。SSRF 防护：默认禁止访问回环/链路本地/私网地址
// （含 DNS 解析结果的任一命中，防 DNS rebinding）；仅 --insecure 显式放行。
// 重定向仅允许同 host（防止重定向链跳到内部网络或第三方收集数据）。
func (s *ResendService) Resend(req *models.ResendRequest) (*ResendResult, error) {
	parsedURL, err := url.Parse(req.URL)
	if err != nil {
		return nil, ErrValidation(fmt.Sprintf("invalid URL: %s", err.Error()))
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return nil, ErrValidation(fmt.Sprintf("unsupported scheme: %s", parsedURL.Scheme))
	}

	isHTTPS := parsedURL.Scheme == "https"

	// SSRF 防护：仅 --insecure 时放行私网/回环地址；调用方 IP 不参与信任判定，
	// 否则本机客户端（浏览器/任意本机进程）可无条件探测内网
	trusted := s.insecure
	originalHost := parsedURL.Host
	if err := checkResendTarget(parsedURL, trusted); err != nil {
		return nil, ErrValidation(err.Error())
	}

	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: s.transport,
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			// 最多 10 跳（len(via) 为已跳次数，>10 时超出）
			if len(via) > 10 {
				return errors.New("stopped after 10 redirects")
			}
			// 重定向仅允许同 host：跨 host 的跳转一律拦截（含重新做 SSRF 校验）
			if r.URL.Host != originalHost {
				return errBlockedTarget
			}
			if err := checkResendTarget(r.URL, trusted); err != nil {
				return errBlockedTarget
			}
			return nil
		},
	}

	// bodyBytes: 缓存请求体字节，用于每次重试重建 body (http.Request.Body 只能读一次)
	bodyBytes := []byte(req.Body)

	// newRequest 构造一个全新的 *http.Request（含 fresh body），供每次发送/重试使用
	newRequest := func() (*http.Request, error) {
		var bodyReader io.Reader
		if len(bodyBytes) > 0 {
			bodyReader = bytes.NewReader(bodyBytes)
		}
		r, err := http.NewRequest(req.Method, req.URL, bodyReader)
		if err != nil {
			return nil, err
		}
		for k, v := range req.Headers {
			r.Header.Set(k, v)
		}
		if r.Header.Get("User-Agent") == "" {
			r.Header.Set("User-Agent", "PacketLab/2.0")
		}
		return r, nil
	}

	// 首次构造用于校验 Method/URL 合法性
	if _, err := newRequest(); err != nil {
		return nil, ErrValidation(fmt.Sprintf("create request: %s", err.Error()))
	}

	var resp *http.Response
	var lastErr error
	startTime := time.Now()
	for attempt := 1; attempt <= 3; attempt++ {
		var httpReq *http.Request
		httpReq, lastErr = newRequest()
		if lastErr != nil {
			break
		}
		resp, lastErr = client.Do(httpReq)
		if lastErr == nil {
			break
		}
		slog.Warn("resend request failed, retrying", "attempt", attempt, "error", lastErr, "url", req.URL)
		// 目标被 SSRF 防护拦截时不重试
		if errors.Is(lastErr, errBlockedTarget) {
			break
		}
		if attempt < 3 {
			time.Sleep(time.Duration(attempt*100) * time.Millisecond)
		}
	}
	if lastErr != nil {
		if errors.Is(lastErr, errBlockedTarget) {
			// errBlockedTarget 可能来自拨号层（目标/重定向地址被 SSRF 拦截）
			// 或 CheckRedirect（跨 host 重定向），统一描述
			return nil, ErrValidation("target not allowed (private/loopback address or cross-host redirect)")
		}
		return nil, ErrBadGateway(fmt.Sprintf("send request after 3 retries: %s", lastErr.Error()))
	}
	// 用闭包变量，重试后 resp = resp2，defer 时关最新的 resp.Body
	defer func() {
		if resp != nil {
			resp.Body.Close()
		}
	}()

	if resp.StatusCode >= 500 && resp.StatusCode < 600 {
		time.Sleep(200 * time.Millisecond)
		if retryReq, err := newRequest(); err == nil {
			if resp2, err := client.Do(retryReq); err == nil {
				resp.Body.Close()
				resp = resp2
			}
		}
	}

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRequestBodySize))

	result := &ResendResult{
		StatusCode: resp.StatusCode,
		ResHeaders: FlattenHeaders(resp.Header),
		ResBody:    string(respBody),
		DurationMs: time.Since(startTime).Milliseconds(),
		SizeBytes:  int64(len(respBody)),
	}

	captured := &models.CapturedRequest{
		Method:      req.Method,
		URL:         req.URL,
		Host:        parsedURL.Host,
		Path:        parsedURL.Path,
		Protocol:    resp.Proto,
		IsHTTPS:     isHTTPS,
		ReqHeaders:  req.Headers,
		ReqBody:     req.Body,
		StatusCode:  resp.StatusCode,
		ResHeaders:  result.ResHeaders,
		ResBody:     result.ResBody,
		DurationMs:  result.DurationMs,
		SizeBytes:   result.SizeBytes,
		CapturedAt:  time.Now(),
		CaptureMode: "resend",
	}

	id, err := s.store.Save(captured)
	if err != nil {
		return nil, ErrInternal("failed to save resend result")
	}
	result.ID = id

	if s.hub != nil {
		s.hub.broadcast(captured)
	}

	return result, nil
}

type HARService struct {
	store *store.Store
}

func NewHARService(st *store.Store) *HARService {
	return &HARService{store: st}
}

// Export 导出完整 HAR（内存版，用于测试与兼容；生产 HTTP 路径使用 ExportTo 流式导出）。
func (s *HARService) Export(limit int) (map[string]interface{}, error) {
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	items, err := s.store.ListFull("", "", "", false, limit, 0)
	if err != nil {
		return nil, ErrInternal("failed to query requests for HAR export")
	}
	entries := make([]map[string]interface{}, 0, len(items))
	for i := range items {
		entries = append(entries, buildHAREntry(&items[i]))
	}
	return map[string]interface{}{
		"log": map[string]interface{}{
			"version": "1.2",
			"creator": map[string]string{
				"name":    "PacketLab",
				"version": "2.0",
			},
			"entries": entries,
		},
	}, nil
}

// ExportTo 流式导出 HAR 到 w。
// 逐条查询并写入 entries，避免大结果集（每条记录请求/响应体合计最大约 6MB，
// limit 上限 5000 时最坏近 30GB）一次性载入内存导致 OOM。
func (s *HARService) ExportTo(w io.Writer, limit int) error {
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}

	if _, err := io.WriteString(w, `{"log":{"version":"1.2","creator":{"name":"PacketLab","version":"2.0"},"entries":[`); err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	first := true
	err := s.store.ForEachFull("", "", "", false, limit, 0, func(req *models.CapturedRequest) error {
		if !first {
			if _, err := w.Write([]byte(",")); err != nil {
				return err
			}
		}
		first = false
		return enc.Encode(buildHAREntry(req))
	})
	if err != nil {
		return ErrInternal("failed to query requests for HAR export")
	}
	_, err = io.WriteString(w, `]}}`)
	return err
}

func buildHAREntry(req *models.CapturedRequest) map[string]interface{} {
	return map[string]interface{}{
		"startedDateTime": req.CapturedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		"time":            req.DurationMs,
		"request": map[string]interface{}{
			"method":      req.Method,
			"url":         req.URL,
			"httpVersion": "HTTP/1.1",
			"headers":     toHARHeaders(req.ReqHeaders),
			"headersSize": -1,
			"bodySize":    len(req.ReqBody),
		},
		"response": map[string]interface{}{
			"status":      req.StatusCode,
			"statusText":  http.StatusText(req.StatusCode),
			"httpVersion": "HTTP/1.1",
			"headers":     toHARHeaders(req.ResHeaders),
			"headersSize": -1,
			"bodySize":    len(req.ResBody),
			"content": map[string]interface{}{
				"size": len(req.ResBody),
				"text": req.ResBody,
			},
		},
		"cache":           map[string]interface{}{},
		"timings":         map[string]interface{}{"send": 0, "wait": req.DurationMs, "receive": 0},
		"serverIPAddress": req.Host,
	}
}

func toHARHeaders(headers map[string]string) []map[string]string {
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := make([]map[string]string, 0, len(headers))
	for _, k := range keys {
		h = append(h, map[string]string{"name": k, "value": headers[k]})
	}
	return h
}

// ========================================
// SSRF 防护
// ========================================

// blockedIP 判断 IP 是否属于禁止重发访问的地址段。
// 禁止：回环、链路本地（169.254.0.0/16、fe80::/10）、RFC1918 私网、
// CGNAT（100.64.0.0/10）、组播、未指定地址。
func blockedIP(ip net.IP) bool {
	if ip == nil {
		return true // 无法解析的地址一律拦截
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsPrivate() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1]&0xC0 == 64 {
		return true // 100.64.0.0/10 CGNAT
	}
	return false
}

// resolveAllowedIP 解析主机名并校验全部结果，返回首个放行的 IP。
// IP 字面量直接校验；主机名任一解析结果命中禁止段返回 errBlockedTarget。
// 快速失败校验（checkResendTarget）与拨号层（resendDialContext）共用，
// 保证两层判定语义一致。
func resolveAllowedIP(ctx context.Context, host string) (net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if blockedIP(ip) {
			return nil, errBlockedTarget
		}
		return ip, nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("cannot resolve target host %s", host)
	}
	for _, r := range ips {
		if blockedIP(r.IP) {
			return nil, errBlockedTarget
		}
	}
	return ips[0].IP, nil
}

// checkResendTarget 校验重发目标：trusted=true（--insecure）时放行全部地址。
// 非 trusted 时：解析主机名（防 DNS rebinding，任一解析结果命中禁止段即拦截）。
// 仅用于快速失败与清晰报错——真实安全职责由 resendDialContext 在拨号处承担。
func checkResendTarget(u *url.URL, trusted bool) error {
	if trusted {
		return nil
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("invalid URL: missing host")
	}
	if _, err := resolveAllowedIP(context.Background(), host); err != nil {
		if errors.Is(err, errBlockedTarget) {
			if net.ParseIP(host) != nil {
				return fmt.Errorf("target %s is a private/loopback address, resend blocked", host)
			}
			return fmt.Errorf("target %s resolves to a private/loopback address, resend blocked", host)
		}
		return err
	}
	return nil
}
