package llm

import (
	"testing"
)

func TestDetectProvider(t *testing.T) {
	tests := []struct {
		host     string
		path     string
		expected Provider
	}{
		{"api.openai.com", "/v1/chat/completions", ProviderOpenAI},
		{"openai.com", "/v1/completions", ProviderOpenAI},
		{"api.anthropic.com", "/v1/messages", ProviderAnthropic},
		{"generativelanguage.googleapis.com", "/v1beta/models/gemini-pro:generateContent", ProviderGemini},
		{"example.com", "/api/data", ProviderUnknown},
	}
	for _, tt := range tests {
		got := DetectProvider(tt.host, tt.path)
		if got != tt.expected {
			t.Errorf("DetectProvider(%q, %q) = %v, want %v", tt.host, tt.path, got, tt.expected)
		}
	}
}

func TestParseOpenAIRequest(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4",
		"messages": [
			{"role": "system", "content": "You are helpful."},
			{"role": "user", "content": "Hello"}
		],
		"stream": true,
		"max_tokens": 100,
		"temperature": 0.7
	}`)
	info := ParseRequest(ProviderOpenAI, body)
	if info == nil {
		t.Fatal("ParseRequest returned nil")
	}
	if info.Model != "gpt-4" {
		t.Errorf("model = %q, want gpt-4", info.Model)
	}
	if len(info.Messages) != 2 {
		t.Fatalf("messages count = %d, want 2", len(info.Messages))
	}
	if info.Messages[0].Role != "system" || info.Messages[0].Content != "You are helpful." {
		t.Errorf("message[0] unexpected: %+v", info.Messages[0])
	}
	if info.Messages[1].Role != "user" || info.Messages[1].Content != "Hello" {
		t.Errorf("message[1] unexpected: %+v", info.Messages[1])
	}
	if !info.Stream {
		t.Error("stream should be true")
	}
	if info.MaxTokens != 100 {
		t.Errorf("max_tokens = %d, want 100", info.MaxTokens)
	}
	if info.Temperature != 0.7 {
		t.Errorf("temperature = %f, want 0.7", info.Temperature)
	}
}

func TestParseOpenAIRequestVision(t *testing.T) {
	// OpenAI vision format: content is array of parts
	body := []byte(`{
		"model": "gpt-4o",
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "What's in this image?"},
				{"type": "image_url", "image_url": {"url": "..."}}
			]}
		]
	}`)
	info := ParseRequest(ProviderOpenAI, body)
	if info == nil {
		t.Fatal("ParseRequest returned nil")
	}
	if len(info.Messages) != 1 {
		t.Fatalf("messages count = %d, want 1", len(info.Messages))
	}
	if info.Messages[0].Content != "What's in this image?" {
		t.Errorf("content = %q, want 'What's in this image?'", info.Messages[0].Content)
	}
}

func TestParseAnthropicRequest(t *testing.T) {
	body := []byte(`{
		"model": "claude-3-opus-20240229",
		"system": "You are an AI assistant.",
		"messages": [
			{"role": "user", "content": "Tell me a joke"}
		],
		"max_tokens": 200
	}`)
	info := ParseRequest(ProviderAnthropic, body)
	if info == nil {
		t.Fatal("ParseRequest returned nil")
	}
	if info.Model != "claude-3-opus-20240229" {
		t.Errorf("model = %q", info.Model)
	}
	if info.System != "You are an AI assistant." {
		t.Errorf("system = %q", info.System)
	}
	if len(info.Messages) != 1 || info.Messages[0].Content != "Tell me a joke" {
		t.Errorf("messages unexpected: %+v", info.Messages)
	}
}

func TestParseGeminiRequest(t *testing.T) {
	body := []byte(`{
		"contents": [
			{"role": "user", "parts": [{"text": "Hello Gemini"}]}
		],
		"systemInstruction": {
			"parts": [{"text": "Be concise."}]
		}
	}`)
	info := ParseRequest(ProviderGemini, body)
	if info == nil {
		t.Fatal("ParseRequest returned nil")
	}
	if info.System != "Be concise." {
		t.Errorf("system = %q", info.System)
	}
	if len(info.Messages) != 1 {
		t.Fatalf("messages count = %d", len(info.Messages))
	}
	if info.Messages[0].Role != "user" || info.Messages[0].Content != "Hello Gemini" {
		t.Errorf("message unexpected: %+v", info.Messages[0])
	}
}

func TestParseOpenAIResponse(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4",
		"choices": [
			{"message": {"content": "Hi there!"}, "finish_reason": "stop"}
		],
		"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	}`)
	info := ParseResponse(ProviderOpenAI, body)
	if info == nil {
		t.Fatal("ParseResponse returned nil")
	}
	if info.Content != "Hi there!" {
		t.Errorf("content = %q", info.Content)
	}
	if info.PromptTokens != 10 || info.CompletionTokens != 5 || info.TotalTokens != 15 {
		t.Errorf("usage unexpected: %+v", info)
	}
}

