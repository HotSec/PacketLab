package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"packetlab/internal/models"
)

// ========================================
// LLM Exchange
// ========================================

// SetLLMData marks a request as an LLM exchange and stores the parsed LLM data JSON.
func (s *Store) SetLLMData(id int64, llmDataJSON string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("UPDATE requests SET is_llm = 1, llm_data = ? WHERE id = ?",
		truncateStr(llmDataJSON, 4*1024*1024), id)
	return err
}

// LLMExchangeListItem is a lightweight summary for the LLM list view.
type LLMExchangeListItem struct {
	ID         int64  `json:"id"`
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Host       string `json:"host"`
	URL        string `json:"url"`
	IsHTTPS    bool   `json:"is_https"`
	CapturedAt string `json:"captured_at"`
	// Snippet of the last user message
	PromptSnippet string `json:"prompt_snippet"`
	// Snippet of the response
	ResponseSnippet string `json:"response_snippet"`
	// Token usage + cost (parsed from llm_data, zero when absent)
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	Stream           bool    `json:"stream"`
}

// ListLLM returns all LLM exchanges, newest first.
func (s *Store) ListLLM(limit, offset int) ([]LLMExchangeListItem, int, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	var total int
	if err := s.readDB().QueryRow("SELECT COUNT(*) FROM requests WHERE is_llm = 1").Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count llm: %w", err)
	}

	rows, err := s.readDB().Query(
		`SELECT id, host, url, is_https, captured_at, llm_data
		 FROM requests WHERE is_llm = 1
		 ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("query llm: %w", err)
	}
	defer rows.Close()

	var items []LLMExchangeListItem
	for rows.Next() {
		var item LLMExchangeListItem
		var llmDataStr string
		if err := rows.Scan(&item.ID, &item.Host, &item.URL, &item.IsHTTPS, &item.CapturedAt, &llmDataStr); err != nil {
			slog.Warn("scan failed", "error", err)
			continue
		}
		// Parse llm_data for display fields
		if llmDataStr != "" {
			var ex models.LLMExchange
			if err := json.Unmarshal([]byte(llmDataStr), &ex); err == nil {
				item.Provider = ex.Provider
				item.Model = ex.Model
				item.Stream = ex.Stream
				item.PromptSnippet = truncateStr(lastUserMessage(ex.Messages), 200)
				item.ResponseSnippet = truncateStr(ex.Response, 200)
				if ex.Usage != nil {
					item.PromptTokens = ex.Usage.PromptTokens
					item.CompletionTokens = ex.Usage.CompletionTokens
					item.TotalTokens = ex.Usage.TotalTokens
					item.CostUSD = ex.Usage.CostUSD
				}
			}
		}
		items = append(items, item)
	}
	return items, total, nil
}

// GetLLMData retrieves the raw llm_data JSON for a specific request.
func (s *Store) GetLLMData(id int64) (string, error) {
	var llmData string
	err := s.readDB().QueryRow("SELECT llm_data FROM requests WHERE id = ? AND is_llm = 1", id).Scan(&llmData)
	if err != nil {
		return "", err
	}
	return llmData, nil
}

// lastUserMessage returns the content of the last user-role message.
func lastUserMessage(messages []models.LLMMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content
		}
	}
	if len(messages) > 0 {
		return messages[len(messages)-1].Content
	}
	return ""
}

// ========================================
// LLM Statistics (aggregation)
// ========================================

// LLMStats aggregates token usage and cost across all captured LLM exchanges.
// json_extract 由 modernc.org/sqlite 原生支持；字段缺省时按 0 处理。
type LLMStats struct {
	Exchanges        int     `json:"exchanges"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// LLMModelStat is per-model aggregation ordered by cost descending.
type LLMModelStat struct {
	Model            string  `json:"model"`
	Provider         string  `json:"provider"`
	Exchanges        int     `json:"exchanges"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// LLMProviderStat is per-provider aggregation ordered by cost descending.
type LLMProviderStat struct {
	Provider         string  `json:"provider"`
	Exchanges        int     `json:"exchanges"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// LLMStats returns aggregate usage/cost totals and per-model breakdowns.
func (s *Store) LLMStats() (*LLMStats, []LLMModelStat, []LLMProviderStat, error) {
	const usageExpr = `json_extract(llm_data, '$.usage')`

	// 总计：exchanges 数 + token 汇总 + 成本汇总
	var totals LLMStats
	err := s.readDB().QueryRow(fmt.Sprintf(`
		SELECT COUNT(*),
		       COALESCE(SUM(json_extract(%s, '$.prompt_tokens')), 0),
		       COALESCE(SUM(json_extract(%s, '$.completion_tokens')), 0),
		       COALESCE(SUM(json_extract(%s, '$.total_tokens')), 0),
		       COALESCE(SUM(json_extract(%s, '$.cost_usd')), 0)
		FROM requests WHERE is_llm = 1`, usageExpr, usageExpr, usageExpr, usageExpr)).
		Scan(&totals.Exchanges, &totals.PromptTokens, &totals.CompletionTokens, &totals.TotalTokens, &totals.CostUSD)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("llm stats totals: %w", err)
	}

	// 按模型聚合（成本降序）
	modelRows, err := s.readDB().Query(fmt.Sprintf(`
		SELECT COALESCE(NULLIF(json_extract(llm_data, '$.model'), ''), 'unknown') AS model,
		       COALESCE(NULLIF(json_extract(llm_data, '$.provider'), ''), 'unknown') AS provider,
		       COUNT(*),
		       COALESCE(SUM(json_extract(%s, '$.prompt_tokens')), 0),
		       COALESCE(SUM(json_extract(%s, '$.completion_tokens')), 0),
		       COALESCE(SUM(json_extract(%s, '$.total_tokens')), 0),
		       COALESCE(SUM(json_extract(%s, '$.cost_usd')), 0)
		FROM requests WHERE is_llm = 1
		GROUP BY model, provider
		ORDER BY SUM(COALESCE(json_extract(%s, '$.cost_usd'), 0)) DESC, COUNT(*) DESC`,
		usageExpr, usageExpr, usageExpr, usageExpr, usageExpr))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("llm stats by model: %w", err)
	}
	defer modelRows.Close()

	var byModel []LLMModelStat
	for modelRows.Next() {
		var m LLMModelStat
		if err := modelRows.Scan(&m.Model, &m.Provider, &m.Exchanges,
			&m.PromptTokens, &m.CompletionTokens, &m.TotalTokens, &m.CostUSD); err != nil {
			slog.Warn("scan llm model stat failed", "error", err)
			continue
		}
		byModel = append(byModel, m)
	}

	// 按厂商聚合（成本降序）
	providerRows, err := s.readDB().Query(fmt.Sprintf(`
		SELECT COALESCE(NULLIF(json_extract(llm_data, '$.provider'), ''), 'unknown') AS provider,
		       COUNT(*),
		       COALESCE(SUM(json_extract(%s, '$.prompt_tokens')), 0),
		       COALESCE(SUM(json_extract(%s, '$.completion_tokens')), 0),
		       COALESCE(SUM(json_extract(%s, '$.total_tokens')), 0),
		       COALESCE(SUM(json_extract(%s, '$.cost_usd')), 0)
		FROM requests WHERE is_llm = 1
		GROUP BY provider
		ORDER BY SUM(COALESCE(json_extract(%s, '$.cost_usd'), 0)) DESC, COUNT(*) DESC`,
		usageExpr, usageExpr, usageExpr, usageExpr, usageExpr))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("llm stats by provider: %w", err)
	}
	defer providerRows.Close()

	var byProvider []LLMProviderStat
	for providerRows.Next() {
		var p LLMProviderStat
		if err := providerRows.Scan(&p.Provider, &p.Exchanges,
			&p.PromptTokens, &p.CompletionTokens, &p.TotalTokens, &p.CostUSD); err != nil {
			slog.Warn("scan llm provider stat failed", "error", err)
			continue
		}
		byProvider = append(byProvider, p)
	}

	if byModel == nil {
		byModel = []LLMModelStat{}
	}
	if byProvider == nil {
		byProvider = []LLMProviderStat{}
	}
	return &totals, byModel, byProvider, nil
}

