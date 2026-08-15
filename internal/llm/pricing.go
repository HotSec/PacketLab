package llm

// pricing.go — LLM 模型成本估算（内置静态定价表，单位 USD per 1M tokens）
//
// 定价数据来源：
//   - OpenAI/Anthropic/Google 官方定价页面（2026-07 快照）
//   - OpenCode Go 套餐定价页（2026-08-01 快照，见技能 llm-cost-analysis 的
//     references/opencode-go-pricing.md）：GLM / Kimi / MiMo / MiniMax /
//     Qwen / DeepSeek V4 系列 / Grok
//   - DeepSeek 官方 API 文档（2026-07-31，V4 Flash-0731 规格）
//
// 未覆盖的模型返回 0 成本。定价表是纯数据，新增模型只需加一行。

// ModelPricing 模型定价（USD per 1M tokens）
type ModelPricing struct {
	InputPerMTokens  float64 // 输入 token 单价（缓存未命中价）
	OutputPerMTokens float64 // 输出 token 单价
	// CacheReadPerMTokens 缓存读取单价（部分厂商公布；0 = 未公布）。
	// 注意：PacketLab 只从 usage 拿到 prompt/completion 两级 token 数，
	// 无法区分缓存命中，成本估算统一用未命中价（保守高估）。
	CacheReadPerMTokens float64
}

