package llm

// pricing.go — LLM 模型成本估算（内置静态定价表，单位 USD per 1M tokens）
// 定价数据来源：OpenAI/Anthropic/Google 官方定价页面（2026-07-17 快照）。
// 未覆盖的模型返回 0 成本。

// ModelPricing 模型定价（USD per 1M tokens）
type ModelPricing struct {
	InputPerMTokens  float64 // 输入 token 单价
	OutputPerMTokens float64 // 输出 token 单价
}

// pricingTable 内置定价表。key 是模型名前缀（如 "gpt-4o" 匹配 "gpt-4o-2024-..."）。
// 查找时按 key 长度降序，优先匹配更具体的前缀。
var pricingTable = map[string]ModelPricing{
	// OpenAI
	"gpt-4o":            {InputPerMTokens: 2.50, OutputPerMTokens: 10.00},
	"gpt-4o-mini":       {InputPerMTokens: 0.15, OutputPerMTokens: 0.60},
	"gpt-4-turbo":       {InputPerMTokens: 10.00, OutputPerMTokens: 30.00},
	"gpt-4":             {InputPerMTokens: 30.00, OutputPerMTokens: 60.00},
	"gpt-3.5-turbo":     {InputPerMTokens: 0.50, OutputPerMTokens: 1.50},
	"o1":                {InputPerMTokens: 15.00, OutputPerMTokens: 60.00},
	"o1-mini":           {InputPerMTokens: 3.00, OutputPerMTokens: 12.00},
	"o1-preview":        {InputPerMTokens: 15.00, OutputPerMTokens: 60.00},
	"o3-mini":           {InputPerMTokens: 3.00, OutputPerMTokens: 12.00},
	// Anthropic
	"claude-3-5-sonnet": {InputPerMTokens: 3.00, OutputPerMTokens: 15.00},
	"claude-3-5-haiku":  {InputPerMTokens: 0.80, OutputPerMTokens: 4.00},
	"claude-3-opus":     {InputPerMTokens: 15.00, OutputPerMTokens: 75.00},
	"claude-3-sonnet":   {InputPerMTokens: 3.00, OutputPerMTokens: 15.00},
	"claude-3-haiku":    {InputPerMTokens: 0.25, OutputPerMTokens: 1.25},
	// Gemini
	"gemini-1.5-pro":    {InputPerMTokens: 1.25, OutputPerMTokens: 5.00},
	"gemini-1.5-flash":  {InputPerMTokens: 0.075, OutputPerMTokens: 0.30},
	"gemini-2.0-flash":  {InputPerMTokens: 0.10, OutputPerMTokens: 0.40},
	"gemini-2.0-pro":    {InputPerMTokens: 1.25, OutputPerMTokens: 10.00},
}

// EstimateCost 根据模型名、输入/输出 token 数估算成本（USD）。
// 模型未在定价表中时返回 0。
// EstimateCost estimates cost (USD) by model name and token usage.
// Returns 0 for unknown models.
func EstimateCost(model string, promptTokens, completionTokens int) float64 {
	if model == "" {
		return 0
	}
	p := LookupPricing(model)
	if p.InputPerMTokens == 0 && p.OutputPerMTokens == 0 {
		return 0
	}
	return float64(promptTokens)*p.InputPerMTokens/1_000_000 +
		float64(completionTokens)*p.OutputPerMTokens/1_000_000
}

// LookupPricing 查找模型定价。按 key 长度降序匹配最具体的前缀。
// LookupPricing looks up model pricing. Matches the longest prefix.
func LookupPricing(model string) ModelPricing {
	// 大小写不敏感
	m := toLowerASCII(model)
	var best ModelPricing
	var bestKeyLen int
	for key, p := range pricingTable {
		k := toLowerASCII(key)
		if hasPrefix(m, k) && len(k) > bestKeyLen {
			best = p
			bestKeyLen = len(k)
		}
	}
	return best
}