func TestStreamAssemblerOpenAI(t *testing.T) {
	a := NewStreamAssembler(ProviderOpenAI)

	chunks := [][]byte{
		[]byte(`{"model":"gpt-4","choices":[{"delta":{"content":"Hello"}}]}`),
		[]byte(`{"choices":[{"delta":{"content":" world"}}]}`),
		[]byte(`{"choices":[{"delta":{"content":"!"},"finish_reason":"stop"}]}`),
		[]byte(`[DONE]`),
	}
	for _, chunk := range chunks {
		a.Feed(chunk)
	}

	result := a.Result()
	if result.Content != "Hello world!" {
		t.Errorf("content = %q, want 'Hello world!'", result.Content)
	}
	if result.Model != "gpt-4" {
		t.Errorf("model = %q", result.Model)
	}
	if result.FinishReason != "stop" {
		t.Errorf("finish_reason = %q", result.FinishReason)
	}
}

func TestStreamAssemblerAnthropic(t *testing.T) {
	a := NewStreamAssembler(ProviderAnthropic)

	chunks := [][]byte{
		[]byte(`{"type":"message_start","delta":{"model":"claude-3","message":{"usage":{"input_tokens":10}}}}`),
		[]byte(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}`),
		[]byte(`{"type":"content_block_delta","delta":{"type":"text_delta","text":" world"}}`),
		[]byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn","usage":{"output_tokens":5}}}`),
	}
	for _, chunk := range chunks {
		a.Feed(chunk)
	}

	result := a.Result()
	if result.Content != "Hello world" {
		t.Errorf("content = %q, want 'Hello world'", result.Content)
	}
	if result.Model != "claude-3" {
		t.Errorf("model = %q", result.Model)
	}
	if result.FinishReason != "end_turn" {
		t.Errorf("finish_reason = %q", result.FinishReason)
	}
	if result.PromptTokens != 10 {
		t.Errorf("prompt_tokens = %d", result.PromptTokens)
	}
	if result.CompletionTokens != 5 {
		t.Errorf("completion_tokens = %d", result.CompletionTokens)
	}
}

func TestStreamAssemblerGemini(t *testing.T) {
	a := NewStreamAssembler(ProviderGemini)

	// Gemini streaming sends array of objects
	chunks := [][]byte{
		[]byte(`[{"candidates":[{"content":{"parts":[{"text":"Hello "}]},"finishReason":"STOP"}]}]`),
		[]byte(`[{"candidates":[{"content":{"parts":[{"text":"world"}]}}]}]`),
	}
	for _, chunk := range chunks {
		a.Feed(chunk)
	}

	result := a.Result()
	if result.Content != "Hello world" {
		t.Errorf("content = %q, want 'Hello world'", result.Content)
	}
}
