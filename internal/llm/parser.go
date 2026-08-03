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
	Role      string     `json:"role"`                 // "system" | "user" | "assistant" | "tool"
	Content   string     `json:"content"`              // text content (concatenated for multimodal)
	Name      string     `json:"name,omitempty"`       // tool name (OpenAI function calls)
	ToolCalls []ToolCall `json:"tool_calls,omitempty"` // 历史 assistant 消息中的工具调用
}

// ToolDefinition 表示一个工具/函数定义（OpenAI tools 数组项的 function 部分）。
// 跨 provider 统一格式：Anthropic schema 与 OpenAI 不同，但在此抽象为统一结构。
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"` // JSON Schema 参数定义（原样保留）
}

// ToolCall 表示一次工具调用（assistant 消息的 tool_calls 数组项）。
type ToolCall struct {
	ID        string `json:"id,omitempty"`        // OpenAI 调用 ID（tool 角色消息需引用）
	Name      string `json:"name"`                // 函数名
	Arguments string `json:"arguments,omitempty"` // 参数 JSON 字符串（原样保留，便于调试）
}

// RequestInfo holds extracted data from an LLM API request body.
type RequestInfo struct {
	Provider Provider  `json:"provider"`
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	System   string    `json:"system,omitempty"` // Anthropic-style top-level system prompt
	Stream   bool      `json:"stream"`
	// Usage hints from the request side (optional)
	MaxTokens   int     `json:"max_tokens,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
	// Tools 字段：请求中声明的工具定义（OpenAI tools / Anthropic tools / Gemini functionDeclarations）
	Tools []ToolDefinition `json:"tools,omitempty"`
	// ToolChoice 请求指定的工具选择策略（"auto"/"none"/"required"/具体对象）
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`
}

