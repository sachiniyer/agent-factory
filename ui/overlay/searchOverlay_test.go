package overlay

import "testing"

// Explicit forms so the source encoding can't silently change what is tested:
// cafeNFC uses composed U+00E9, cafeNFD uses "e" + combining acute U+0301.
const (
	cafeNFC      = "caf\u00e9"
	cafeNFD      = "cafe\u0301"
	cafeUpperNFC = "CAF\u00c9"
	cafeUpperNFD = "CAFE\u0301"
)

func TestFuzzyMatch(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		str     string
		want    bool
	}{
		// Basic ASCII behavior.
		{"empty pattern matches anything", "", "anything", true},
		{"exact match", "deploy", "deploy", true},
		{"subsequence match", "dpy", "deploy", true},
		{"out-of-order does not match", "yd", "deploy", false},
		{"pattern longer than str", "deployment", "deploy", false},

		// Case folding (previously handled by strings.ToLower at call sites).
		{"ascii case folded", "DePLoY", "deploy", true},
		{"non-ascii case folded", cafeUpperNFC, cafeNFC, true},
		{"sigma folds", "Σ", "σ", true},

		// Exact non-ASCII titles (#822: byte iteration broke these).
		{"accented exact", cafeNFC, cafeNFC, true},
		{"cjk exact", "分身艺术", "分身艺术", true},
		{"cjk subsequence", "分艺", "分身艺术", true},
		{"emoji-bearing title", "🚀dep", "🚀 deploy rocket", true},

		// NFC/NFD canonical equivalence (the report's headline case):
		// composed U+00E9 vs decomposed U+0065 U+0301.
		{"nfc pattern vs nfd str", cafeNFC, cafeNFD, true},
		{"nfd pattern vs nfc str", cafeNFD, cafeNFC, true},
		{"nfd upper pattern vs nfc str", cafeUpperNFD, cafeNFC, true},

		// False positive from the issue: the UTF-8 bytes of "é" (0xC3 0xA9)
		// appear scattered across "Ã" (0xC3 0x83) and "©" (0xC2 0xA9), so
		// byte-wise matching wrongly accepted this. Rune-wise must not.
		{"scattered bytes must not match", "\u00e9", "\u00c3\u00a9", false},
		{"multi-byte rune is not its byte prefix", "\u00e9", "e", false},

		{"non-ascii absent", "日", "deploy", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fuzzyMatch(tt.pattern, tt.str); got != tt.want {
				t.Errorf("fuzzyMatch(%q, %q) = %v, want %v", tt.pattern, tt.str, got, tt.want)
			}
		})
	}
}
