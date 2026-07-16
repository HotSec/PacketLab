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
func DetectProvider(host, path string) Provider {
	host = strings.ToLower(host)
	path = strings.ToLower(path)
	for provider, pat := range providerPatterns {
		for _, hs := range pat.hostSubstrs {
			if strings.Contains(host, hs) {
				return provider
			}
		}
		for _, ps := range pat.pathSubstrs {
			if strings.Contains(path, ps) {
				return provider
			}
		}
	}
	return ProviderUnknown
}

// IsLLMRequest determines whether a request is likely an LLM API call.
// Checks both provider patterns and a heuristic on the request body.
func IsLLMRequest(host, path string) bool {
	return DetectProvider(host, path) != ProviderUnknown
}

// IsValidProvider returns true if the provider is a known LLM provider
// (not unknown and not empty).
func IsValidProvider(p Provider) bool {
	return p != ProviderUnknown && p != ""
}
