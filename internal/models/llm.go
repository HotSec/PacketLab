package models

import "time"

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
}

// LLMMessage is a single message in the LLM conversation.
type LLMMessage struct {
	Role    string `json:"role"`    // "system" | "user" | "assistant" | "tool"
	Content string `json:"content"`
}

// LLMUsage holds token usage statistics.
type LLMUsage struct {
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
}
