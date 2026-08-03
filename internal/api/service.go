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
	"strings"
	"time"

	"packetlab/internal/models"
	"packetlab/internal/store"
)

type ResendService struct {
	store    *store.Store
	hub      *wsHub
	insecure bool
	transport *http.Transport
}

func NewResendService(st *store.Store, hub *wsHub, insecure bool) *ResendService {
	transport := &http.Transport{
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: insecure},
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxResponseHeaderBytes: 64 * 1024,
	}
	return &ResendService{
		store:     st,
		hub:       hub,
		insecure:  insecure,
		transport: transport,
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

// Resend 保持旧签名（默认视为本机客户端调用），供测试与兼容使用。
func (s *ResendService) Resend(req *models.ResendRequest) (*ResendResult, error) {
	return s.ResendFrom("127.0.0.1", req)
}

// errBlockedTarget 目标地址被 SSRF 防护拦截的哨兵错误（跳过无意义重试）。
var errBlockedTarget = errors.New("resend target not allowed")

// ResendFrom 重发请求。clientIP 为调用方地址：来自回环地址的调用视为本机调试，
// 允许访问私网目标；非本机调用禁止访问回环/链路本地/私网地址（SSRF 防护）。
func (s *ResendService) ResendFrom(clientIP string, req *models.ResendRequest) (*ResendResult, error) {
	parsedURL, err := url.Parse(req.URL)
	if err != nil {
		return nil, ErrValidation(fmt.Sprintf("invalid URL: %s", err.Error()))
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return nil, ErrValidation(fmt.Sprintf("unsupported scheme: %s", parsedURL.Scheme))
	}

	isHTTPS := parsedURL.Scheme == "https"

	// SSRF 防护：仅本机客户端或 --insecure 时允许访问私网/回环地址
	trusted := s.insecure || isLoopbackAddr(clientIP)
	if err := checkResendTarget(parsedURL, trusted); err != nil {
		return nil, ErrValidation(err.Error())
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: s.transport,
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
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
			return nil, ErrValidation("redirect target not allowed (private/loopback address)")
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

// isLoopbackAddr 判断地址是否为回环地址（127.0.0.0/8、::1、localhost）。
func isLoopbackAddr(addr string) bool {
	ip := net.ParseIP(strings.TrimSpace(addr))
	if ip != nil {
		return ip.IsLoopback()
	}
	// 主机名形式（如 localhost / 本机 hostname）
	return strings.EqualFold(addr, "localhost") || strings.EqualFold(addr, "::1") || addr == ""
}

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

// checkResendTarget 校验重发目标：trusted=true（本机客户端或 --insecure）时放行全部地址。
// 非 trusted 时：解析主机名（防 DNS rebinding，任一解析结果命中禁止段即拦截）。
func checkResendTarget(u *url.URL, trusted bool) error {
	if trusted {
		return nil
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("invalid URL: missing host")
	}
	if ip := net.ParseIP(host); ip != nil {
		if blockedIP(ip) {
			return fmt.Errorf("target %s is a private/loopback address, resend blocked", host)
		}
		return nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(context.Background(), host)
	if err != nil {
		return fmt.Errorf("cannot resolve target host %s", host)
	}
	if len(ips) == 0 {
		return fmt.Errorf("cannot resolve target host %s", host)
	}
	for _, ip := range ips {
		if blockedIP(ip.IP) {
			return fmt.Errorf("target %s resolves to a private/loopback address (%s), resend blocked", host, ip.IP)
		}
	}
	return nil
}
