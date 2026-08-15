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
	"claude-3-5-haiku":             {InputPerMTokens: 0.8, OutputPerMTokens: 4},
	"claude-3-5-sonnet":            {InputPerMTokens: 3, OutputPerMTokens: 15},
	"claude-3-haiku":               {InputPerMTokens: 0.25, OutputPerMTokens: 1.25},
	"claude-3-opus":                {InputPerMTokens: 15, OutputPerMTokens: 75},
	"claude-3-sonnet":              {InputPerMTokens: 3, OutputPerMTokens: 15},
	"claude-fable-5":               {InputPerMTokens: 10, OutputPerMTokens: 50, CacheReadPerMTokens: 1},
	"claude-haiku-4-5":             {InputPerMTokens: 1, OutputPerMTokens: 5, CacheReadPerMTokens: 0.1},
	"claude-haiku-4-5-20251001":    {InputPerMTokens: 1, OutputPerMTokens: 5, CacheReadPerMTokens: 0.1},
	"claude-opus-4-5":              {InputPerMTokens: 5, OutputPerMTokens: 25, CacheReadPerMTokens: 0.5},
	"claude-opus-4-5-20251101":     {InputPerMTokens: 5, OutputPerMTokens: 25, CacheReadPerMTokens: 0.5},
	"claude-opus-4-6":              {InputPerMTokens: 5, OutputPerMTokens: 25, CacheReadPerMTokens: 0.5},
	"claude-opus-4-7":              {InputPerMTokens: 5, OutputPerMTokens: 25, CacheReadPerMTokens: 0.5},
	"claude-opus-4-8":              {InputPerMTokens: 5, OutputPerMTokens: 25, CacheReadPerMTokens: 0.5},
	"claude-opus-5":                {InputPerMTokens: 5, OutputPerMTokens: 25, CacheReadPerMTokens: 0.5},
	"claude-sonnet-4-5":            {InputPerMTokens: 3, OutputPerMTokens: 15, CacheReadPerMTokens: 0.3},
	"claude-sonnet-4-5-20250929":   {InputPerMTokens: 3, OutputPerMTokens: 15, CacheReadPerMTokens: 0.3},
	"claude-sonnet-4-6":            {InputPerMTokens: 3, OutputPerMTokens: 15, CacheReadPerMTokens: 0.3},
	"claude-sonnet-5":              {InputPerMTokens: 2, OutputPerMTokens: 10, CacheReadPerMTokens: 0.2},
	"deepseek-chat":                {InputPerMTokens: 0.14, OutputPerMTokens: 0.28, CacheReadPerMTokens: 0.0028},
	"deepseek-r1":                  {InputPerMTokens: 0.55, OutputPerMTokens: 2.19, CacheReadPerMTokens: 0.14},
	"deepseek-reasoner":            {InputPerMTokens: 0.14, OutputPerMTokens: 0.28, CacheReadPerMTokens: 0.0028},
	"deepseek-v3.1":                {InputPerMTokens: 0.28, OutputPerMTokens: 0.42, CacheReadPerMTokens: 0.028},
	"deepseek-v3.2":                {InputPerMTokens: 0.28, OutputPerMTokens: 0.42, CacheReadPerMTokens: 0.028},
	"deepseek-v4-flash":            {InputPerMTokens: 0.14, OutputPerMTokens: 0.28, CacheReadPerMTokens: 0.0028},
	"deepseek-v4-pro":              {InputPerMTokens: 0.435, OutputPerMTokens: 0.87, CacheReadPerMTokens: 0.003625},
	"gemini-1.5-flash":             {InputPerMTokens: 0.075, OutputPerMTokens: 0.3},
	"gemini-1.5-pro":               {InputPerMTokens: 1.25, OutputPerMTokens: 5},
	"gemini-2.0-flash":             {InputPerMTokens: 0.1, OutputPerMTokens: 0.4},
	"gemini-2.0-pro":               {InputPerMTokens: 1.25, OutputPerMTokens: 10},
	"gemini-2.5-flash":             {InputPerMTokens: 0.3, OutputPerMTokens: 2.5, CacheReadPerMTokens: 0.03},
	"gemini-2.5-flash-lite":        {InputPerMTokens: 0.1, OutputPerMTokens: 0.4, CacheReadPerMTokens: 0.01},
	"gemini-2.5-pro":               {InputPerMTokens: 1.25, OutputPerMTokens: 10, CacheReadPerMTokens: 0.125},
	"gemini-3-flash-preview":       {InputPerMTokens: 0.5, OutputPerMTokens: 3, CacheReadPerMTokens: 0.05},
	"gemini-3.1-flash-lite":        {InputPerMTokens: 0.25, OutputPerMTokens: 1.5, CacheReadPerMTokens: 0.025},
	"gemini-3.1-pro-preview":       {InputPerMTokens: 2, OutputPerMTokens: 12, CacheReadPerMTokens: 0.2},
	"gemini-3.5-flash":             {InputPerMTokens: 1.5, OutputPerMTokens: 9, CacheReadPerMTokens: 0.15},
	"gemini-3.5-flash-lite":        {InputPerMTokens: 0.3, OutputPerMTokens: 2.5, CacheReadPerMTokens: 0.03},
	"gemini-3.6-flash":             {InputPerMTokens: 1.5, OutputPerMTokens: 7.5, CacheReadPerMTokens: 0.15},
	"gemini-3.7-flash":             {InputPerMTokens: 0.75, OutputPerMTokens: 3.75, CacheReadPerMTokens: 0.075},
	"glm-4.5":                      {InputPerMTokens: 0.6, OutputPerMTokens: 2.2, CacheReadPerMTokens: 0.11},
	"glm-4.5-air":                  {InputPerMTokens: 0.2, OutputPerMTokens: 1.1, CacheReadPerMTokens: 0.03},
	"glm-4.5v":                     {InputPerMTokens: 0.6, OutputPerMTokens: 1.8},
	"glm-4.6":                      {InputPerMTokens: 0.6, OutputPerMTokens: 2.2, CacheReadPerMTokens: 0.11},
	"glm-4.6v":                     {InputPerMTokens: 0.3, OutputPerMTokens: 0.9},
	"glm-4.7":                      {InputPerMTokens: 0.6, OutputPerMTokens: 2.2, CacheReadPerMTokens: 0.11},
	"glm-4.7-flashx":               {InputPerMTokens: 0.07, OutputPerMTokens: 0.4, CacheReadPerMTokens: 0.01},
	"glm-5":                        {InputPerMTokens: 1, OutputPerMTokens: 3.2, CacheReadPerMTokens: 0.2},
	"glm-5.1":                      {InputPerMTokens: 1.4, OutputPerMTokens: 4.4, CacheReadPerMTokens: 0.26},
	"glm-5.2":                      {InputPerMTokens: 1.4, OutputPerMTokens: 4.4, CacheReadPerMTokens: 0.26},
	"glm-5v-turbo":                 {InputPerMTokens: 5, OutputPerMTokens: 22, CacheReadPerMTokens: 1.2},
	"gpt-3.5-turbo":                {InputPerMTokens: 0.5, OutputPerMTokens: 1.5},
	"gpt-4":                        {InputPerMTokens: 30, OutputPerMTokens: 60},
	"gpt-4-turbo":                  {InputPerMTokens: 10, OutputPerMTokens: 30},
	"gpt-4.1":                      {InputPerMTokens: 2, OutputPerMTokens: 8, CacheReadPerMTokens: 0.5},
	"gpt-4.1-mini":                 {InputPerMTokens: 0.4, OutputPerMTokens: 1.6, CacheReadPerMTokens: 0.1},
	"gpt-4.1-nano":                 {InputPerMTokens: 0.1, OutputPerMTokens: 0.4, CacheReadPerMTokens: 0.025},
	"gpt-4o":                       {InputPerMTokens: 2.5, OutputPerMTokens: 10, CacheReadPerMTokens: 1.25},
	"gpt-4o-mini":                  {InputPerMTokens: 0.15, OutputPerMTokens: 0.6, CacheReadPerMTokens: 0.075},
	"gpt-5":                        {InputPerMTokens: 1.25, OutputPerMTokens: 10, CacheReadPerMTokens: 0.125},
	"gpt-5-mini":                   {InputPerMTokens: 0.25, OutputPerMTokens: 2, CacheReadPerMTokens: 0.025},
	"gpt-5-nano":                   {InputPerMTokens: 0.05, OutputPerMTokens: 0.4, CacheReadPerMTokens: 0.005},
	"gpt-5-pro":                    {InputPerMTokens: 15, OutputPerMTokens: 120},
	"gpt-5.1":                      {InputPerMTokens: 1.25, OutputPerMTokens: 10, CacheReadPerMTokens: 0.125},
	"gpt-5.2":                      {InputPerMTokens: 1.75, OutputPerMTokens: 14, CacheReadPerMTokens: 0.175},
	"gpt-5.2-pro":                  {InputPerMTokens: 21, OutputPerMTokens: 168},
	"gpt-5.3-codex":                {InputPerMTokens: 1.75, OutputPerMTokens: 14, CacheReadPerMTokens: 0.175},
	"gpt-5.4":                      {InputPerMTokens: 2.5, OutputPerMTokens: 15, CacheReadPerMTokens: 0.25},
	"gpt-5.4-mini":                 {InputPerMTokens: 0.75, OutputPerMTokens: 4.5, CacheReadPerMTokens: 0.075},
	"gpt-5.4-nano":                 {InputPerMTokens: 0.2, OutputPerMTokens: 1.25, CacheReadPerMTokens: 0.02},
	"gpt-5.5":                      {InputPerMTokens: 5, OutputPerMTokens: 30, CacheReadPerMTokens: 0.5},
	"gpt-5.6":                      {InputPerMTokens: 5, OutputPerMTokens: 30, CacheReadPerMTokens: 0.5},
	"gpt-5.6-luna":                 {InputPerMTokens: 0.2, OutputPerMTokens: 1.2, CacheReadPerMTokens: 0.02},
	"grok-3":                       {InputPerMTokens: 3, OutputPerMTokens: 15},
	"grok-4":                       {InputPerMTokens: 3, OutputPerMTokens: 15},
	"grok-4.20-0309-non-reasoning": {InputPerMTokens: 1.25, OutputPerMTokens: 2.5, CacheReadPerMTokens: 0.2},
	"grok-4.20-0309-reasoning":     {InputPerMTokens: 1.25, OutputPerMTokens: 2.5, CacheReadPerMTokens: 0.2},
	"grok-4.20-multi-agent-0309":   {InputPerMTokens: 1.25, OutputPerMTokens: 2.5, CacheReadPerMTokens: 0.2},
	"grok-4.3":                     {InputPerMTokens: 1.25, OutputPerMTokens: 2.5, CacheReadPerMTokens: 0.2},
	"grok-4.5":                     {InputPerMTokens: 2, OutputPerMTokens: 6, CacheReadPerMTokens: 0.3},
	"grok-4.6":                     {InputPerMTokens: 2, OutputPerMTokens: 6, CacheReadPerMTokens: 0.5},
	"grok-build-0.1":               {InputPerMTokens: 1, OutputPerMTokens: 2, CacheReadPerMTokens: 0.2},
	"hy3":                          {InputPerMTokens: 0.14, OutputPerMTokens: 0.58, CacheReadPerMTokens: 0.035},
	"kimi-k2-0711-preview":         {InputPerMTokens: 0.6, OutputPerMTokens: 2.5, CacheReadPerMTokens: 0.15},
	"kimi-k2-0905-preview":         {InputPerMTokens: 0.6, OutputPerMTokens: 2.5, CacheReadPerMTokens: 0.15},
	"kimi-k2-thinking":             {InputPerMTokens: 0.6, OutputPerMTokens: 2.5, CacheReadPerMTokens: 0.15},
	"kimi-k2-thinking-turbo":       {InputPerMTokens: 1.15, OutputPerMTokens: 8, CacheReadPerMTokens: 0.15},
	"kimi-k2-turbo-preview":        {InputPerMTokens: 2.4, OutputPerMTokens: 10, CacheReadPerMTokens: 0.6},
	"kimi-k2.5":                    {InputPerMTokens: 0.6, OutputPerMTokens: 3, CacheReadPerMTokens: 0.1},
	"kimi-k2.6":                    {InputPerMTokens: 0.95, OutputPerMTokens: 4, CacheReadPerMTokens: 0.16},
	"kimi-k2.7":                    {InputPerMTokens: 0.95, OutputPerMTokens: 4, CacheReadPerMTokens: 0.19},
	"kimi-k2.7-code":               {InputPerMTokens: 0.95, OutputPerMTokens: 4, CacheReadPerMTokens: 0.19},
	"kimi-k2.7-code-highspeed":     {InputPerMTokens: 1.9, OutputPerMTokens: 8, CacheReadPerMTokens: 0.38},
	"kimi-k3":                      {InputPerMTokens: 3, OutputPerMTokens: 15, CacheReadPerMTokens: 0.3},
	"mimo":                         {InputPerMTokens: 0.14, OutputPerMTokens: 0.28, CacheReadPerMTokens: 0.0028},
	"minimax-m2":                   {InputPerMTokens: 0.3, OutputPerMTokens: 1.2},
	"minimax-m2.1":                 {InputPerMTokens: 0.3, OutputPerMTokens: 1.2, CacheReadPerMTokens: 0.03},
	"minimax-m2.5":                 {InputPerMTokens: 0.3, OutputPerMTokens: 1.2, CacheReadPerMTokens: 0.03},
	"minimax-m2.5-highspeed":       {InputPerMTokens: 0.6, OutputPerMTokens: 2.4, CacheReadPerMTokens: 0.06},
	"minimax-m2.7":                 {InputPerMTokens: 0.3, OutputPerMTokens: 1.2, CacheReadPerMTokens: 0.06},
	"minimax-m2.7-highspeed":       {InputPerMTokens: 0.6, OutputPerMTokens: 2.4, CacheReadPerMTokens: 0.06},
	"minimax-m3":                   {InputPerMTokens: 0.3, OutputPerMTokens: 1.2, CacheReadPerMTokens: 0.06},
	"minimax-mimo-v2.5":            {InputPerMTokens: 0.14, OutputPerMTokens: 0.28, CacheReadPerMTokens: 0.0028},
	"minimax-mimo-v2.5-pro":        {InputPerMTokens: 0.435, OutputPerMTokens: 0.87, CacheReadPerMTokens: 0.003625},
	"o1":                           {InputPerMTokens: 15, OutputPerMTokens: 60, CacheReadPerMTokens: 7.5},
	"o1-mini":                      {InputPerMTokens: 3, OutputPerMTokens: 12},
	"o1-preview":                   {InputPerMTokens: 15, OutputPerMTokens: 60},
	"o3":                           {InputPerMTokens: 2, OutputPerMTokens: 8, CacheReadPerMTokens: 0.5},
	"o3-mini":                      {InputPerMTokens: 1.1, OutputPerMTokens: 4.4, CacheReadPerMTokens: 0.55},
	"o4-mini":                      {InputPerMTokens: 1.1, OutputPerMTokens: 4.4, CacheReadPerMTokens: 0.275},
	"qvq-max":                      {InputPerMTokens: 1.2, OutputPerMTokens: 4.8},
	"qwen-max":                     {InputPerMTokens: 1.6, OutputPerMTokens: 6.4},
	"qwen-plus":                    {InputPerMTokens: 0.4, OutputPerMTokens: 1.2},
	"qwen-turbo":                   {InputPerMTokens: 0.05, OutputPerMTokens: 0.2},
	"qwen3-max":                    {InputPerMTokens: 1.2, OutputPerMTokens: 6},
	"qwen3-plus":                   {InputPerMTokens: 0.4, OutputPerMTokens: 1.6, CacheReadPerMTokens: 0.04},
	"qwen3.6-max-preview":          {InputPerMTokens: 1.3, OutputPerMTokens: 7.8, CacheReadPerMTokens: 0.13},
	"qwen3.6-plus":                 {InputPerMTokens: 0.5, OutputPerMTokens: 3, CacheReadPerMTokens: 0.05},
	"qwen3.7-max":                  {InputPerMTokens: 2.5, OutputPerMTokens: 7.5, CacheReadPerMTokens: 0.5},
	"qwen3.7-plus":                 {InputPerMTokens: 0.5, OutputPerMTokens: 3, CacheReadPerMTokens: 0.05},
	"qwen3.8-max":                  {InputPerMTokens: 2, OutputPerMTokens: 6, CacheReadPerMTokens: 0.25},
	"qwq-plus":                     {InputPerMTokens: 0.8, OutputPerMTokens: 2.4},
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
