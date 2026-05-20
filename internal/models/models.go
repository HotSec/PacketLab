package models

import "time"

// CapturedRequest 一条被捕获的 HTTP 请求记录
type CapturedRequest struct {
	ID          int64             `json:"id"`
	Method      string            `json:"method"`
	URL         string            `json:"url"`
	Host        string            `json:"host"`
	Path        string            `json:"path"`
	Protocol    string            `json:"protocol"`
	IsHTTPS     bool              `json:"is_https"`
	ReqHeaders  map[string]string `json:"req_headers"`
	ReqBody     string            `json:"req_body"`
	StatusCode  int               `json:"status_code"`
	ResHeaders  map[string]string `json:"res_headers"`
	ResBody     string            `json:"res_body"`
	DurationMs  int64             `json:"duration_ms"`
	SizeBytes   int64             `json:"size_bytes"`
	CapturedAt  time.Time         `json:"captured_at"`
}

// RequestListItem 列表项的简略信息
type RequestListItem struct {
	ID         int64     `json:"id"`
	Method     string    `json:"method"`
	URL        string    `json:"url"`
	Host       string    `json:"host"`
	StatusCode int       `json:"status_code"`
	DurationMs int64     `json:"duration_ms"`
	SizeBytes  int64     `json:"size_bytes"`
	CapturedAt time.Time `json:"captured_at"`
	IsHTTPS    bool      `json:"is_https"`
}

// ResendRequest 重发请求的结构
type ResendRequest struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

// ResendResponse 重发请求的响应
type ResendResponse struct {
	ID          int64             `json:"id"`
	StatusCode  int               `json:"status_code"`
	ResHeaders  map[string]string `json:"res_headers"`
	ResBody     string            `json:"res_body"`
	DurationMs  int64             `json:"duration_ms"`
	SizeBytes   int64             `json:"size_bytes"`
}

// APINote API 接口备注
type APINote struct {
	ID        int64     `json:"id"`
	Host      string    `json:"host"`
	Path      string    `json:"path"`
	Method    string    `json:"method"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// APINoteRequest 创建/更新备注的请求
type APINoteRequest struct {
	Host   string `json:"host"`
	Path   string `json:"path"`
	Method string `json:"method"`
	Note   string `json:"note"`
}
