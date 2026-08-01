package llm

// provider.go — LLM provider detection and metadata

import "strings"

// Provider identifies a known LLM API provider.
type Provider string

const (
	ProviderOpenAI    Provider = "openai"
	ProviderAnthropic Provider = "anthropic"
	ProviderGemini    Provider = "gemini"
	ProviderUnknown   Provider = "unknown"
)

// providerPattern defines host/path signatures for each provider.
type providerPattern struct {
	hostSubstrs []string
	pathSubstrs []string
}

var providerPatterns = map[Provider]providerPattern{
	ProviderOpenAI: {
		hostSubstrs: []string{"openai.com", "api.openai.com"},
		pathSubstrs: []string{"/v1/chat/completions", "/v1/completions", "/chat/completions"},
	},
	ProviderAnthropic: {
		hostSubstrs: []string{"anthropic.com"},
		pathSubstrs: []string{"/v1/messages", "/messages"},
	},
	ProviderGemini: {
		hostSubstrs: []string{"generativelanguage.googleapis.com", "aiplatform.googleapis.com"},
		pathSubstrs: []string{"generatecontent", "streamgeneratecontent"},
	},
}

// DetectProvider attempts to identify the LLM provider from host and path.
// 先匹配内置 3 大厂模式（仅按 host 子串匹配，path 单独匹配容易误判）；
// 未命中则查自定义端点注册表，命中视为 openai 兼容。
// DetectProvider identifies the LLM provider from host and path.
// Only host substrings are matched for built-in providers; path-only matching
// was removed because it caused false positives (any host whose path contains
// "/messages" was treated as Anthropic).
func DetectProvider(host, path string) Provider {
	host = strings.ToLower(host)
	path = strings.ToLower(path)
	for provider, pat := range providerPatterns {
		for _, hs := range pat.hostSubstrs {
			// 复用 custom_endpoints.go 的 hostMatches：精确匹配或 "."+pattern 后缀匹配。
			// 避免子串匹配被 "openai.com.evil.com" 这类伪造 host 绕过。
			// Use hostMatches (exact or "."+pattern suffix) instead of strings.Contains
			// so forged hosts like "openai.com.evil.com" can't impersonate providers.
			if hostMatches(host, hs) {
				return provider
			}
		}
	}
	// 自定义端点匹配（OpenAI 兼容）
	// Custom endpoint match (OpenAI-compatible)
	if _, ok := matchCustomEndpoint(host, path); ok {
		return ProviderOpenAI
	}
	return ProviderUnknown
}

// IsLLMRequest determines whether a request is likely an LLM API call
// by matching the host and path against known LLM provider patterns.
func IsLLMRequest(host, path string) bool {
	return DetectProvider(host, path) != ProviderUnknown
}

// IsValidProvider returns true if the provider is a known LLM provider
// (not unknown and not empty).
func IsValidProvider(p Provider) bool {
	return p != ProviderUnknown && p != ""
}
