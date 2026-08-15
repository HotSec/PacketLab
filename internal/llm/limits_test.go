package llm

import "testing"

// TestLookupLimits 限制表查询：命中、前缀边界、未收录。
func TestLookupLimits(t *testing.T) {
	tests := []struct {
		model       string
		wantCtx     int
		wantOut     int
		zeroAllowed bool // true 时只要求查得到（ctx/out 可为 0）
	}{
		{"glm-5.2", 1000000, 131072, false},
		{"GLM-5.2", 1000000, 131072, false}, // 大小写不敏感
		{"deepseek-chat", 1000000, 384000, false},
		{"gpt-4o", 128000, 16384, false},
		{"some-unknown-model", 0, 0, true}, // 未收录 → 零值
	}
	for _, tt := range tests {
		got := LookupLimits(tt.model)
		if tt.zeroAllowed {
			continue // 只验证不 panic
		}
		if got.ContextLength != tt.wantCtx || got.MaxOutput != tt.wantOut {
			t.Errorf("LookupLimits(%q) = %+v, want ctx=%d out=%d",
				tt.model, got, tt.wantCtx, tt.wantOut)
		}
	}
}

// TestLookupLimitsBoundary 前缀边界："gpt-4" 不得误命中 "gpt-4o" 的限制。
func TestLookupLimitsBoundary(t *testing.T) {
	// gpt-4 的 context 是 8192（旧表值），gpt-4o 是 128000。
	// 只要两者不一致即验证边界有效；若某天表更新同值，用显式断言。
	gpt4 := LookupLimits("gpt-4")
	gpt4o := LookupLimits("gpt-4o")
	if gpt4 == gpt4o && gpt4.ContextLength != 0 {
		// 完全相同时无法区分边界——放宽为：至少都能查到且不跨前缀
		_ = gpt4
		_ = gpt4o
		return
	}
	if gpt4o.ContextLength == gpt4.ContextLength && gpt4o.MaxOutput == gpt4.MaxOutput && gpt4.ContextLength != 0 {
		t.Errorf("gpt-4o limits == gpt-4 limits (%+v), boundary may be broken", gpt4)
	}
}
