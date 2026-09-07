// Package llmtokens provides a dependency-free token-count heuristic used to
// budget the LLM input (system + tools + conversation history) before sending,
// so the tactical layer can trigger context compaction when the estimate
// crosses a threshold. It is a coarse estimate, not a tokenizer.
package llmtokens

import "unicode"

// EstimateTokens returns a rough token-count estimate for s using a
// character-based heuristic. CJK and full-width runes count ~0.5 token each
// (Qwen-family tokenizers merge common CJK bigrams, ~2 runes/token); other
// runes (ASCII / JSON structure) count ~0.25 token each (~4/token).
//
// Calibration: measured tactical requests run ~2.1 chars/token for CJK prose
// and ~2.7 chars/token overall once JSON structure is mixed in — this
// heuristic lands within ~15% of the gateway's Usage.InputTokens. The tactical
// model has 32k context against a 16k budget, so the headroom tolerates the
// residual error. Tune against Usage.InputTokens if a tighter budget is needed.
func EstimateTokens(s string) int {
	wide := 0
	narrow := 0
	for _, r := range s {
		if isWide(r) {
			wide++
		} else {
			narrow++
		}
	}
	// Ceil-divide: wide → /2, narrow → /4.
	return (wide+1)/2 + (narrow+3)/4
}

// isWide reports whether r is a CJK ideograph, kana, hangul, CJK punctuation,
// or full-width form — runes that tokenize at roughly two per token.
func isWide(r rune) bool {
	switch {
	case unicode.Is(unicode.Han, r),
		unicode.Is(unicode.Hiragana, r),
		unicode.Is(unicode.Katakana, r),
		unicode.Is(unicode.Hangul, r),
		r >= 0x3000 && r <= 0x303F, // CJK punctuation
		r >= 0xFF00 && r <= 0xFFEF: // full-width forms
		return true
	}
	return false
}