// pricingTable 内置定价表。key 是模型名前缀（如 "gpt-4o" 匹配 "gpt-4o-2024-..."）。
// 查找时按 key 长度降序，优先匹配更具体的前缀；前缀必须落在模型名边界上
// （key 之后是 '-' 或结束），避免 "gpt-4" 误匹配 "gpt-4o"。
var pricingTable = map[string]ModelPricing{
	// ── OpenAI ────────────────────────────────────────────────
	"gpt-4o":        {InputPerMTokens: 2.50, OutputPerMTokens: 10.00},
	"gpt-4o-mini":   {InputPerMTokens: 0.15, OutputPerMTokens: 0.60},
	"gpt-4-turbo":   {InputPerMTokens: 10.00, OutputPerMTokens: 30.00},
	"gpt-4":         {InputPerMTokens: 30.00, OutputPerMTokens: 60.00},
	"gpt-3.5-turbo": {InputPerMTokens: 0.50, OutputPerMTokens: 1.50},
	"o1":            {InputPerMTokens: 15.00, OutputPerMTokens: 60.00},
	"o1-mini":       {InputPerMTokens: 3.00, OutputPerMTokens: 12.00},
	"o1-preview":    {InputPerMTokens: 15.00, OutputPerMTokens: 60.00},
	"o3-mini":       {InputPerMTokens: 3.00, OutputPerMTokens: 12.00},

	// ── Anthropic ─────────────────────────────────────────────
	"claude-3-5-sonnet": {InputPerMTokens: 3.00, OutputPerMTokens: 15.00},
	"claude-3-5-haiku":  {InputPerMTokens: 0.80, OutputPerMTokens: 4.00},
	"claude-3-opus":     {InputPerMTokens: 15.00, OutputPerMTokens: 75.00},
	"claude-3-sonnet":   {InputPerMTokens: 3.00, OutputPerMTokens: 15.00},
	"claude-3-haiku":    {InputPerMTokens: 0.25, OutputPerMTokens: 1.25},

	// ── Google Gemini ─────────────────────────────────────────
	"gemini-1.5-pro":        {InputPerMTokens: 1.25, OutputPerMTokens: 5.00},
	"gemini-1.5-flash":      {InputPerMTokens: 0.075, OutputPerMTokens: 0.30},
	"gemini-2.0-flash":      {InputPerMTokens: 0.10, OutputPerMTokens: 0.40},
	"gemini-2.0-pro":        {InputPerMTokens: 1.25, OutputPerMTokens: 10.00},
	"gemini-2.5-flash":      {InputPerMTokens: 0.30, OutputPerMTokens: 2.50},
	"gemini-2.5-pro":        {InputPerMTokens: 1.25, OutputPerMTokens: 10.00},
	"gemini-2.5-flash-lite": {InputPerMTokens: 0.10, OutputPerMTokens: 0.40},

	// ── 智谱 GLM ──────────────────────────────────────────────
	// 来源：OpenCode Go 套餐定价（2026-08-01）
	"glm-5.2": {InputPerMTokens: 1.40, OutputPerMTokens: 4.40, CacheReadPerMTokens: 0.26},
	"glm-5.1": {InputPerMTokens: 1.40, OutputPerMTokens: 4.40, CacheReadPerMTokens: 0.26},

	// ── Moonshot Kimi ─────────────────────────────────────────
	"kimi-k3":       {InputPerMTokens: 3.00, OutputPerMTokens: 15.00, CacheReadPerMTokens: 0.30},
	"kimi-k2.7":     {InputPerMTokens: 0.95, OutputPerMTokens: 4.00, CacheReadPerMTokens: 0.19},
	"kimi-k2.6":     {InputPerMTokens: 0.95, OutputPerMTokens: 4.00, CacheReadPerMTokens: 0.16},

	// ── MiniMax（MiMo / MiniMax-M 系列）───────────────────────
	"minimax-mimo-v2.5-pro": {InputPerMTokens: 0.435, OutputPerMTokens: 0.87, CacheReadPerMTokens: 0.003625},
	"minimax-mimo-v2.5":     {InputPerMTokens: 0.14, OutputPerMTokens: 0.28, CacheReadPerMTokens: 0.0028},
	"minimax-m3":            {InputPerMTokens: 0.30, OutputPerMTokens: 1.20, CacheReadPerMTokens: 0.06},
	"minimax-m2.7":          {InputPerMTokens: 0.30, OutputPerMTokens: 1.20, CacheReadPerMTokens: 0.06},
	"mimo":                  {InputPerMTokens: 0.14, OutputPerMTokens: 0.28, CacheReadPerMTokens: 0.0028},

	// ── 阿里云 Qwen ───────────────────────────────────────────
	"qwen3.7-max":      {InputPerMTokens: 2.50, OutputPerMTokens: 7.50, CacheReadPerMTokens: 0.50},
	"qwen3.7-plus":     {InputPerMTokens: 0.40, OutputPerMTokens: 1.60, CacheReadPerMTokens: 0.04},
	"qwen3.6-plus":     {InputPerMTokens: 0.50, OutputPerMTokens: 3.00, CacheReadPerMTokens: 0.05},
	"qwen3-max":        {InputPerMTokens: 2.50, OutputPerMTokens: 7.50, CacheReadPerMTokens: 0.50},
	"qwen3-plus":       {InputPerMTokens: 0.40, OutputPerMTokens: 1.60, CacheReadPerMTokens: 0.04},

	// ── DeepSeek ──────────────────────────────────────────────
	// V4 系列来源：OpenCode Go 定价（2026-08-01）；V3 系列为国内 API 官方价。
	"deepseek-v4-pro":    {InputPerMTokens: 0.435, OutputPerMTokens: 0.87, CacheReadPerMTokens: 0.003625},
	"deepseek-v4-flash":  {InputPerMTokens: 0.14, OutputPerMTokens: 0.28, CacheReadPerMTokens: 0.0028},
	"deepseek-v3.2":      {InputPerMTokens: 0.28, OutputPerMTokens: 0.42, CacheReadPerMTokens: 0.028},
	"deepseek-v3.1":      {InputPerMTokens: 0.28, OutputPerMTokens: 0.42, CacheReadPerMTokens: 0.028},
	"deepseek-r1":        {InputPerMTokens: 0.55, OutputPerMTokens: 2.19, CacheReadPerMTokens: 0.14},
	"deepseek-chat":      {InputPerMTokens: 0.28, OutputPerMTokens: 0.42, CacheReadPerMTokens: 0.028},
	"deepseek-reasoner":  {InputPerMTokens: 0.55, OutputPerMTokens: 2.19, CacheReadPerMTokens: 0.14},

	// ── xAI Grok ──────────────────────────────────────────────
	"grok-4.5": {InputPerMTokens: 2.00, OutputPerMTokens: 6.00, CacheReadPerMTokens: 0.30},
	"grok-4":   {InputPerMTokens: 3.00, OutputPerMTokens: 15.00},
	"grok-3":   {InputPerMTokens: 3.00, OutputPerMTokens: 15.00},

	// ── 其他 OpenAI 兼容常见模型（Hy3，OpenCode Go 在售）────────
	"hy3": {InputPerMTokens: 0.14, OutputPerMTokens: 0.58, CacheReadPerMTokens: 0.035},
}

// EstimateCost 根据模型名、输入/输出 token 数估算成本（USD）。
// 模型未在定价表中时返回 0。
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
// 前缀必须落在边界上（key 之后的字符是 '-' 或字符串结束），否则视为不同模型：
// 例如 "gpt-4" 不能匹配 "gpt-4.1" / "gpt-4o"，未覆盖的模型返回零值定价（成本 0）。
func LookupPricing(model string) ModelPricing {
	// 大小写不敏感
	m := toLowerASCII(model)
	var best ModelPricing
	var bestKeyLen int
	for key, p := range pricingTable {
		k := toLowerASCII(key)
		if len(k) <= bestKeyLen {
			continue
		}
		// 边界匹配：key 之后的字符必须是 '-' 或结束
		if !hasPrefix(m, k) {
			continue
		}
		if len(m) > len(k) && m[len(k)] != '-' {
			continue
		}
		best = p
		bestKeyLen = len(k)
	}
	return best
}
