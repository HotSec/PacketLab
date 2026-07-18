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

func TestParseOpenAIRequestWithTools(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4",
		"messages": [{"role": "user", "content": "What's the weather?"}],
		"tools": [{
			"type": "function",
			"function": {
				"name": "get_weather",
				"description": "Get weather for a city",
				"parameters": {"type": "object", "properties": {"city": {"type": "string"}}}
			}
		}],
		"tool_choice": "auto"
	}`)
	info := ParseRequest(ProviderOpenAI, body)
	if info == nil {
		t.Fatal("ParseRequest returned nil")
	}
	if len(info.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(info.Tools))
	}
	if info.Tools[0].Name != "get_weather" {
		t.Errorf("tool name = %q, want get_weather", info.Tools[0].Name)
	}
	if info.Tools[0].Description != "Get weather for a city" {
		t.Errorf("tool desc = %q", info.Tools[0].Description)
	}
	if len(info.ToolChoice) == 0 {
		t.Error("expected tool_choice to be set")
	}
}

func TestParseOpenAIResponseWithToolCalls(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4",
		"choices": [{
			"message": {
				"role": "assistant",
				"content": null,
				"tool_calls": [{
					"id": "call_abc",
					"type": "function",
					"function": {
						"name": "get_weather",
						"arguments": "{\"city\":\"SF\"}"
					}
				}]
			},
			"finish_reason": "tool_calls"
		}]
	}`)
	info := ParseResponse(ProviderOpenAI, body)
	if info == nil {
		t.Fatal("ParseResponse returned nil")
	}
	if len(info.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(info.ToolCalls))
	}
	tc := info.ToolCalls[0]
	if tc.ID != "call_abc" {
		t.Errorf("id = %q, want call_abc", tc.ID)
	}
	if tc.Name != "get_weather" {
		t.Errorf("name = %q, want get_weather", tc.Name)
	}
	if tc.Arguments != `{"city":"SF"}` {
		t.Errorf("arguments = %q", tc.Arguments)
	}
	if info.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", info.FinishReason)
	}
}

func TestParseAnthropicRequestWithTools(t *testing.T) {
	body := []byte(`{
		"model": "claude-3",
		"max_tokens": 1024,
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [{
			"name": "search",
			"description": "Search the web",
			"input_schema": {"type": "object", "properties": {"q": {"type": "string"}}}
		}]
	}`)
	info := ParseRequest(ProviderAnthropic, body)
	if info == nil {
		t.Fatal("ParseRequest returned nil")
	}
	if len(info.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(info.Tools))
	}
	if info.Tools[0].Name != "search" {
		t.Errorf("tool name = %q, want search", info.Tools[0].Name)
	}
}

func TestParseAnthropicResponseWithToolUse(t *testing.T) {
	body := []byte(`{
		"model": "claude-3",
		"content": [
			{"type": "text", "text": "Let me search."},
			{"type": "tool_use", "id": "tool_1", "name": "search", "input": {"q": "Go 1.25"}}
		],
		"stop_reason": "tool_use"
	}`)
	info := ParseResponse(ProviderAnthropic, body)
	if info == nil {
		t.Fatal("ParseResponse returned nil")
	}
	if info.Content != "Let me search." {
		t.Errorf("content = %q", info.Content)
	}
	if len(info.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(info.ToolCalls))
	}
	if info.ToolCalls[0].ID != "tool_1" {
		t.Errorf("id = %q", info.ToolCalls[0].ID)
	}
	if info.ToolCalls[0].Name != "search" {
		t.Errorf("name = %q", info.ToolCalls[0].Name)
	}
	if info.ToolCalls[0].Arguments == "" {
		t.Error("expected non-empty arguments")
	}
}

func TestStreamAssemblerOpenAIToolCalls(t *testing.T) {
	a := NewStreamAssembler(ProviderOpenAI)
	// 第一个 chunk：含 id 和 name
	a.Feed([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`))
	// 第二个 chunk：arguments 增量
	a.Feed([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`))
	// 第三个 chunk：arguments 增量
	a.Feed([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"SF\"}"}}]}}]}`))
	a.Feed([]byte("[DONE]"))

	result := a.Result()
	if len(result.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(result.ToolCalls))
	}
	if result.ToolCalls[0].ID != "call_1" {
		t.Errorf("id = %q, want call_1", result.ToolCalls[0].ID)
	}
	if result.ToolCalls[0].Name != "get_weather" {
		t.Errorf("name = %q, want get_weather", result.ToolCalls[0].Name)
	}
	if result.ToolCalls[0].Arguments != `{"city":"SF"}` {
		t.Errorf("arguments = %q, want {\"city\":\"SF\"}", result.ToolCalls[0].Arguments)
	}
}

// TestStreamAssemblerAnthropicToolCalls 验证 Anthropic 流式 tool_use 增量拼接。
// 原始 bug：partial_json 字段定义为 json.RawMessage，导致追加时保留 JSON 外层引号，
// 最终 arguments 不是合法 JSON。修复：改为 string 类型，让 Go 自动 unmarshal。
func TestStreamAssemblerAnthropicToolCalls(t *testing.T) {
	a := NewStreamAssembler(ProviderAnthropic)
	// content_block_start: tool_use 块开始（含 id 和 name）
	a.Feed([]byte(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_01","name":"get_weather"}}`))
	// input_json_delta: arguments 增量（partial_json 是 string，值含内部转义）
	a.Feed([]byte(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`))
	a.Feed([]byte(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"SF\"}"}}`))

	result := a.Result()
	if len(result.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(result.ToolCalls))
	}
	if result.ToolCalls[0].ID != "toolu_01" {
		t.Errorf("id = %q, want toolu_01", result.ToolCalls[0].ID)
	}
	if result.ToolCalls[0].Name != "get_weather" {
		t.Errorf("name = %q, want get_weather", result.ToolCalls[0].Name)
	}
	// 期望拼接后的 arguments 是合法 JSON 字符串 {"city":"SF"}
	// bug 行为：partial_json 作为 RawMessage 时 string() 转换会保留外层引号，
	// 得到 "\"{\\\"city\\\":\"\"\\\"SF\\\"}\"，无法 JSON 解析
	if result.ToolCalls[0].Arguments != `{"city":"SF"}` {
		t.Errorf("arguments = %q, want {\"city\":\"SF\"}", result.ToolCalls[0].Arguments)
	}
}

// TestStreamAssemblerOpenAIToolCalls_NegativeIndex 验证 OpenAI 流式 tool_calls 中
// index 为负数时不会导致无限循环或越界。
func TestStreamAssemblerOpenAIToolCalls_NegativeIndex(t *testing.T) {
	a := NewStreamAssembler(ProviderOpenAI)
	// 异常 chunk：index 为负数
	a.Feed([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":-1,"id":"x","function":{"name":"f","arguments":""}}]}}]}`))
	a.Feed([]byte("[DONE]"))

	result := a.Result()
	// 负 index 的 tool_call 应被忽略，不应导致 panic 或无限循环
	if len(result.ToolCalls) != 0 {
		t.Errorf("expected 0 tool calls (negative index ignored), got %d", len(result.ToolCalls))
	}
}
