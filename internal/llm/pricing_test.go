package llm

import (
	"math"
	"testing"
)

func TestEstimateCost_GPT4o(t *testing.T) {
	// gpt-4o: $2.50/1M input, $10.00/1M output
	// 1000 input + 500 output = 1000*2.5/1e6 + 500*10/1e6 = 0.0025 + 0.005 = 0.0075
	cost := EstimateCost("gpt-4o", 1000, 500)
	if math.Abs(cost-0.0075) > 1e-9 {
		t.Errorf("gpt-4o cost = %v, want 0.0075", cost)
	}
}

func TestEstimateCost_GPT4oMini(t *testing.T) {
	// gpt-4o-mini 应优先匹配，而非 gpt-4o
	// $0.15/1M input, $0.60/1M output
	// 1M input + 1M output = 0.15 + 0.60 = 0.75
	cost := EstimateCost("gpt-4o-mini", 1_000_000, 1_000_000)
	if math.Abs(cost-0.75) > 1e-9 {
		t.Errorf("gpt-4o-mini cost = %v, want 0.75", cost)
	}
}

func TestEstimateCost_ClaudeSonnet(t *testing.T) {
	// claude-3-5-sonnet: $3.00/1M input, $15.00/1M output
	// 1000 + 1000 = 0.003 + 0.015 = 0.018
	cost := EstimateCost("claude-3-5-sonnet-20241022", 1000, 1000)
	if math.Abs(cost-0.018) > 1e-9 {
		t.Errorf("claude-3-5-sonnet cost = %v, want 0.018", cost)
	}
}

func TestEstimateCost_GeminiFlash(t *testing.T) {
	// gemini-1.5-flash: $0.075/1M input, $0.30/1M output
	// 1M + 1M = 0.075 + 0.30 = 0.375
	cost := EstimateCost("gemini-1.5-flash", 1_000_000, 1_000_000)
	if math.Abs(cost-0.375) > 1e-9 {
		t.Errorf("gemini-1.5-flash cost = %v, want 0.375", cost)
	}
}

func TestEstimateCost_UnknownModel(t *testing.T) {
	cost := EstimateCost("some-unknown-model", 1000, 1000)
	if cost != 0 {
		t.Errorf("unknown model cost = %v, want 0", cost)
	}
}

func TestEstimateCost_EmptyModel(t *testing.T) {
	cost := EstimateCost("", 1000, 1000)
	if cost != 0 {
		t.Errorf("empty model cost = %v, want 0", cost)
	}
}

func TestLookupPricing_CaseInsensitive(t *testing.T) {
	p := LookupPricing("GPT-4o-MINI")
	if p.InputPerMTokens != 0.15 {
		t.Errorf("GPT-4o-MINI input = %v, want 0.15", p.InputPerMTokens)
	}
}

func TestEstimateCost_ZeroTokens(t *testing.T) {
	cost := EstimateCost("gpt-4o", 0, 0)
	if cost != 0 {
		t.Errorf("zero tokens cost = %v, want 0", cost)
	}
}
