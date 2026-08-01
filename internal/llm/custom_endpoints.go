package llm

import (
	"net"
	"strings"
	"sync"
)

// custom_endpoints.go — 用户配置的自定义 OpenAI 兼容端点注册表。
// 支持 DeepSeek、Moonshot、本地 vLLM 等 OpenAI 兼容服务的识别。

// CustomEndpoint 定义一个用户配置的 OpenAI 兼容端点。
// 匹配规则：host 完全相等或后缀匹配（如 "api.deepseek.com" 匹配 "*.api.deepseek.com"）。
// path 前缀匹配（如 "/v1/chat/completions" 匹配任何以该前缀开头的路径）。
// CustomEndpoint defines a user-configured OpenAI-compatible endpoint.
type CustomEndpoint struct {
	Host string // 完全匹配或后缀匹配（".api.deepseek.com" 匹配 sub.api.deepseek.com）
	Path string // 前缀匹配（如 "/v1/chat/completions"）；空表示匹配所有路径
	Name string // 显示名（如 "DeepSeek"），可选
}

// customEndpointRegistry 自定义端点注册表，支持运行时动态添加。
// 端到端线程安全，使用 RWMutex 保护。
type customEndpointRegistry struct {
	mu        sync.RWMutex
	endpoints []CustomEndpoint
}

var customEndpoints = &customEndpointRegistry{}

// RegisterCustomEndpoint 注册一个自定义 OpenAI 兼容端点。
// RegisterCustomEndpoint registers a custom OpenAI-compatible endpoint.
func RegisterCustomEndpoint(ep CustomEndpoint) {
	if ep.Host == "" {
		return
	}
	customEndpoints.mu.Lock()
	defer customEndpoints.mu.Unlock()
	// 去重：host+path 相同则覆盖。host 比较大小写不敏感（与 matchCustomEndpoint 的
	// toLowerASCII 匹配保持一致），避免重复注册仅大小写不同的端点。
	// Dedup host case-insensitively to match the case-insensitive matching logic.
	for i, existing := range customEndpoints.endpoints {
		if strings.EqualFold(existing.Host, ep.Host) && existing.Path == ep.Path {
			customEndpoints.endpoints[i] = ep
			return
		}
	}
	customEndpoints.endpoints = append(customEndpoints.endpoints, ep)
}

// UnregisterCustomEndpoint 移除自定义端点。
func UnregisterCustomEndpoint(host, path string) {
	customEndpoints.mu.Lock()
	defer customEndpoints.mu.Unlock()
	for i, existing := range customEndpoints.endpoints {
		if existing.Host == host && existing.Path == path {
			customEndpoints.endpoints = append(customEndpoints.endpoints[:i], customEndpoints.endpoints[i+1:]...)
			return
		}
	}
}

// ListCustomEndpoints 返回已注册的自定义端点副本。
func ListCustomEndpoints() []CustomEndpoint {
	customEndpoints.mu.RLock()
	defer customEndpoints.mu.RUnlock()
	out := make([]CustomEndpoint, len(customEndpoints.endpoints))
	copy(out, customEndpoints.endpoints)
	return out
}

// matchCustomEndpoint 检查 host/path 是否匹配任一自定义端点。
// 返回匹配的 CustomEndpoint（含 Name 用于显示），未匹配返回 false。
// host/path 大小写不敏感（统一转小写比较）。
func matchCustomEndpoint(host, path string) (CustomEndpoint, bool) {
	host = toLowerASCII(host)
	path = toLowerASCII(path)
	customEndpoints.mu.RLock()
	defer customEndpoints.mu.RUnlock()
	for _, ep := range customEndpoints.endpoints {
		epHost := toLowerASCII(ep.Host)
		if !hostMatches(host, epHost) {
			continue
		}
		if ep.Path == "" {
			return ep, true
		}
		epPath := toLowerASCII(ep.Path)
		if hasPrefix(path, epPath) {
			return ep, true
		}
	}
	return CustomEndpoint{}, false
}

// hostMatches 判断 host 是否匹配 pattern。
// 支持：完全匹配、后缀匹配（"api.deepseek.com" 匹配 "sub.api.deepseek.com"）。
// host 含端口时（如 "localhost:8000"）自动剥离端口后再比较。
func hostMatches(host, pattern string) bool {
	host = toLowerASCII(host)
	pattern = toLowerASCII(pattern)
	// 剥离端口号，使 "localhost:8000" 能匹配 pattern "localhost"。
	// Strip port so "localhost:8000" matches pattern "localhost".
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == pattern {
		return true
	}
	// 否则按 "." 分隔后缀匹配（"api.deepseek.com" 匹配 "sub.api.deepseek.com"）
	return hasSuffix(host, "."+pattern)
}

// toLowerASCII 仅 ASCII 字符转小写，避免 unicode 转换开销。
func toLowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

// resetCustomEndpointsForTest 仅用于测试：清空注册表。
func resetCustomEndpointsForTest() {
	customEndpoints.mu.Lock()
	defer customEndpoints.mu.Unlock()
	customEndpoints.endpoints = nil
}
