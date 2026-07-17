package models

import (
	"encoding/json"
	"time"
)

// LLMExchange represents a captured LLM API interaction (request + response).
// Stored as a JSON blob in the llm_data column of the requests table.
type LLMExchange struct {
	Provider  string         `json:"provider"`   // "openai" | "anthropic" | "gemini" | "unknown"
	Model     string         `json:"model"`      // model name from request/response
	Stream    bool           `json:"stream"`     // whether the response was streamed
	System    string         `json:"system,omitempty"`
	Messages  []LLMMessage   `json:"messages"`   // request messages (conversation context)
	Response  string         `json:"response"`   // assembled response text
	Usage     *LLMUsage      `json:"usage,omitempty"`
	RequestAt time.Time      `json:"request_at"`
	// Tools 请求中声明的工具定义
	Tools      []LLMToolDefinition `json:"tools,omitempty"`
	// ToolChoice 请求指定的工具选择策略
	ToolChoice json.RawMessage     `json:"tool_choice,omitempty"`
	// ToolCalls 响应中的工具调用列表
	ToolCalls  []LLMToolCall       `json:"tool_calls,omitempty"`
}

// LLMMessage is a single message in the LLM conversation.
type LLMMessage struct {
	Role    string `json:"role"`    // "system" | "user" | "assistant" | "tool"
	Content string `json:"content"`
}

// LLMToolDefinition 工具定义（跨 provider 统一）
type LLMToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// LLMToolCall 工具调用记录
type LLMToolCall struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
}

// LLMUsage holds token usage statistics.
type LLMUsage struct {
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
	// CostUSD 估算成本（美元）。模型未在定价表中时为 0。
	CostUSD          float64 `json:"cost_usd,omitempty"`
}
