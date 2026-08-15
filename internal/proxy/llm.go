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
		// 模型上下文/输出限制（models.dev，未知时为 0，前端不展示）
		if lim := llm.LookupLimits(reqInfo.Model); lim.ContextLength > 0 || lim.MaxOutput > 0 {
			ex.ContextLength = lim.ContextLength
			ex.MaxOutput = lim.MaxOutput
		}
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
		// 优先使用真实 usage；缺失时按文本启发式估算（标记 TokensEstimated）
		promptTok, compTok, totalTok, estimated := usageWithFallback(reqInfo, resInfo)
		if promptTok > 0 || compTok > 0 || totalTok > 0 {
			cost := llm.EstimateCost(ex.Model, promptTok, compTok)
			ex.Usage = &models.LLMUsage{
				PromptTokens:     promptTok,
				CompletionTokens: compTok,
				TotalTokens:      totalTok,
				CostUSD:          cost,
				TokensEstimated:  estimated,
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

// usageWithFallback 计算 token 用量：优先响应 usage，缺失时按请求/响应文本估算。
// 返回 (prompt, completion, total, estimated)。estimated=true 表示数值来自估算而非上游 usage。
func usageWithFallback(reqInfo *llm.RequestInfo, resInfo *llm.ResponseInfo) (int, int, int, bool) {
	if resInfo != nil && (resInfo.PromptTokens > 0 || resInfo.CompletionTokens > 0 || resInfo.TotalTokens > 0) {
		return resInfo.PromptTokens, resInfo.CompletionTokens, resInfo.TotalTokens, false
	}
	// usage 缺失：拼接请求侧全部文本（system + messages + 工具定义）与
	// 响应侧文本（content + tool_calls），按中英文混合启发式估算。
	var promptText strings.Builder
	if reqInfo != nil {
		promptText.WriteString(reqInfo.System)
		for _, m := range reqInfo.Messages {
			promptText.WriteString(m.Content)
			for _, tc := range m.ToolCalls {
				promptText.WriteString(tc.Name)
				promptText.WriteString(tc.Arguments)
			}
		}
		for _, t := range reqInfo.Tools {
			promptText.WriteString(t.Name)
			promptText.WriteString(t.Description)
			promptText.Write(t.Parameters)
		}
	}
	var responseText strings.Builder
	if resInfo != nil {
		responseText.WriteString(resInfo.Content)
		for _, tc := range resInfo.ToolCalls {
			responseText.WriteString(tc.Name)
			responseText.WriteString(tc.Arguments)
		}
	}
	promptTok := llm.EstimateTokens(promptText.String())
	responseTok := llm.EstimateTokens(responseText.String())
	if promptTok == 0 && responseTok == 0 {
		return 0, 0, 0, false
	}
	return promptTok, responseTok, promptTok + responseTok, true
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
