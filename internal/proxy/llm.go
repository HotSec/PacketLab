package proxy

// llm.go — LLM traffic integration for the proxy.
// Detects LLM API calls, parses prompts and responses, and persists structured data.

import (
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"packetlab/internal/llm"
	"packetlab/internal/models"
)

// processLLMRequest parses an LLM request body and returns the extracted info.
// Returns nil if parsing fails.
func processLLMRequest(provider llm.Provider, reqBody string) *llm.RequestInfo {
	if reqBody == "" {
		return nil
	}
	return llm.ParseRequest(provider, []byte(reqBody))
}

// processLLMResponse parses a complete (non-streamed) LLM response body.
func processLLMResponse(provider llm.Provider, resBody string) *llm.ResponseInfo {
	if resBody == "" {
		return nil
	}
	return llm.ParseResponse(provider, []byte(resBody))
}

// buildLLMExchange assembles the full LLM exchange from request and response info.
func buildLLMExchange(provider llm.Provider, reqInfo *llm.RequestInfo, resInfo *llm.ResponseInfo, capturedAt time.Time) *models.LLMExchange {
	ex := &models.LLMExchange{
		Provider:  string(provider),
		RequestAt: capturedAt,
	}
	if reqInfo != nil {
		ex.Model = reqInfo.Model
		ex.Stream = reqInfo.Stream
		ex.System = reqInfo.System
		for _, m := range reqInfo.Messages {
			ex.Messages = append(ex.Messages, models.LLMMessage{
				Role:    m.Role,
				Content: m.Content,
			})
		}
		// 转换 tools
		for _, t := range reqInfo.Tools {
			ex.Tools = append(ex.Tools, models.LLMToolDefinition{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			})
		}
		if len(reqInfo.ToolChoice) > 0 {
			ex.ToolChoice = reqInfo.ToolChoice
		}
	}
	if resInfo != nil {
		if ex.Model == "" {
			ex.Model = resInfo.Model
		}
		ex.Response = resInfo.Content
		if resInfo.PromptTokens > 0 || resInfo.CompletionTokens > 0 || resInfo.TotalTokens > 0 {
			ex.Usage = &models.LLMUsage{
				PromptTokens:     resInfo.PromptTokens,
				CompletionTokens: resInfo.CompletionTokens,
				TotalTokens:      resInfo.TotalTokens,
			}
		}
		// 转换 tool_calls
		for _, tc := range resInfo.ToolCalls {
			ex.ToolCalls = append(ex.ToolCalls, models.LLMToolCall{
				ID:        tc.ID,
				Name:      tc.Name,
				Arguments: tc.Arguments,
			})
		}
	}
	return ex
}

// saveLLMExchange saves the LLM exchange data to the store.
func saveLLMExchange(store interface {
	SetLLMData(id int64, llmDataJSON string) error
}, id int64, ex *models.LLMExchange) {
	if ex == nil || id == 0 {
		return
	}
	data, err := json.Marshal(ex)
	if err != nil {
		slog.Warn("proxy: LLM exchange marshal failed", "id", id, "error", err)
		return
	}
	if err := store.SetLLMData(id, string(data)); err != nil {
		slog.Warn("proxy: LLM exchange save failed", "id", id, "error", err)
	}
}

// parseSSEDataLine extracts the JSON payload from an SSE "data:" line.
// Returns the raw JSON bytes, or nil if the line is not a data line or is [DONE].
func parseSSEDataLine(line string) []byte {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return nil
	}
	data := strings.TrimSpace(line[5:])
	if data == "[DONE]" || data == "" {
		return nil
	}
	return []byte(data)
}
