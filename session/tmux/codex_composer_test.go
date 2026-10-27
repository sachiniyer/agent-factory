package tmux

import "testing"

// codexUltraComposer is the real Codex 0.163.0 pane capture from #5383: the
// composer prompt is "»" (U+00BB), which Codex draws only while the Ultra
// reasoning tier is active (upstream codex-rs/tui
// bottom_pane/effort_ignition.rs EffortTier::prompt_glyph).
const codexUltraComposer = "\x1b[38;5;141m»\x1b[0m \x1b[2mAsk Codex to do anything\r\n" +
	"  \x1b[1m←\x1b[0m for agents · \x1b[1m?\x1b[0m for shortcuts"

// TestIsCodexComposerLine pins the glyph set and the positional contract of
// the Codex composer check (#5383): "›" at every reasoning effort plus "»"
// under the Ultra tier, accepted only at the start of a line. A picker row
// like "› 1. Yes, continue" answers true on purpose — position is all the
// glyph claims, and telling composer from selected option is the caller's
// anchored job (CodexTrustPromptPresent, the picker inspectors).
func TestIsCodexComposerLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{"empty", "", false},
		{"whitespace only", "   ", false},
		{"bare old glyph", "› ", true},
		{"bare Ultra glyph", "» ", true},
		{"old glyph composer", "› Ask Codex to do anything", true},
		{"Ultra glyph composer", "» Ask Codex to do anything", true},
		{"indented glyph still line-leading", "  › ", true},
		{"old glyph picker row", "› 1. Yes, continue", true},
		{"glyph mid-line", "x » y", false},
		{"glyph mid-path", "/src/a»b/repo", false},
		{"old glyph mid-path", "/src/a›b/repo", false},
		{"trailing glyph", "text ›", false},
		{"claude glyph is not codex", "❯ ", false},
		{"aider glyph is not codex", "> ", false},
		{"devin glyph is not codex", "❭ ", false},
		{"single guillemet is not the glyph", "« bonjour", false},
		{"ANSI-stripped Ultra prompt", "» [Pasted Content]", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsCodexComposerLine(tc.line); got != tc.want {
				t.Errorf("IsCodexComposerLine(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

// TestCodexStatusLineModelComposerGlyph pins that a line-leading prompt
// marker never leaks into the learned model segment. The status line's second
// segment must be path-shaped, so a composer line can only collide when its
// typed text ends in " · <path>" — exactly the shape the marker exclusion
// exists to reject.
func TestCodexStatusLineModelComposerGlyph(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "ordinary status line still reads",
			content: "• Working\n\n  gpt-5.6-sol max · ~/agent-factory",
			want:    "gpt-5.6-sol max",
		},
		{
			name:    "old glyph composer text is not a model",
			content: "› rename thing · ~/repo",
			want:    "",
		},
		{
			name:    "Ultra glyph composer text is not a model (#5383)",
			content: "» rename thing · ~/repo",
			want:    "",
		},
		{
			name:    "Ultra capture's hint row is not a status line (#5383)",
			content: codexUltraComposer,
			want:    "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexStatusLineModel(tc.content); got != tc.want {
				t.Errorf("codexStatusLineModel(%q) = %q, want %q", tc.content, got, tc.want)
			}
		})
	}
}
