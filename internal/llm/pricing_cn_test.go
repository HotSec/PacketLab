package llm

import "testing"

// TestPricingChineseModels 国内模型定价表：覆盖 + 边界匹配。
func TestPricingChineseModels(t *testing.T) {
	tests := []struct {
		model         string
		wantInput     float64
		wantOutput    float64
		wantCacheRead float64
	}{
		// 智谱 GLM（models.dev 官方 API 价 2026-08 同步）
		{"glm-5.2", 1.40, 4.40, 0.26},
		{"glm-5.1", 1.40, 4.40, 0.26},
		{"GLM-5.2", 1.40, 4.40, 0.26}, // 大小写不敏感
		// Kimi
		{"kimi-k3", 3.00, 15.00, 0.30},
		{"kimi-k2.7-code", 0.95, 4.00, 0.19},
		// MiniMax
		{"MiniMax-M2.7", 0.30, 1.20, 0.06},
		{"minimax-m3", 0.30, 1.20, 0.06},
		// Qwen
		{"qwen3.7-max", 2.50, 7.50, 0.50},
		{"qwen3.7-plus", 0.50, 3.00, 0.05},
		// DeepSeek（官方 API 价：chat/reasoner 已与 v4-flash 同价）
		{"deepseek-v4-flash", 0.14, 0.28, 0.0028},
		{"deepseek-v4-pro", 0.435, 0.87, 0.003625},
		{"deepseek-chat", 0.14, 0.28, 0.0028},
		{"deepseek-reasoner", 0.14, 0.28, 0.0028},
		// xAI
		{"grok-4.5", 2.00, 6.00, 0.30},
		// 其他
		{"hy3", 0.14, 0.58, 0.035},
	}
	for _, tt := range tests {
		p := LookupPricing(tt.model)
		if p.InputPerMTokens != tt.wantInput || p.OutputPerMTokens != tt.wantOutput {
			t.Errorf("LookupPricing(%q) = %+v, want input %.4g output %.4g",
				tt.model, p, tt.wantInput, tt.wantOutput)
		}
		if p.CacheReadPerMTokens != tt.wantCacheRead {
			t.Errorf("LookupPricing(%q).CacheRead = %.6g, want %.6g",
				tt.model, p.CacheReadPerMTokens, tt.wantCacheRead)
		}
	}
}

// TestPricingBoundary 边界匹配：前缀不跨 '-' 边界。
func TestPricingBoundary(t *testing.T) {
	// "glm-5" 不应匹配 "glm-5.2"（无此 key，但验证边界机制用 gpt 系列更直观）
	if p := LookupPricing("gpt-4o-mini"); p.InputPerMTokens != 0.15 {
		t.Errorf("gpt-4o-mini should match itself, got %+v", p)
	}
	// "gpt-4" 不得匹配 "gpt-4o"
	if p := LookupPricing("gpt-4o"); p.InputPerMTokens == 30.00 {
		t.Errorf("gpt-4o wrongly matched gpt-4 pricing: %+v", p)
	}
	// 未覆盖模型 → 零值
	if p := LookupPricing("some-unknown-model"); p.InputPerMTokens != 0 || p.OutputPerMTokens != 0 {
		t.Errorf("unknown model should be zero-valued, got %+v", p)
	}
}

// TestEstimateCostGLM 端到端成本估算（用户主力模型 GLM-5.2）。
func TestEstimateCostGLM(t *testing.T) {
	// 1M input + 1M output → $1.40 + $4.40 = $5.80
	cost := EstimateCost("glm-5.2", 1_000_000, 1_000_000)
	if cost < 5.79 || cost > 5.81 {
		t.Errorf("EstimateCost(glm-5.2, 1M, 1M) = %.6f, want ~5.80", cost)
	}
	// 未知模型 → 0
	if cost := EstimateCost("unknown-model", 1000, 1000); cost != 0 {
		t.Errorf("unknown model cost = %.6f, want 0", cost)
	}
	// 空模型 → 0
	if cost := EstimateCost("", 1000, 1000); cost != 0 {
		t.Errorf("empty model cost = %.6f, want 0", cost)
	}
}
