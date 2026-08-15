package llm

import "testing"

// TestDetectProviderBuiltin 内置厂商检测：精确匹配、后缀匹配、伪造 host 拒绝。
func TestDetectProviderBuiltin(t *testing.T) {
	tests := []struct {
		name     string
		host     string
		path     string
		expected Provider
	}{
		// 国际大厂
		{"openai exact", "api.openai.com", "/v1/chat/completions", ProviderOpenAI},
		{"openai subdomain", "api.openai.com", "", ProviderOpenAI},
		{"openai forged", "openai.com.evil.com", "/v1/chat/completions", ProviderUnknown},
		{"anthropic exact", "api.anthropic.com", "/v1/messages", ProviderAnthropic},
		{"anthropic subdomain", "console.anthropic.com", "", ProviderAnthropic},
		{"gemini generative", "generativelanguage.googleapis.com", "/v1beta/models/x:generateContent", ProviderGemini},
		{"gemini aiplatform", "us-central1-aiplatform.googleapis.com", "", ProviderGemini},
		// 国内厂商
		{"deepseek", "api.deepseek.com", "/chat/completions", ProviderDeepSeek},
		{"moonshot cn", "api.moonshot.cn", "/v1/chat/completions", ProviderMoonshot},
		{"moonshot intl", "api.moonshot.ai", "/v1/chat/completions", ProviderMoonshot},
		{"zhipu", "open.bigmodel.cn", "/api/paas/v4/chat/completions", ProviderZhipu},
		{"minimax chat", "api.minimax.chat", "/v1/text/chatcompletion_v2", ProviderMiniMax},
		{"minimax intl", "api.minimaxi.com", "/v1/text/chatcompletion_v2", ProviderMiniMax},
		{"qwen dashscope", "dashscope.aliyuncs.com", "/compatible-mode/v1/chat/completions", ProviderQwen},
		{"xai", "api.x.ai", "/v1/chat/completions", ProviderXAI},
		// 非 LLM
		{"random host", "example.com", "/v1/chat/completions", ProviderUnknown},
		{"github", "api.github.com", "/repos", ProviderUnknown},
		{"empty host", "", "", ProviderUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DetectProvider(tt.host, tt.path); got != tt.expected {
				t.Errorf("DetectProvider(%q, %q) = %q, want %q", tt.host, tt.path, got, tt.expected)
			}
		})
	}
}

// TestDetectProviderForged 伪造 host 不得命中内置厂商（安全回归）。
func TestDetectProviderForged(t *testing.T) {
	forged := []string{
		"api.openai.com.evil.com",
		"evil-openai.com",
		"api.deepseek.com.attacker.io",
		"open.bigmodel.cn.evil.cn",
		"notapi.anthropic.com.xyz",
	}
	for _, host := range forged {
		if got := DetectProvider(host, ""); got != ProviderUnknown {
			t.Errorf("forged host %q detected as %q, want unknown", host, got)
		}
	}
}

// TestProtocolFor 协议路由：国内厂商应走 OpenAI 兼容协议。
func TestProtocolFor(t *testing.T) {
	tests := []struct {
		p        Provider
		expected ParseProtocol
	}{
		{ProviderOpenAI, ParseOpenAI},
		{ProviderAnthropic, ParseAnthropic},
		{ProviderGemini, ParseGemini},
		{ProviderDeepSeek, ParseOpenAI},
		{ProviderMoonshot, ParseOpenAI},
		{ProviderZhipu, ParseOpenAI},
		{ProviderMiniMax, ParseOpenAI},
		{ProviderQwen, ParseOpenAI},
		{ProviderXAI, ParseOpenAI},
		{ProviderUnknown, ParseOpenAI}, // 默认按 OpenAI 兼容
	}
	for _, tt := range tests {
		if got := ProtocolFor(tt.p); got != tt.expected {
			t.Errorf("ProtocolFor(%q) = %q, want %q", tt.p, got, tt.expected)
		}
	}
}

// TestDisplayName 展示名非空且覆盖国内厂商。
func TestDisplayName(t *testing.T) {
	for _, p := range []Provider{ProviderDeepSeek, ProviderZhipu, ProviderMoonshot, ProviderQwen, ProviderMiniMax, ProviderXAI} {
		if got := DisplayName(p); got == "" || got == string(p) {
			t.Errorf("DisplayName(%q) = %q, want a friendly name", p, got)
		}
	}
}

// TestParseRequestChineseProvider 国内厂商请求体按 OpenAI 格式解析。
func TestParseRequestChineseProvider(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","stream":true,"messages":[
		{"role":"system","content":"你是助手"},
		{"role":"user","content":"你好"}
	]}`)
	info := ParseRequest(ProviderZhipu, body)
	if info == nil {
		t.Fatal("ParseRequest returned nil for zhipu (OpenAI-compatible) body")
	}
	if info.Model != "glm-5.2" || !info.Stream {
		t.Errorf("parsed = %+v, want model glm-5.2 stream true", info)
	}
	if len(info.Messages) != 2 || info.Messages[1].Content != "你好" {
		t.Errorf("messages = %+v, want 2 messages with Chinese content intact", info.Messages)
	}
}

// TestParseResponseChineseProvider 国内厂商响应体按 OpenAI 格式解析（含 usage）。
func TestParseResponseChineseProvider(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","choices":[{"message":{"content":"你好！"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	info := ParseResponse(ProviderZhipu, body)
	if info == nil {
		t.Fatal("ParseResponse returned nil")
	}
	if info.Content != "你好！" || info.PromptTokens != 10 || info.CompletionTokens != 5 {
		t.Errorf("parsed = %+v", info)
	}
}
