package llm

// parser.go — Parse LLM API requests and responses (OpenAI, Anthropic, Gemini)
// Extract prompts, model names, parameters, and assemble streamed responses.

import (
	"encoding/json"
	"strings"
)

// ── Public types ──────────────────────────────────────────────

// Message represents a single message in a chat conversation.
type Message struct {
	Role    string `json:"role"`              // "system" | "user" | "assistant" | "tool"
	Content string `json:"content"`           // text content (concatenated for multimodal)
	Name    string `json:"name,omitempty"`    // tool name (OpenAI function calls)
}

// RequestInfo holds extracted data from an LLM API request body.
type RequestInfo struct {
	Provider Provider  `json:"provider"`
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	System   string    `json:"system,omitempty"`  // Anthropic-style top-level system prompt
	Stream   bool      `json:"stream"`
	// Usage hints from the request side (optional)
	MaxTokens   int     `json:"max_tokens,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
}

// ResponseInfo holds extracted data from an LLM API response.
type ResponseInfo struct {
	Model       string `json:"model,omitempty"`
	Content     string `json:"content"`              // assembled full response text
	FinishReason string `json:"finish_reason,omitempty"`
	// Token usage
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
}

// ── Request parsing ───────────────────────────────────────────

// ParseRequest extracts LLM request info from a JSON body.
func ParseRequest(provider Provider, body []byte) *RequestInfo {
	if len(body) == 0 {
		return nil
	}
	switch provider {
	case ProviderOpenAI:
		return parseOpenAIRequest(body)
	case ProviderAnthropic:
		return parseAnthropicRequest(body)
	case ProviderGemini:
		return parseGeminiRequest(body)
	}
	// Fallback: try OpenAI format (most common)
	return parseOpenAIRequest(body)
}

func parseOpenAIRequest(body []byte) *RequestInfo {
	var raw struct {
		Model       string            `json:"model"`
		Messages    []json.RawMessage `json:"messages"`
		Stream      bool              `json:"stream"`
		MaxTokens   int               `json:"max_tokens"`
		Temperature float64           `json:"temperature"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	info := &RequestInfo{
		Provider:    ProviderOpenAI,
		Model:       raw.Model,
		Stream:      raw.Stream,
		MaxTokens:   raw.MaxTokens,
		Temperature: raw.Temperature,
	}
	for _, rawMsg := range raw.Messages {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
			Name    string          `json:"name"`
		}
		if err := json.Unmarshal(rawMsg, &msg); err != nil {
			continue
		}
		content := extractTextContent(msg.Content)
		info.Messages = append(info.Messages, Message{
			Role:    msg.Role,
			Content: content,
			Name:    msg.Name,
		})
	}
	return info
}

func parseAnthropicRequest(body []byte) *RequestInfo {
	var raw struct {
		Model       string          `json:"model"`
		System      json.RawMessage `json:"system"`
		Messages    []json.RawMessage `json:"messages"`
		Stream      bool            `json:"stream"`
		MaxTokens   int             `json:"max_tokens"`
		Temperature float64         `json:"temperature"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	info := &RequestInfo{
		Provider:    ProviderAnthropic,
		Model:       raw.Model,
		Stream:      raw.Stream,
		MaxTokens:   raw.MaxTokens,
		Temperature: raw.Temperature,
		System:      extractTextContent(raw.System),
	}
	for _, rawMsg := range raw.Messages {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(rawMsg, &msg); err != nil {
			continue
		}
		content := extractTextContent(msg.Content)
		info.Messages = append(info.Messages, Message{
			Role:    msg.Role,
			Content: content,
		})
	}
	return info
}

func parseGeminiRequest(body []byte) *RequestInfo {
	var raw struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"contents"`
		SystemInstruction *struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"systemInstruction"`
		GenerationConfig *struct {
			MaxOutputTokens int     `json:"maxOutputTokens"`
			Temperature     float64 `json:"temperature"`
		} `json:"generationConfig"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	info := &RequestInfo{
		Provider: ProviderGemini,
	}
	if raw.SystemInstruction != nil {
		var sb strings.Builder
		for _, p := range raw.SystemInstruction.Parts {
			sb.WriteString(p.Text)
		}
		info.System = sb.String()
	}
	for _, c := range raw.Contents {
		var sb strings.Builder
		for _, p := range c.Parts {
			sb.WriteString(p.Text)
		}
		role := c.Role
		if role == "model" {
			role = "assistant"
		}
		if role == "" {
			role = "user"
		}
		info.Messages = append(info.Messages, Message{
			Role:    role,
			Content: sb.String(),
		})
	}
	if raw.GenerationConfig != nil {
		info.MaxTokens = raw.GenerationConfig.MaxOutputTokens
		info.Temperature = raw.GenerationConfig.Temperature
	}
	return info
}

// extractTextContent handles both string content and array-of-parts content
// (OpenAI vision format, Anthropic content blocks).
func extractTextContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try simple string first
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Try array of content parts
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			if p.Text != "" {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(p.Text)
			}
		}
		return sb.String()
	}
	return ""
}

