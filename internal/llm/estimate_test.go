package llm

import "testing"

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		exact int // 期望精确值（启发式是确定性的）
	}{
		{"empty", "", 0},
		{"ascii", "Hello world", 3}, // 11 字符 / 4 上取整 = 3
		{"ascii long", "This is a longer English sentence.", 9}, // 34 / 4 上取整 = 9
		{"cjk", "你好世界", 3},                                      // 4 汉字 / 1.5 上取整 = 3
		{"cjk long", "这是一个很长的中文句子用来测试估算的准确性", 14},               // 21 汉字 / 1.5 = 14
		{"mixed", "Hello你好", 3},                                 // (5/4)+(2/1.5)=2.58 上取整 = 3
		{"json", `{"name":"x","args":{"a":1}}`, 7},              // 27 ASCII / 4 上取整 = 7
		{"single char", "a", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EstimateTokens(tt.text)
			if got != tt.exact {
				t.Errorf("EstimateTokens(%q) = %d, want %d", tt.text, got, tt.exact)
			}
		})
	}
}

// TestEstimateTokensUTF8Invalid 非法 UTF-8 按字节粗估，不 panic。
func TestEstimateTokensUTF8Invalid(t *testing.T) {
	if got := EstimateTokensUTF8([]byte{0xff, 0xfe, 0xfd}); got != 1 {
		t.Errorf("invalid utf8 (3 bytes) = %d, want 1", got)
	}
	if got := EstimateTokensUTF8([]byte("hello")); got != 2 {
		t.Errorf("hello = %d, want 2 (5 bytes/4 ceil)", got)
	}
	if got := EstimateTokensUTF8(nil); got != 0 {
		t.Errorf("nil = %d, want 0", got)
	}
}

func TestHasCJK(t *testing.T) {
	if !HasCJK("你好") {
		t.Error("HasCJK(你好) = false, want true")
	}
	if HasCJK("hello world") {
		t.Error("HasCJK(hello) = true, want false")
	}
}

// TestEstimateTokensRatio 启发式比率 sanity check：常见中英文比例的估值落在合理区间。
func TestEstimateTokensRatio(t *testing.T) {
	// 1000 汉字 → ~667 tokens；tiktoken 实际约 500-800，区间内即可
	zh := make([]rune, 1000)
	for i := range zh {
		zh[i] = '中'
	}
	got := EstimateTokens(string(zh))
	if got < 500 || got > 800 {
		t.Errorf("1000 CJK chars = %d tokens, want in [500,800]", got)
	}
	// 4000 ASCII → ~1000 tokens
	en := make([]byte, 4000)
	for i := range en {
		en[i] = 'a'
	}
	if got := EstimateTokens(string(en)); got != 1000 {
		t.Errorf("4000 ASCII chars = %d, want 1000", got)
	}
}
