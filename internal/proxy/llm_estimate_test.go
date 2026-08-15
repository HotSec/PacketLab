package proxy

import (
	"testing"

	"packetlab/internal/llm"
)

// TestUsageWithFallback 真实 usage 优先，缺失时估算兜底。
func TestUsageWithFallback(t *testing.T) {
	reqInfo := &llm.RequestInfo{
		Model:    "glm-5.2",
		System:   "You are helpful.",
		Messages: []llm.Message{{Role: "user", Content: "你好，请帮我写代码"}},
	}

	t.Run("real usage wins", func(t *testing.T) {
		res := &llm.ResponseInfo{
			Content:          "好的",
			PromptTokens:     100,
			CompletionTokens: 50,
			TotalTokens:      150,
		}
		p, c, total, est := usageWithFallback(reqInfo, res)
		if p != 100 || c != 50 || total != 150 || est {
			t.Errorf("got (%d,%d,%d,%v), want (100,50,150,false)", p, c, total, est)
		}
	})

	t.Run("estimate fallback", func(t *testing.T) {
		res := &llm.ResponseInfo{Content: "好的，代码如下"} // 无 usage
		p, c, total, est := usageWithFallback(reqInfo, res)
		if !est {
			t.Error("expected estimated=true when usage missing")
		}
		if p <= 0 || c <= 0 || total != p+c {
			t.Errorf("got (%d,%d,%d), want positive estimates summing correctly", p, c, total)
		}
	})

	t.Run("both empty", func(t *testing.T) {
		p, c, total, est := usageWithFallback(nil, &llm.ResponseInfo{})
		if p != 0 || c != 0 || total != 0 || est {
			t.Errorf("got (%d,%d,%d,%v), want all zero, estimated=false", p, c, total, est)
		}
	})
}
