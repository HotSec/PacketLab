package llm

import (
	"strings"
	"unicode/utf8"
)

// estimate.go — 无 usage 字段时的 token 数估算
//
// 部分国产网关/代理（9router 等自建路由）不回传 usage，此时成本与用量
// 统计为空。这里用业界通用的启发式估算兜底：英文约 4 字符/token，
// 中文约 1.5 字符/token（GPT 系中文 tokenizer 大致 1 token ≈ 1.5-2 汉字）。
// 估算只用于展示级成本参考，精度约 ±30%；一旦上游回传真实 usage，
// buildLLMExchange 优先使用真实值并标记 TokensEstimated=false。

// EstimateTokens 估算一段文本的 token 数。
// 规则：CJK 字符按 1.5 字符/token，其余按 4 字符/token，结果向上取整。
// 空文本返回 0。与 OpenAI tiktoken 的 cl100k_base 在混合中英文文本上的
// 偏差通常在 ±30% 以内。
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	var cjkChars, otherChars int
	for _, r := range text {
		if isCJK(r) {
			cjkChars++
		} else {
			otherChars++
		}
	}
	if cjkChars == 0 && otherChars == 0 {
		return 0
	}
	tokens := float64(cjkChars)/1.5 + float64(otherChars)/4.0
	n := int(tokens + 0.999999) // 向上取整
	if n < 1 {
		return 1
	}
	return n
}

// isCJK 判断 rune 是否为 CJK 统一表意文字（含扩展 A、扩展 B 简查）。
// 覆盖 U+4E00–U+9FFF（基本区）、U+3400–U+4DBF（扩展A）。
// 全角标点/假名/谚文按非 CJK 处理（走 4 字符/token 的保守路径）。
func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) ||
		(r >= 0x3400 && r <= 0x4DBF) ||
		(r >= 0xF900 && r <= 0xFAFF) // CJK 兼容表意文字
}

// EstimateTokensUTF8 估算 UTF-8 字节串的 token 数（非法 UTF-8 按字节/token 粗估）。
func EstimateTokensUTF8(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	if !utf8.Valid(b) {
		return (len(b) + 3) / 4
	}
	return EstimateTokens(string(b))
}

// HasCJK 判断文本是否含 CJK 字符（用于日志/调试标注）。
func HasCJK(text string) bool {
	return strings.IndexFunc(text, isCJK) >= 0
}
