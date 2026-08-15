package llm

// limits.go — 模型上下文/输出限制（models.dev [limit] 段，scripts/sync_pricing.py 生成）。
// 未收录的模型返回零值（前端不展示限制）。

// ModelLimits 模型上下文与输出长度限制（tokens）。
type ModelLimits struct {
	ContextLength int // 最大上下文长度
	MaxOutput     int // 最大输出长度
}

// modelLimits 限制表。key 语义同 pricingTable（'-' 边界前缀匹配）。
var modelLimits = map[string]ModelLimits{
	"claude-fable-5":               {ContextLength: 1000000, MaxOutput: 128000},
	"claude-haiku-4-5-20251001":    {ContextLength: 200000, MaxOutput: 64000},
	"claude-opus-4-5-20251101":     {ContextLength: 200000, MaxOutput: 64000},
	"claude-opus-4-6":              {ContextLength: 1000000, MaxOutput: 128000},
	"claude-opus-4-7":              {ContextLength: 1000000, MaxOutput: 128000},
	"claude-opus-4-8":              {ContextLength: 1000000, MaxOutput: 128000},
	"claude-sonnet-4-5":            {ContextLength: 1000000, MaxOutput: 0},
	"claude-sonnet-4-5-20250929":   {ContextLength: 1000000, MaxOutput: 64000},
	"claude-sonnet-4-6":            {ContextLength: 1000000, MaxOutput: 128000},
	"claude-sonnet-5":              {ContextLength: 1000000, MaxOutput: 128000},
	"deepseek-chat":                {ContextLength: 1000000, MaxOutput: 384000},
	"deepseek-reasoner":            {ContextLength: 1000000, MaxOutput: 384000},
	"gemini-2.5-flash-lite":        {ContextLength: 1048576, MaxOutput: 65536},
	"gemini-3-flash-preview":       {ContextLength: 1048576, MaxOutput: 65536},
	"gemini-3.1-flash-lite":        {ContextLength: 1048576, MaxOutput: 65536},
	"gemini-3.1-pro-preview":       {ContextLength: 1048576, MaxOutput: 65536},
	"gemini-3.5-flash":             {ContextLength: 1048576, MaxOutput: 65536},
	"glm-4.5":                      {ContextLength: 131072, MaxOutput: 98304},
	"glm-4.5-air":                  {ContextLength: 131072, MaxOutput: 98304},
	"glm-4.5v":                     {ContextLength: 64000, MaxOutput: 16384},
	"glm-4.6":                      {ContextLength: 204800, MaxOutput: 131072},
	"glm-4.6v":                     {ContextLength: 128000, MaxOutput: 32768},
	"glm-4.7":                      {ContextLength: 204800, MaxOutput: 131072},
	"glm-4.7-flashx":               {ContextLength: 200000, MaxOutput: 131072},
	"glm-5":                        {ContextLength: 204800, MaxOutput: 131072},
	"glm-5.1":                      {ContextLength: 200000, MaxOutput: 131072},
	"glm-5.2":                      {ContextLength: 1000000, MaxOutput: 131072},
	"glm-5v-turbo":                 {ContextLength: 200000, MaxOutput: 131072},
	"gpt-3.5-turbo":                {ContextLength: 16385, MaxOutput: 4096},
	"gpt-4":                        {ContextLength: 8192, MaxOutput: 8192},
	"gpt-4-turbo":                  {ContextLength: 128000, MaxOutput: 4096},
	"gpt-4.1":                      {ContextLength: 1047576, MaxOutput: 32768},
	"gpt-4.1-mini":                 {ContextLength: 1047576, MaxOutput: 32768},
	"gpt-4.1-nano":                 {ContextLength: 1047576, MaxOutput: 32768},
	"gpt-4o":                       {ContextLength: 128000, MaxOutput: 16384},
	"gpt-4o-mini":                  {ContextLength: 128000, MaxOutput: 16384},
	"gpt-5-mini":                   {ContextLength: 400000, MaxOutput: 128000},
	"gpt-5-nano":                   {ContextLength: 400000, MaxOutput: 128000},
	"gpt-5-pro":                    {ContextLength: 400000, MaxOutput: 272000},
	"gpt-5.1":                      {ContextLength: 400000, MaxOutput: 128000},
	"gpt-5.2":                      {ContextLength: 400000, MaxOutput: 128000},
	"gpt-5.2-pro":                  {ContextLength: 400000, MaxOutput: 128000},
	"gpt-5.3-codex":                {ContextLength: 400000, MaxOutput: 128000},
	"gpt-5.4":                      {ContextLength: 1050000, MaxOutput: 128000},
	"gpt-5.4-mini":                 {ContextLength: 400000, MaxOutput: 128000},
	"gpt-5.4-nano":                 {ContextLength: 400000, MaxOutput: 128000},
	"gpt-5.5":                      {ContextLength: 1050000, MaxOutput: 128000},
	"grok-4.20-0309-non-reasoning": {ContextLength: 1000000, MaxOutput: 30000},
	"grok-4.20-0309-reasoning":     {ContextLength: 1000000, MaxOutput: 30000},
	"grok-4.20-multi-agent-0309":   {ContextLength: 1000000, MaxOutput: 30000},
	"grok-4.3":                     {ContextLength: 1000000, MaxOutput: 30000},
	"grok-build-0.1":               {ContextLength: 256000, MaxOutput: 256000},
	"kimi-k2-0711-preview":         {ContextLength: 131072, MaxOutput: 16384},
	"kimi-k2-0905-preview":         {ContextLength: 262144, MaxOutput: 262144},
	"kimi-k2-thinking":             {ContextLength: 262144, MaxOutput: 262144},
	"kimi-k2-thinking-turbo":       {ContextLength: 262144, MaxOutput: 262144},
	"kimi-k2-turbo-preview":        {ContextLength: 262144, MaxOutput: 262144},
	"kimi-k2.5":                    {ContextLength: 262144, MaxOutput: 262144},
	"kimi-k2.6":                    {ContextLength: 262144, MaxOutput: 262144},
	"minimax-m2":                   {ContextLength: 196608, MaxOutput: 128000},
	"minimax-m2.1":                 {ContextLength: 204800, MaxOutput: 131072},
	"minimax-m2.5":                 {ContextLength: 204800, MaxOutput: 131072},
	"minimax-m2.5-highspeed":       {ContextLength: 204800, MaxOutput: 131072},
	"minimax-m2.7":                 {ContextLength: 204800, MaxOutput: 131072},
	"minimax-m2.7-highspeed":       {ContextLength: 204800, MaxOutput: 131072},
	"minimax-m3":                   {ContextLength: 1000000, MaxOutput: 128000},
	"o1":                           {ContextLength: 200000, MaxOutput: 100000},
	"o3":                           {ContextLength: 200000, MaxOutput: 100000},
	"o3-mini":                      {ContextLength: 200000, MaxOutput: 100000},
	"o4-mini":                      {ContextLength: 200000, MaxOutput: 100000},
	"qvq-max":                      {ContextLength: 131072, MaxOutput: 8192},
	"qwen-max":                     {ContextLength: 32768, MaxOutput: 8192},
	"qwen-plus":                    {ContextLength: 1000000, MaxOutput: 32768},
	"qwen-turbo":                   {ContextLength: 1000000, MaxOutput: 16384},
	"qwen3-max":                    {ContextLength: 262144, MaxOutput: 65536},
	"qwen3.6-max-preview":          {ContextLength: 262144, MaxOutput: 65536},
	"qwen3.6-plus":                 {ContextLength: 1000000, MaxOutput: 65536},
	"qwen3.7-max":                  {ContextLength: 1000000, MaxOutput: 65536},
	"qwen3.7-plus":                 {ContextLength: 1000000, MaxOutput: 65536},
	"qwq-plus":                     {ContextLength: 131072, MaxOutput: 8192},
}

// LookupLimits 查找模型限制。与 LookupPricing 相同的前缀边界匹配语义。
// 未收录返回零值（ContextLength 与 MaxOutput 均为 0）。
func LookupLimits(model string) ModelLimits {
	m := toLowerASCII(model)
	var best ModelLimits
	var bestKeyLen int
	for key, lim := range modelLimits {
		k := toLowerASCII(key)
		if len(k) <= bestKeyLen {
			continue
		}
		if !hasPrefix(m, k) {
			continue
		}
		if len(m) > len(k) && m[len(k)] != '-' {
			continue
		}
		best = lim
		bestKeyLen = len(k)
	}
	return best
}
