package api

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"time"

	"packetlab/internal/models"
	"packetlab/internal/store"
)

type ResendService struct {
	store    *store.Store
	hub      *wsHub
	client   *http.Client
}

func NewResendService(st *store.Store, hub *wsHub, insecure bool) *ResendService {
	transport := &http.Transport{
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: insecure},
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}
	return &ResendService{
		store: st,
		hub:   hub,
		client: &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
		},
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

func (s *ResendService) Resend(req *models.ResendRequest) (*ResendResult, error) {
	parsedURL, err := url.Parse(req.URL)
	if err != nil {
		return nil, ErrValidation(fmt.Sprintf("invalid URL: %s", err.Error()))
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return nil, ErrValidation(fmt.Sprintf("unsupported scheme: %s", parsedURL.Scheme))
	}

	isHTTPS := parsedURL.Scheme == "https"

	var bodyReader io.Reader
	if req.Body != "" {
		bodyReader = bytes.NewBufferString(req.Body)
	}

	httpReq, err := http.NewRequest(req.Method, req.URL, bodyReader)
	if err != nil {
		return nil, ErrValidation(fmt.Sprintf("create request: %s", err.Error()))
	}

	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	if httpReq.Header.Get("User-Agent") == "" {
		httpReq.Header.Set("User-Agent", "PacketLab/2.0")
	}

	var resp *http.Response
	var lastErr error
	startTime := time.Now()
	for attempt := 1; attempt <= 3; attempt++ {
		resp, lastErr = s.client.Do(httpReq)
		if lastErr == nil {
			break
		}
		slog.Warn("resend request failed, retrying", "attempt", attempt, "error", lastErr, "url", req.URL)
		if attempt < 3 {
			time.Sleep(time.Duration(attempt*100) * time.Millisecond)
		}
	}
	if lastErr != nil {
		return nil, ErrBadGateway(fmt.Sprintf("send request after 3 retries: %s", lastErr.Error()))
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 && resp.StatusCode < 600 {
		time.Sleep(200 * time.Millisecond)
		resp2, err := s.client.Do(httpReq)
		if err == nil {
			resp.Body.Close()
			resp = resp2
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
		Method:     req.Method,
		URL:        req.URL,
		Host:       parsedURL.Host,
		Path:       parsedURL.Path,
		Protocol:   resp.Proto,
		IsHTTPS:    isHTTPS,
		ReqHeaders: req.Headers,
		ReqBody:    req.Body,
		StatusCode: resp.StatusCode,
		ResHeaders: result.ResHeaders,
		ResBody:    result.ResBody,
		DurationMs: result.DurationMs,
		SizeBytes:  result.SizeBytes,
		CapturedAt: time.Now(),
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

func (s *HARService) Export(limit int) (map[string]interface{}, error) {
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}

	items, err := s.store.ListFull("", "", "", false, limit, 0)
	if err != nil {
		return nil, ErrInternal("failed to query requests for HAR export")
	}

	entries := make([]map[string]interface{}, 0, len(items))
	for _, req := range items {
		entry := map[string]interface{}{
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
			"cache":            map[string]interface{}{},
			"timings":          map[string]interface{}{"send": 0, "wait": req.DurationMs, "receive": 0},
			"serverIPAddress":  req.Host,
		}
		entries = append(entries, entry)
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
