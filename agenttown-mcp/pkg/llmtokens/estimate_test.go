package llmtokens

import "testing"

func TestEstimateTokens_Empty(t *testing.T) {
	if got := EstimateTokens(""); got != 0 {
		t.Fatalf("EstimateTokens(\"\") = %d, want 0", got)
	}
}

func TestEstimateTokens_CJKAndASCII(t *testing.T) {
	// 4 CJK runes ≈ 2 tokens; "abcd" (4 narrow) ≈ 1 token.
	if got, want := EstimateTokens("你好世界abcd"), 3; got != want {
		t.Fatalf("EstimateTokens = %d, want %d", got, want)
	}
}

func TestEstimateTokens_NarrowRounding(t *testing.T) {
	// Single narrow rune rounds up to 1 token.
	if got := EstimateTokens("a"); got != 1 {
		t.Fatalf("EstimateTokens(\"a\") = %d, want 1", got)
	}
	// 5 narrow runes → ceil(5/4) = 2.
	if got := EstimateTokens("abcde"); got != 2 {
		t.Fatalf("EstimateTokens(\"abcde\") = %d, want 2", got)
	}
}

func TestEstimateTokens_JSONStructure(t *testing.T) {
	// JSON key:value with CJK value — CJK dominates but ASCII structure adds
	// a bounded, non-zero amount.
	got := EstimateTokens(`{"content":"你好"}`)
	// 你好 = 1 wide token; ~16 structural narrow → 4. Total ~5.
	if got < 3 || got > 6 {
		t.Fatalf("EstimateTokens(json) = %d, want in [3,6]", got)
	}
}
