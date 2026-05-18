package tmux

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResumeProgram(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// Claude: append --continue at the end. Position-independent flag,
		// so the original program string is preserved verbatim except for
		// the appended token.
		{"claude bare", "claude", "claude --continue"},
		{
			"claude with flag",
			"claude --dangerously-skip-permissions",
			"claude --dangerously-skip-permissions --continue",
		},
		{
			"claude absolute path",
			"/home/user/.local/bin/claude --foo",
			"/home/user/.local/bin/claude --foo --continue",
		},
		{
			// Quoted path with spaces (regression for #569): existing
			// quoting on the executable token must survive untouched.
			"claude quoted path with spaces",
			"'/Applications/Claude Code.app/Contents/MacOS/claude' --foo",
			"'/Applications/Claude Code.app/Contents/MacOS/claude' --foo --continue",
		},
		// Claude: already-has-resume cases — no-op so repeated Restore
		// calls (e.g. user kills tmux several times in a row) don't
		// accumulate flags.
		{"claude already --continue", "claude --continue", "claude --continue"},
		{"claude already -c", "claude -c", "claude -c"},
		{"claude already --resume", "claude --resume foo", "claude --resume foo"},
		{"claude already -r", "claude -r abc123", "claude -r abc123"},

		// Codex: insert "resume --last" after the codex token (subcommand
		// position matters for codex; a tail append wouldn't parse).
		{"codex bare", "codex", "codex resume --last"},
		{"codex with model flag", "codex --model gpt-5", "codex resume --last --model gpt-5"},
		{
			"codex absolute path",
			"/usr/local/bin/codex --model gpt-5",
			"/usr/local/bin/codex resume --last --model gpt-5",
		},
		// Codex headless: "exec" is a subcommand; resume comes after it.
		{"codex exec", "codex exec", "codex exec resume --last"},
		{"codex exec with flag", "codex exec --foo bar", "codex exec resume --last --foo bar"},
		// Codex: already-has-resume cases — no-op.
		{"codex already resume", "codex resume --last", "codex resume --last"},
		{"codex exec already resume", "codex exec resume --last", "codex exec resume --last"},

		// Agents without a documented resume-most-recent flag are passed
		// through unchanged. Deferred to follow-up issues if/when those
		// CLIs expose comparable flags.
		{"aider with model flag", "aider --model x", "aider --model x"},
		{"aider absolute path", "/usr/local/bin/aider", "/usr/local/bin/aider"},
		{"gemini bare", "gemini", "gemini"},
		{"gemini with flag", "gemini --quiet", "gemini --quiet"},

		// Unknown programs are passed through unchanged so unrelated CLIs
		// aren't accidentally rewritten.
		{"unknown program", "mytool --bar", "mytool --bar"},
		{"empty", "", ""},
		{"whitespace only", "   ", "   "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, resumeProgram(tc.in))
		})
	}
}

// TestResumeProgram_QuotedCodexPath verifies that a codex executable whose
// path is quoted because it contains spaces still gets the resume insertion
// in the right token slot, and the rebuilt string preserves shell-safe
// quoting on the path.
func TestResumeProgram_QuotedCodexPath(t *testing.T) {
	got := resumeProgram("'/path with space/codex' --model gpt-5")
	require.Equal(t, "'/path with space/codex' resume --last --model gpt-5", got)
}

// TestResumeProgram_Idempotent verifies that running the rewrite twice
// produces the same string as running it once — defense against repeated
// Restore() calls (e.g. user kills tmux multiple times in a row).
func TestResumeProgram_Idempotent(t *testing.T) {
	for _, in := range []string{
		"claude",
		"claude --foo",
		"codex",
		"codex exec",
		"codex --model gpt-5",
		"aider",
		"gemini",
	} {
		once := resumeProgram(in)
		twice := resumeProgram(once)
		require.Equal(t, once, twice, "resumeProgram should be idempotent for %q", in)
	}
}
