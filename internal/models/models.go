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
	CaptureMode string            `json:"capture_mode,omitempty"` // "proxy" | "nic"
	ProcessPID  int               `json:"process_pid,omitempty"`
	ProcessName string            `json:"process_name,omitempty"`
	IsSSE       bool              `json:"is_sse,omitempty"`      // 是否为 SSE 流式响应
	SSEEvents   string            `json:"sse_events,omitempty"`  // SSE 事件累积内容
}

// ProcessInfo 进程信息
type ProcessInfo struct {
	PID     int    `json:"pid"`
	Name    string `json:"name"`
	Cmdline string `json:"cmdline,omitempty"`
}

// RequestListItem 列表项的简略信息
type RequestListItem struct {
	ID          int64     `json:"id"`
	Method      string    `json:"method"`
	URL         string    `json:"url"`
	Host        string    `json:"host"`
	StatusCode  int       `json:"status_code"`
	DurationMs  int64     `json:"duration_ms"`
	SizeBytes   int64     `json:"size_bytes"`
	CapturedAt  time.Time `json:"captured_at"`
	IsHTTPS     bool      `json:"is_https"`
	CaptureMode string    `json:"capture_mode,omitempty"`
	ProcessPID  int       `json:"process_pid,omitempty"`
	ProcessName string    `json:"process_name,omitempty"`
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

// InterceptRule 拦截规则
type InterceptRule struct {
	ID        int64     `json:"id"`
	Pattern   string    `json:"pattern"`
	Method    string    `json:"method,omitempty"` // 可选：限定 HTTP 方法（空=所有方法）
	Action    string    `json:"action"`           // "allow" | "block"
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

// InterceptResult 拦截操作结果
type InterceptResult struct {
	RequestID  string            `json:"request_id"`
	Action     string            `json:"action"` // "allow" | "drop" | "modify"
	Method     string            `json:"method,omitempty"`
	URL        string            `json:"url,omitempty"`
	NewHeaders map[string]string `json:"new_headers,omitempty"`
	NewBody    string            `json:"new_body,omitempty"`
}

// InterceptLog 拦截操作日志
type InterceptLog struct {
	ID            int64  `json:"id"`
	Action        string `json:"action"`         // "allow" | "drop" | "modify"
	RequestURL    string `json:"request_url"`
	RequestMethod string `json:"request_method"`
	RequestHost   string `json:"request_host"`
	RulePattern   string `json:"rule_pattern"` // auto 模式命中规则时的 pattern，manual 模式为空
	Mode          string `json:"mode"`          // "auto" | "manual"
	CreatedAt     string `json:"created_at"`
}

// CleanupRequest POST /api/maintenance/cleanup 请求体
type CleanupRequest struct {
	RetentionDays int `json:"retention_days"` // 可选，默认从 settings 表读取
}

// CleanupResponse 返回体
type CleanupResponse struct {
	DeletedRequests int64 `json:"deleted_requests"`
	DeletedLogs     int64 `json:"deleted_logs"`
	RetentionDays   int   `json:"retention_days"`
}

// PendingRequest 待审批请求
type PendingRequest struct {
	ID        string            `json:"id"`
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Host      string            `json:"host"`
	Path      string            `json:"path"`
	Headers   map[string]string `json:"headers"`
	Body      string            `json:"body"`
	Timestamp time.Time         `json:"timestamp"`
	Age       float64           `json:"age_sec"`
}