// ── Response parsing ──────────────────────────────────────────

// ParseResponse extracts LLM response info from a complete (non-streamed) JSON body.
func ParseResponse(provider Provider, body []byte) *ResponseInfo {
	if len(body) == 0 {
		return nil
	}
	switch provider {
	case ProviderOpenAI:
		return parseOpenAIResponse(body)
	case ProviderAnthropic:
		return parseAnthropicResponse(body)
	case ProviderGemini:
		return parseGeminiResponse(body)
	}
	return parseOpenAIResponse(body)
}

func parseOpenAIResponse(body []byte) *ResponseInfo {
	var raw struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	info := &ResponseInfo{Model: raw.Model}
	if len(raw.Choices) > 0 {
		info.Content = raw.Choices[0].Message.Content
		info.FinishReason = raw.Choices[0].FinishReason
	}
	if raw.Usage != nil {
		info.PromptTokens = raw.Usage.PromptTokens
		info.CompletionTokens = raw.Usage.CompletionTokens
		info.TotalTokens = raw.Usage.TotalTokens
	}
	return info
}

func parseAnthropicResponse(body []byte) *ResponseInfo {
	var raw struct {
		Model    string `json:"model"`
		Content  []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	info := &ResponseInfo{
		Model:        raw.Model,
		FinishReason: raw.StopReason,
	}
	var sb strings.Builder
	for _, b := range raw.Content {
		if b.Type == "text" || b.Type == "" {
			sb.WriteString(b.Text)
		}
	}
	info.Content = sb.String()
	if raw.Usage != nil {
		info.PromptTokens = raw.Usage.InputTokens
		info.CompletionTokens = raw.Usage.OutputTokens
		info.TotalTokens = raw.Usage.InputTokens + raw.Usage.OutputTokens
	}
	return info
}

func parseGeminiResponse(body []byte) *ResponseInfo {
	var raw struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata *struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
			TotalTokenCount      int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	info := &ResponseInfo{}
	if len(raw.Candidates) > 0 {
		var sb strings.Builder
		for _, p := range raw.Candidates[0].Content.Parts {
			sb.WriteString(p.Text)
		}
		info.Content = sb.String()
		info.FinishReason = raw.Candidates[0].FinishReason
	}
	if raw.UsageMetadata != nil {
		info.PromptTokens = raw.UsageMetadata.PromptTokenCount
		info.CompletionTokens = raw.UsageMetadata.CandidatesTokenCount
		info.TotalTokens = raw.UsageMetadata.TotalTokenCount
	}
	return info
}

// ── SSE stream assembly ───────────────────────────────────────

// StreamAssembler incrementally assembles streamed LLM responses from SSE chunks.
// It auto-detects the streaming format from the first chunk and delegates accordingly.
type StreamAssembler struct {
	provider Provider
	model    string
	content  strings.Builder
	finish   string
	usage    *ResponseInfo
	detected bool
}

// NewStreamAssembler creates a new assembler for the given provider.
func NewStreamAssembler(provider Provider) *StreamAssembler {
	return &StreamAssembler{provider: provider}
}

// Feed processes a single SSE data payload (the part after "data: ").
// Returns true if the chunk was successfully processed.
func (a *StreamAssembler) Feed(data []byte) bool {
	// Handle [DONE] sentinel (OpenAI)
	if strings.TrimSpace(string(data)) == "[DONE]" {
		return true
	}
	switch a.provider {
	case ProviderAnthropic:
		return a.feedAnthropic(data)
	case ProviderGemini:
		return a.feedGemini(data)
	default:
		return a.feedOpenAI(data)
	}
}

func (a *StreamAssembler) feedOpenAI(data []byte) bool {
	var chunk struct {
		Model string `json:"model"`
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return false
	}
	if a.model == "" && chunk.Model != "" {
		a.model = chunk.Model
	}
	if len(chunk.Choices) > 0 {
		c := chunk.Choices[0]
		a.content.WriteString(c.Delta.Content)
		if c.FinishReason != "" {
			a.finish = c.FinishReason
		}
	}
	return true
}

func (a *StreamAssembler) feedAnthropic(data []byte) bool {
	var event struct {
		Type  string          `json:"type"`
		Delta json.RawMessage `json:"delta"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return false
	}
	switch event.Type {
	case "message_start":
		var msg struct {
			Model string `json:"model"`
			Message struct {
				Usage *struct {
					InputTokens int `json:"input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal(event.Delta, &msg); err == nil {
			a.model = msg.Model
			if a.usage == nil {
				a.usage = &ResponseInfo{}
			}
			a.usage.PromptTokens = msg.Message.Usage.InputTokens
		}
	case "content_block_delta":
		var delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(event.Delta, &delta); err == nil && delta.Type == "text_delta" {
			a.content.WriteString(delta.Text)
		}
	case "message_delta":
		var delta struct {
			StopReason string `json:"stop_reason"`
			Usage *struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(event.Delta, &delta); err == nil {
			if delta.StopReason != "" {
				a.finish = delta.StopReason
			}
			if delta.Usage != nil {
				if a.usage == nil {
					a.usage = &ResponseInfo{}
				}
				a.usage.CompletionTokens = delta.Usage.OutputTokens
			}
		}
	}
	return true
}

func (a *StreamAssembler) feedGemini(data []byte) bool {
	// Gemini streaming returns array of candidates objects
	var chunks []struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(data, &chunks); err != nil {
		// Try single object (non-array)
		var single struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
				FinishReason string `json:"finishReason"`
			} `json:"candidates"`
		}
		if err2 := json.Unmarshal(data, &single); err2 != nil {
			return false
		}
		for _, c := range single.Candidates {
			for _, p := range c.Content.Parts {
				a.content.WriteString(p.Text)
			}
			if c.FinishReason != "" {
				a.finish = c.FinishReason
			}
		}
		return true
	}
	for _, chunk := range chunks {
		for _, c := range chunk.Candidates {
			for _, p := range c.Content.Parts {
				a.content.WriteString(p.Text)
			}
			if c.FinishReason != "" {
				a.finish = c.FinishReason
			}
		}
	}
	return true
}

// Result returns the assembled response info.
func (a *StreamAssembler) Result() *ResponseInfo {
	info := &ResponseInfo{
		Model:        a.model,
		Content:      a.content.String(),
		FinishReason: a.finish,
	}
	if a.usage != nil {
		info.PromptTokens = a.usage.PromptTokens
		info.CompletionTokens = a.usage.CompletionTokens
		info.TotalTokens = a.usage.PromptTokens + a.usage.CompletionTokens
	}
	return info
}
