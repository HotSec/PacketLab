package store

import (
	"path/filepath"
	"testing"
)

// TestLLMStatsAggregation 验证聚合查询：总数、token 汇总、成本、按模型/厂商分组。
func TestLLMStatsAggregation(t *testing.T) {
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	// 造数据：两个厂商三个模型（含 1 个未知模型）
	exchanges := []struct {
		id      int64
		llmData string
	}{
		{1, `{"provider":"zhipu","model":"glm-5.2","usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,"cost_usd":0.00036}}`},
		{2, `{"provider":"zhipu","model":"glm-5.2","usage":{"prompt_tokens":200,"completion_tokens":100,"total_tokens":300,"cost_usd":0.00072}}`},
		{3, `{"provider":"deepseek","model":"deepseek-v4-flash","usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500,"cost_usd":0.00028}}`},
		{4, `{"provider":"openai","model":"","usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"cost_usd":0}}`},
		{5, `{"provider":"openai","model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`}, // 无 usage
	}
	for _, ex := range exchanges {
		if _, err := s.db.Exec(
			`INSERT INTO requests (id, method, url, host, path, captured_at, is_llm, llm_data)
			 VALUES (?, 'POST', 'https://x/v1/chat/completions', 'x', '/v1/chat/completions', datetime('now'), 1, ?)`,
			ex.id, ex.llmData); err != nil {
			t.Fatalf("insert exchange %d: %v", ex.id, err)
		}
	}
	// 非 LLM 请求不得计入
	if _, err := s.db.Exec(
		`INSERT INTO requests (id, method, url, host, path, captured_at, is_llm, llm_data)
		 VALUES (99, 'GET', 'https://example.com/', 'example.com', '/', datetime('now'), 0, '')`); err != nil {
		t.Fatalf("insert non-llm: %v", err)
	}

	totals, byModel, byProvider, err := s.LLMStats()
	if err != nil {
		t.Fatalf("LLMStats: %v", err)
	}

	if totals.Exchanges != 5 {
		t.Errorf("totals.Exchanges = %d, want 5", totals.Exchanges)
	}
	// prompt: 100+200+1000+10 = 1310；completion: 50+100+500+5 = 655
	if totals.PromptTokens != 1310 || totals.CompletionTokens != 655 || totals.TotalTokens != 1965 {
		t.Errorf("totals tokens = %+v, want 1310/655/1965", totals)
	}
	// cost: 0.00036+0.00072+0.00028 = 0.00136（gpt-4o 无 usage、unknown model cost 0）
	if totals.CostUSD < 0.00135 || totals.CostUSD > 0.00137 {
		t.Errorf("totals.CostUSD = %.6f, want ~0.00136", totals.CostUSD)
	}

	// by_model：成本降序 → glm-5.2 (0.00108) > deepseek-v4-flash (0.00028) > unknown (0) / gpt-4o (0)
	// 共 4 组：glm-5.2 / deepseek-v4-flash / unknown（空 model 归一）/ gpt-4o
	if len(byModel) != 4 {
		t.Fatalf("byModel len = %d, want 4 (glm-5.2 / deepseek-v4-flash / unknown / gpt-4o)", len(byModel))
	}
	if byModel[0].Model != "glm-5.2" || byModel[0].Exchanges != 2 {
		t.Errorf("byModel[0] = %+v, want glm-5.2 x2", byModel[0])
	}
	if byModel[1].Model != "deepseek-v4-flash" || byModel[1].PromptTokens != 1000 {
		t.Errorf("byModel[1] = %+v, want deepseek-v4-flash prompt 1000", byModel[1])
	}
	// 空 model 归一为 "unknown" 且成本为 0（无定价）
	for _, m := range byModel {
		if m.Model == "unknown" && m.CostUSD != 0 {
			t.Errorf("unknown model cost = %.6f, want 0", m.CostUSD)
		}
	}

	// by_provider：成本降序 → zhipu > deepseek > openai
	if len(byProvider) != 3 {
		t.Fatalf("byProvider len = %d, want 3", len(byProvider))
	}
	if byProvider[0].Provider != "zhipu" || byProvider[0].CostUSD < 0.00107 {
		t.Errorf("byProvider[0] = %+v, want zhipu cost ~0.00108", byProvider[0])
	}
}

// TestListLLMFiltered 过滤列表：provider / model 精确匹配 + 总数正确。
func TestListLLMFiltered(t *testing.T) {
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	for i, llmData := range []string{
		`{"provider":"zhipu","model":"glm-5.2","messages":[{"role":"user","content":"你好"}],"response":"hello"}`,
		`{"provider":"zhipu","model":"glm-5.1","messages":[{"role":"user","content":"second"}],"response":"x"}`,
		`{"provider":"deepseek","model":"deepseek-chat","messages":[{"role":"user","content":"third"}],"response":"y"}`,
	} {
		if _, err := s.db.Exec(
			`INSERT INTO requests (id, method, url, host, path, captured_at, is_llm, llm_data)
			 VALUES (?, 'POST', 'https://x/v1/chat/completions', 'x', '/v1/chat/completions', datetime('now'), 1, ?)`,
			int64(i+1), llmData); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	// provider 过滤
	items, total, err := s.ListLLMFiltered(LLMFilter{Provider: "zhipu"}, 100, 0)
	if err != nil {
		t.Fatalf("ListLLMFiltered(provider): %v", err)
	}
	if total != 2 || len(items) != 2 {
		t.Errorf("provider filter: total=%d len=%d, want 2/2", total, len(items))
	}

	// model 过滤
	items, total, err = s.ListLLMFiltered(LLMFilter{Model: "glm-5.1"}, 100, 0)
	if err != nil {
		t.Fatalf("ListLLMFiltered(model): %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].PromptSnippet != "second" {
		t.Errorf("model filter: total=%d len=%d snippet=%q, want 1/1/second", total, len(items), items[0].PromptSnippet)
	}

	// 无过滤等价于 ListLLM
	_, totalAll, err := s.ListLLMFiltered(LLMFilter{}, 100, 0)
	if err != nil {
		t.Fatalf("ListLLMFiltered(all): %v", err)
	}
	if totalAll != 3 {
		t.Errorf("unfiltered total = %d, want 3", totalAll)
	}
}