// LLMFilter 列表过滤条件。零值字段不参与过滤。
type LLMFilter struct {
	Provider string // 精确匹配 provider 字段
	Model    string // 精确匹配 model 字段
}

// ListLLMFiltered 支持 provider/model 精确过滤的 LLM 列表（含总数）。
func (s *Store) ListLLMFiltered(f LLMFilter, limit, offset int) ([]LLMExchangeListItem, int, error) {
	where := " WHERE is_llm = 1"
	var args []any
	if f.Provider != "" {
		where += " AND json_extract(llm_data, '$.provider') = ?"
		args = append(args, f.Provider)
	}
	if f.Model != "" {
		where += " AND json_extract(llm_data, '$.model') = ?"
		args = append(args, f.Model)
	}

	var total int
	if err := s.readDB().QueryRow("SELECT COUNT(*) FROM requests"+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count llm: %w", err)
	}

	queryArgs := append(append([]any{}, args...), limit, offset)
	rows, err := s.readDB().Query(
		`SELECT id, host, url, is_https, captured_at, llm_data
		 FROM requests`+where+` ORDER BY id DESC LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("query llm: %w", err)
	}
	defer rows.Close()

	var items []LLMExchangeListItem
	for rows.Next() {
		var item LLMExchangeListItem
		var llmDataStr string
		if err := rows.Scan(&item.ID, &item.Host, &item.URL, &item.IsHTTPS, &item.CapturedAt, &llmDataStr); err != nil {
			slog.Warn("scan failed", "error", err)
			continue
		}
		if llmDataStr != "" {
			var ex models.LLMExchange
			if err := json.Unmarshal([]byte(llmDataStr), &ex); err == nil {
				item.Provider = ex.Provider
				item.Model = ex.Model
				item.Stream = ex.Stream
				item.PromptSnippet = truncateStr(lastUserMessage(ex.Messages), 200)
				item.ResponseSnippet = truncateStr(ex.Response, 200)
				if ex.Usage != nil {
					item.PromptTokens = ex.Usage.PromptTokens
					item.CompletionTokens = ex.Usage.CompletionTokens
					item.TotalTokens = ex.Usage.TotalTokens
					item.CostUSD = ex.Usage.CostUSD
				}
			}
		}
		items = append(items, item)
	}
	return items, total, nil
}

// CountLLM 返回 LLM 交换总数（用于「AI 对话」入口角标）。
func (s *Store) CountLLM() (int, error) {
	var n int
	err := s.readDB().QueryRow("SELECT COUNT(*) FROM requests WHERE is_llm = 1").Scan(&n)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return n, nil
}