// ResponseInfo holds extracted data from an LLM API response.
type ResponseInfo struct {
	Model        string `json:"model,omitempty"`
	Content      string `json:"content"` // assembled full response text
	FinishReason string `json:"finish_reason,omitempty"`
	// Token usage
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
	// ToolCalls 工具调用列表（assistant 调用工具时填充）
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// ── Request parsing ───────────────────────────────────────────

// GeminiModelFromPath extracts the model name from a Gemini API URL path.
// Gemini 把模型名放在 URL 路径中而非请求体（如
// "/v1beta/models/gemini-2.5-flash:generateContent"），aiplatform 路径形如
// "/v1beta1/projects/p/locations/l/publishers/google/models/gemini-2.5-flash:predict"，
// 两者都含 "/models/<model>:"。取 "/models/" 之后到 ':'/ '/'/'?'/ '#' 之前的部分。
// Gemini carries the model in the URL path, not the request body.
// Returns "" when the path does not contain a model segment.
func GeminiModelFromPath(path string) string {
	marker := "/models/"
	i := strings.Index(path, marker)
	if i < 0 {
		return ""
	}
	rest := path[i+len(marker):]
	end := len(rest)
	for j := 0; j < len(rest); j++ {
		switch rest[j] {
		case ':', '/', '?', '#':
			end = j
		}
		if end != len(rest) {
			break
		}
	}
	return rest[:end]
}

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
		Tools       []struct {
			Type     string `json:"type"`
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
		ToolChoice json.RawMessage `json:"tool_choice"`
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
			// 历史 assistant 消息中的工具调用（OpenAI 格式）。
			// Tool calls carried by historical assistant messages (OpenAI format).
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		}
		if err := json.Unmarshal(rawMsg, &msg); err != nil {
			continue
		}
		content := extractTextContent(msg.Content)
		var toolCalls []ToolCall
		for _, tc := range msg.ToolCalls {
			toolCalls = append(toolCalls, ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
		info.Messages = append(info.Messages, Message{
			Role:      msg.Role,
			Content:   content,
			Name:      msg.Name,
			ToolCalls: toolCalls,
		})
	}
	// 解析 tools
	for _, t := range raw.Tools {
		if t.Type == "function" || t.Type == "" {
			info.Tools = append(info.Tools, ToolDefinition{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  t.Function.Parameters,
			})
		}
	}
	if len(raw.ToolChoice) > 0 {
		info.ToolChoice = raw.ToolChoice
	}
	return info
}

func parseAnthropicRequest(body []byte) *RequestInfo {
	var raw struct {
		Model       string            `json:"model"`
		System      json.RawMessage   `json:"system"`
		Messages    []json.RawMessage `json:"messages"`
		Stream      bool              `json:"stream"`
		MaxTokens   int               `json:"max_tokens"`
		Temperature float64           `json:"temperature"`
		Tools       []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
		ToolChoice json.RawMessage `json:"tool_choice"`
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
	for _, t := range raw.Tools {
		info.Tools = append(info.Tools, ToolDefinition{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		})
	}
	if len(raw.ToolChoice) > 0 {
		info.ToolChoice = raw.ToolChoice
	}
	return info
}

func parseGeminiRequest(body []byte) *RequestInfo {
	var raw struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text string `json:"text"`
				// Gemini functionCall 出现在 model 回复中（assistant 工具调用）。
				// functionCall appears in model responses (assistant tool calls).
				FunctionCall *struct {
					Name string          `json:"name"`
					Args json.RawMessage `json:"args"`
				} `json:"functionCall"`
				// functionResponse 出现在 function 角色消息中（工具返回结果）。
				// functionResponse appears in function-role messages (tool results).
				FunctionResponse *struct {
					Name     string          `json:"name"`
					Response json.RawMessage `json:"response"`
				} `json:"functionResponse"`
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
		Tools []struct {
			FunctionDeclarations []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"functionDeclarations"`
		} `json:"tools"`
		ToolConfig *struct {
			FunctionCallingConfig *struct {
				Mode string `json:"mode"`
			} `json:"functionCallingConfig"`
		} `json:"toolConfig"`
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
		var toolCalls []ToolCall
		for _, p := range c.Parts {
			switch {
			case p.FunctionCall != nil:
				// model 回复中的工具调用
				toolCalls = append(toolCalls, ToolCall{
					Name:      p.FunctionCall.Name,
					Arguments: string(p.FunctionCall.Args),
				})
			case p.FunctionResponse != nil:
				// 工具返回结果（response 是 JSON 对象），原样写入 content
				sb.WriteString(string(p.FunctionResponse.Response))
			default:
				sb.WriteString(p.Text)
			}
		}
		role := c.Role
		if role == "model" {
			role = "assistant"
		} else if role == "function" {
			// Gemini function 角色对应 OpenAI tool 角色
			role = "tool"
		} else if role == "" {
			role = "user"
		}
		info.Messages = append(info.Messages, Message{
			Role:      role,
			Content:   sb.String(),
			ToolCalls: toolCalls,
		})
	}
	if raw.GenerationConfig != nil {
		info.MaxTokens = raw.GenerationConfig.MaxOutputTokens
		info.Temperature = raw.GenerationConfig.Temperature
	}
	for _, t := range raw.Tools {
		for _, fd := range t.FunctionDeclarations {
			info.Tools = append(info.Tools, ToolDefinition{
				Name:        fd.Name,
				Description: fd.Description,
				Parameters:  fd.Parameters,
			})
		}
	}
	if raw.ToolConfig != nil && raw.ToolConfig.FunctionCallingConfig != nil {
		// Gemini 模式：BLOCK_ONLY / ANY / NONE / AUTO → 转为 JSON 字符串。
		// 用 json.Marshal 转义，避免原字符串拼接产生非法 JSON。
		// Marshal the mode string to avoid raw concatenation producing invalid JSON.
		if b, err := json.Marshal(raw.ToolConfig.FunctionCallingConfig.Mode); err == nil {
			info.ToolChoice = json.RawMessage(b)
		}
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
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			var seg string
			switch p.Type {
			case "tool_result":
				// Anthropic tool_result 的文本在 content 字段中（可能是 string
				// 或 text 块数组），递归提取。原代码仅取 text 字段会丢弃这部分文本。
				// Anthropic tool_result text lives in the content field (string or
				// array of text blocks); recurse to extract it.
				seg = extractTextContent(p.Content)
			default:
				// text 块或无类型（OpenAI vision text part）
				seg = p.Text
			}
			if seg != "" {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(seg)
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
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
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
		for _, tc := range raw.Choices[0].Message.ToolCalls {
			info.ToolCalls = append(info.ToolCalls, ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
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
		Model   string `json:"model"`
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
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
		} else if b.Type == "tool_use" {
			info.ToolCalls = append(info.ToolCalls, ToolCall{
				ID:        b.ID,
				Name:      b.Name,
				Arguments: string(b.Input),
			})
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
					Text         string `json:"text"`
					FunctionCall *struct {
						Name string          `json:"name"`
						Args json.RawMessage `json:"args"`
					} `json:"functionCall"`
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
			if p.FunctionCall != nil {
				info.ToolCalls = append(info.ToolCalls, ToolCall{
					Name:      p.FunctionCall.Name,
					Arguments: string(p.FunctionCall.Args),
				})
			} else if p.Text != "" {
				sb.WriteString(p.Text)
			}
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
	provider  Provider
	model     string
	content   strings.Builder
	finish    string
	usage     *ResponseInfo
	toolCalls []ToolCall
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
		Model   string `json:"model"`
		Choices []struct {
			Delta struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		// OpenAI 流式最后一个 chunk 在顶层携带 usage 对象。
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
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
		for _, tc := range c.Delta.ToolCalls {
			// OpenAI 流式：第一个 chunk 含 id 和 name，后续 chunk 仅含 arguments 增量
			idx := tc.Index
			// 防御：负数 index 直接忽略，避免越界 panic
			// Defensive: skip negative index to avoid out-of-bounds panic
			if idx < 0 {
				continue
			}
			// 防御：超大 index 直接忽略，避免恶意 chunk 触发 OOM
			// Defensive: skip absurdly large index to avoid OOM from preallocating slices
			if idx > 64 {
				continue
			}
			for len(a.toolCalls) <= idx {
				a.toolCalls = append(a.toolCalls, ToolCall{})
			}
			if tc.ID != "" {
				a.toolCalls[idx].ID = tc.ID
			}
			if tc.Function.Name != "" {
				a.toolCalls[idx].Name = tc.Function.Name
			}
			a.toolCalls[idx].Arguments += tc.Function.Arguments
		}
		if c.FinishReason != "" {
			a.finish = c.FinishReason
		}
	}
	// OpenAI 流式最后一个 chunk 含 usage 对象
	if chunk.Usage != nil {
		if a.usage == nil {
			a.usage = &ResponseInfo{}
		}
		a.usage.PromptTokens = chunk.Usage.PromptTokens
		a.usage.CompletionTokens = chunk.Usage.CompletionTokens
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
		// Anthropic SSE 规范：message 对象在顶层（不在 delta 字段中），
		// 必须直接从 data 解析。原代码错误地从 event.Delta 解析导致对真实流必然失败。
		// Anthropic SSE spec: the message object is at the top level (not inside delta),
		// so we must parse from data directly.
		var ev struct {
			Message struct {
				Model string `json:"model"`
				Usage *struct {
					InputTokens int `json:"input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal(data, &ev); err == nil {
			a.model = ev.Message.Model
			if a.usage == nil {
				a.usage = &ResponseInfo{}
			}
			if ev.Message.Usage != nil {
				a.usage.PromptTokens = ev.Message.Usage.InputTokens
			}
		}
	case "content_block_start":
		// content_block_start 事件的 content_block 字段在顶层（不在 delta 中），
		// 需要直接从 data 解析。修复：原代码错误使用 event.Delta，导致解析失败。
		// content_block_start event has content_block at the top level (not inside delta),
		// so we must parse from data directly. Fix: original code incorrectly used event.Delta.
		var ev struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
		}
		if err := json.Unmarshal(data, &ev); err == nil && ev.ContentBlock.Type == "tool_use" {
			a.toolCalls = append(a.toolCalls, ToolCall{
				ID:   ev.ContentBlock.ID,
				Name: ev.ContentBlock.Name,
			})
		}
	case "content_block_delta":
		var delta struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Partial string `json:"partial_json"`
		}
		if err := json.Unmarshal(event.Delta, &delta); err == nil {
			if delta.Type == "text_delta" {
				a.content.WriteString(delta.Text)
			} else if delta.Type == "input_json_delta" && len(a.toolCalls) > 0 {
				// partial_json 是字符串类型（Anthropic SSE 规范），
				// Go 自动 unmarshal 去除 JSON 外层引号。
				// 修复：原代码定义为 json.RawMessage 导致 string() 转换保留引号，
				// 最终 arguments 不是合法 JSON。
				// partial_json is a string type (Anthropic SSE spec);
				// Go auto-unmarshals and strips JSON outer quotes.
				// Fix: original code defined it as json.RawMessage, causing string()
				// conversion to keep quotes, making final arguments invalid JSON.
				a.toolCalls[len(a.toolCalls)-1].Arguments += delta.Partial
			}
		}
	case "message_delta":
		var delta struct {
			StopReason string `json:"stop_reason"`
			Usage      *struct {
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
	// Gemini streaming returns array of candidates objects; 非流式可能返回单个对象。
	// Parts 中可能含 text 或 functionCall（与非流式 parseGeminiResponse 保持一致）。
	// Gemini streaming returns array of candidates objects; non-streaming may
	// return a single object. Parts may contain text or functionCall.
	type geminiPart struct {
		Text         string `json:"text"`
		FunctionCall *struct {
			Name string          `json:"name"`
			Args json.RawMessage `json:"args"`
		} `json:"functionCall"`
	}
	type geminiCandidate struct {
		Content struct {
			Parts []geminiPart `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	}
	type geminiChunk struct {
		Candidates []geminiCandidate `json:"candidates"`
		// Gemini 流式最后一个 chunk 含 usageMetadata
		UsageMetadata *struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
			TotalTokenCount      int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}

	// process 处理单个 chunk（候选 + usage），统一数组和单对象路径，避免逻辑重复。
	process := func(ch geminiChunk) {
		for _, cnd := range ch.Candidates {
			for _, p := range cnd.Content.Parts {
				if p.FunctionCall != nil {
					a.toolCalls = append(a.toolCalls, ToolCall{
						Name:      p.FunctionCall.Name,
						Arguments: string(p.FunctionCall.Args),
					})
				} else if p.Text != "" {
					a.content.WriteString(p.Text)
				}
			}
			if cnd.FinishReason != "" {
				a.finish = cnd.FinishReason
			}
		}
		if ch.UsageMetadata != nil {
			if a.usage == nil {
				a.usage = &ResponseInfo{}
			}
			a.usage.PromptTokens = ch.UsageMetadata.PromptTokenCount
			a.usage.CompletionTokens = ch.UsageMetadata.CandidatesTokenCount
		}
	}

	// 优先按数组解析；失败则按单对象解析，走同一处理路径。
	var chunks []geminiChunk
	if err := json.Unmarshal(data, &chunks); err == nil {
		for _, ch := range chunks {
			process(ch)
		}
		return true
	}
	var single geminiChunk
	if err := json.Unmarshal(data, &single); err != nil {
		return false
	}
	process(single)
	return true
}

// Result returns the assembled response info.
func (a *StreamAssembler) Result() *ResponseInfo {
	info := &ResponseInfo{
		Model:        a.model,
		Content:      a.content.String(),
		FinishReason: a.finish,
		ToolCalls:    a.toolCalls,
	}
	if a.usage != nil {
		info.PromptTokens = a.usage.PromptTokens
		info.CompletionTokens = a.usage.CompletionTokens
		info.TotalTokens = a.usage.PromptTokens + a.usage.CompletionTokens
	}
	return info
}
