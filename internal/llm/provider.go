package llm

// provider.go — LLM provider detection and metadata
//
// 内置检测覆盖国际大厂与国内主流 LLM 厂商，均按 host 精确/后缀匹配
// （hostMatches，防 "openai.com.evil.com" 伪造）。未命中内置表的请求
// 回落到自定义端点注册表（OpenAI 兼容），由用户在配置中登记。

import (
	"sort"
	"strings"
)

// Provider identifies a known LLM API provider.
type Provider string

const (
	ProviderOpenAI    Provider = "openai"
	ProviderAnthropic Provider = "anthropic"
	ProviderGemini    Provider = "gemini"
	// 国内厂商
	ProviderDeepSeek Provider = "deepseek"
	ProviderMoonshot Provider = "moonshot" // Kimi
	ProviderZhipu    Provider = "zhipu"    // 智谱 GLM
	ProviderMiniMax  Provider = "minimax"
	ProviderQwen     Provider = "qwen" // 阿里云 DashScope / 百炼
	ProviderXAI      Provider = "xai"  // Grok
	ProviderUnknown  Provider = "unknown"
)

// ParseProtocol describes how to parse requests/responses for a provider.
type ParseProtocol string

const (
	ParseOpenAI    ParseProtocol = "openai"    // OpenAI Chat Completions 兼容
	ParseAnthropic ParseProtocol = "anthropic" // Anthropic Messages API
	ParseGemini    ParseProtocol = "gemini"    // Gemini generateContent
)

// providerDef defines host signatures and parse protocol for each provider.
type providerDef struct {
	name         Provider      // 规范化名称
	displayName  string        // UI 展示名
	hostSuffixes []string      // host 精确匹配或 "."+suffix 后缀匹配
	protocol     ParseProtocol // 解析协议
}

// builtinProviders 内置厂商表。按 hostSuffixes 从长到短排序，
// 保证 api.deepseek.com 先于 deepseek.com 命中（虽然两者解析协议相同，
// 顺序不影响结果，但保持确定性）。同厂多域名用逗号分隔在注释中说明。
var builtinProviders = []providerDef{
	{ProviderOpenAI, "OpenAI", []string{"api.openai.com", "openai.com"}, ParseOpenAI},
	{ProviderAnthropic, "Anthropic", []string{"api.anthropic.com", "anthropic.com"}, ParseAnthropic},
	{ProviderGemini, "Google Gemini", []string{
		"generativelanguage.googleapis.com",
		"aiplatform.googleapis.com",
	}, ParseGemini},
	// 国内厂商
	{ProviderDeepSeek, "DeepSeek", []string{"api.deepseek.com", "deepseek.com"}, ParseOpenAI},
	{ProviderMoonshot, "Moonshot (Kimi)", []string{"api.moonshot.cn", "api.moonshot.ai", "moonshot.cn"}, ParseOpenAI},
	{ProviderZhipu, "智谱 GLM", []string{"open.bigmodel.cn", "bigmodel.cn"}, ParseOpenAI},
	{ProviderMiniMax, "MiniMax", []string{"api.minimax.chat", "api.minimaxi.com", "minimaxi.com"}, ParseOpenAI},
	{ProviderQwen, "阿里云 Qwen", []string{"dashscope.aliyuncs.com"}, ParseOpenAI},
	{ProviderXAI, "xAI (Grok)", []string{"api.x.ai", "x.ai"}, ParseOpenAI},
}

func init() {
	// hostSuffixes 按长度降序排序：更长的后缀（更具体的域名）优先匹配。
	for i := range builtinProviders {
		sort.Slice(builtinProviders[i].hostSuffixes, func(a, b int) bool {
			return len(builtinProviders[i].hostSuffixes[a]) > len(builtinProviders[i].hostSuffixes[b])
		})
	}
	// 厂商表本身也按「最长 suffix 长度」降序排序，保证 generativelanguage.google… 
	// 这类长域名在任何短域名误匹配之前被检查。
	sort.SliceStable(builtinProviders, func(a, b int) bool {
		return len(builtinProviders[a].hostSuffixes[0]) > len(builtinProviders[b].hostSuffixes[0])
	})
}

// DetectProvider identifies the LLM provider from host and path.
// 先匹配内置厂商（仅按 host 后缀匹配，path 单独匹配容易误判）；
// 未命中则查自定义端点注册表，命中视为 OpenAI 兼容。
func DetectProvider(host, path string) Provider {
	host = strings.ToLower(strings.TrimSpace(host))
	path = strings.ToLower(path)
	for i := range builtinProviders {
		def := &builtinProviders[i]
		for _, hs := range def.hostSuffixes {
			// hostMatches：精确匹配或 "."+pattern 后缀匹配。
			// 避免子串匹配被 "openai.com.evil.com" 这类伪造 host 绕过。
			if hostMatches(host, hs) {
				return def.name
			}
		}
	}
	// 区域化 Vertex AI host：<region>-aiplatform.googleapis.com（如
	// us-central1-aiplatform.googleapis.com）。hostMatches 的 "."+pattern
	// 后缀规则不覆盖 "-" 连接的区域前缀，这里单独按后缀处理——host 必须
	// 以该后缀结尾，且处于 googleapis.com 域名空间内，无伪造风险。
	if strings.HasSuffix(host, "-aiplatform.googleapis.com") {
		return ProviderGemini
	}
	// 自定义端点匹配（OpenAI 兼容）
	if _, ok := matchCustomEndpoint(host, path); ok {
		return ProviderOpenAI
	}
	return ProviderUnknown
}

// ProtocolFor returns the parse protocol for a provider.
// 自定义端点已在 DetectProvider 中归一化为 ProviderOpenAI，无需特殊处理。
func ProtocolFor(p Provider) ParseProtocol {
	for i := range builtinProviders {
		if builtinProviders[i].name == p {
			return builtinProviders[i].protocol
		}
	}
	return ParseOpenAI // 默认按 OpenAI 兼容解析
}

// DisplayName returns the human-friendly name for a provider.
func DisplayName(p Provider) string {
	for i := range builtinProviders {
		if builtinProviders[i].name == p {
			return builtinProviders[i].displayName
		}
	}
	return string(p)
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
