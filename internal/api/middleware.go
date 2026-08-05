package api

import (
	"bufio"
	"context"
	"crypto/subtle"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type contextKey string

const requestIDKey contextKey = "request_id"

func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = uuid.New().String()[:12]
		}
		w.Header().Set("X-Request-ID", id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// authMiddleware 可选 Bearer token 鉴权。
// apiToken 为空时不启用鉴权（兼容默认配置）。
// /health、/ready 始终免鉴权；token 可经 Authorization: Bearer <token> 头
// 或 ?token=<token> 查询参数传递（后者用于浏览器 WebSocket 握手）。
func authMiddleware(apiToken string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if apiToken == "" {
				next.ServeHTTP(w, r)
				return
			}
			if r.URL.Path == "/health" || r.URL.Path == "/ready" {
				next.ServeHTTP(w, r)
				return
			}
			token := r.Header.Get("Authorization")
			if strings.HasPrefix(token, "Bearer ") {
				token = strings.TrimPrefix(token, "Bearer ")
			}
			if token == "" {
				token = r.URL.Query().Get("token")
			}
			if subtle.ConstantTimeCompare([]byte(token), []byte(apiToken)) != 1 {
				writeAppError(w, ErrUnauthorized())
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// csrfMiddleware 对状态变更请求（非 GET/HEAD/OPTIONS）要求自定义头
// X-Requested-With: XMLHttpRequest。浏览器跨站 fetch/form 无法自动携带
// 自定义头（需预检且预检会被 CORS 拒绝），因此可有效阻止恶意网页跨站
// 调用 /api/resend、/api/intercept/*、/api/clear 等端点（CWE-352）。
func csrfMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		// WebSocket 升级请求本身是 GET，不在此列；其余状态变更请求必须带自定义头
		if r.Header.Get("X-Requested-With") != "XMLHttpRequest" {
			writeAppError(w, ErrForbidden())
			return
		}
		next.ServeHTTP(w, r)
	})
}

func corsMiddleware(allowOrigins []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if isAllowedOrigin(origin, allowOrigins) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
				// 仅当 Origin 被允许时才声明 credentials，避免浏览器对未授权
				// 响应中出现 Access-Control-Allow-Credentials 而拒绝
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			// Authorization：--api-token 鉴权时前端所有请求携带 Bearer token，必须显式放行
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Request-ID, Authorization")
			w.Header().Set("Access-Control-Max-Age", "86400")

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// isAllowedOrigin 检查 Origin 是否允许
// 允许规则（按优先级）：
//  1. 显式白名单（allowList）
//  2. localhost / 127.0.0.1 / ::1 任意端口（默认）
//  3. 空 Origin → 拒绝（防止 curl/脚本订阅）
func isAllowedOrigin(origin string, allowList []string) bool {
	if origin == "" {
		return false
	}
	// 显式白名单
	for _, a := range allowList {
		if a == origin {
			return true
		}
	}
	// localhost 默认
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-XSS-Protection", "1; mode=block")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

type rateLimiter struct {
	mu       sync.Mutex
	visitors map[string]*visitorInfo
	rate     int
	window   time.Duration
	stopCh   chan struct{}
	stopOnce sync.Once
}

type visitorInfo struct {
	count   int
	resetAt time.Time
}

func newRateLimiter(rate int, window time.Duration) *rateLimiter {
	rl := &rateLimiter{
		visitors: make(map[string]*visitorInfo),
		rate:     rate,
		window:   window,
		stopCh:   make(chan struct{}),
	}
	go rl.cleanup()
	return rl
}

func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	v, ok := rl.visitors[key]
	if !ok || now.After(v.resetAt) {
		rl.visitors[key] = &visitorInfo{count: 1, resetAt: now.Add(rl.window)}
		return true
	}
	v.count++
	return v.count <= rl.rate
}

func (rl *rateLimiter) cleanup() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			rl.mu.Lock()
			now := time.Now()
			for k, v := range rl.visitors {
				if now.After(v.resetAt) {
					delete(rl.visitors, k)
				}
			}
			rl.mu.Unlock()
		case <-rl.stopCh:
			return
		}
	}
}

func (rl *rateLimiter) stop() {
	rl.stopOnce.Do(func() { close(rl.stopCh) })
}

func rateLimitMiddleware(limiter *rateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}
			if r.URL.Path == "/ws" {
				next.ServeHTTP(w, r)
				return
			}
			// 用 host（不含端口）作为限流 key，避免同一客户端不同端口被算作不同访客
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil || host == "" {
				host = r.RemoteAddr
			}
			if !limiter.allow(host) {
				writeAppError(w, ErrRateLimited())
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				reqID := RequestIDFromContext(r.Context())
				slog.Error("panic recovered",
					"error", err,
					"path", r.URL.Path,
					"method", r.Method,
					"request_id", reqID,
				)
				appErr := ErrInternal("Internal server error")
				appErr.RequestID = reqID
				writeAppError(w, appErr)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type requestIDWriter struct {
	http.ResponseWriter
	requestID string
}

func (w *requestIDWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *requestIDWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

func requestIDInjectorMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		riw := &requestIDWriter{ResponseWriter: w, requestID: RequestIDFromContext(r.Context())}
		next.ServeHTTP(riw, r)
	})
}

func writeAppError(w http.ResponseWriter, err *AppError) {
	if riw, ok := w.(*requestIDWriter); ok && riw.requestID != "" && err.RequestID == "" {
		cp := *err
		cp.RequestID = riw.requestID
		err = &cp
	}
	writeJSON(w, err.StatusCode, err)
}
