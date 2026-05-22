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
		// Long-option = syntax: claude accepts `--resume=<value>` so the
		// rewrite must detect it and skip appending `--continue` (regression
		// of #604 for claude — same flag-detection class as gemini).
		{"claude already --resume=value", "claude --resume=abc123", "claude --resume=abc123"},
		{"claude already --resume=latest", "claude --resume=latest", "claude --resume=latest"},
		// Short-option = syntax: detect defensively so we don't accumulate
		// flags if the CLI accepts `-r=VALUE` (same class as gemini's #633).
		{"claude already -r=value", "claude -r=abc123", "claude -r=abc123"},
		{"claude already -r=latest", "claude -r=latest", "claude -r=latest"},

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
		// Codex: "resume" appearing in flag-value position must NOT trip
		// the already-has-resume check (#632). The subcommand can only
		// appear immediately after `codex` or after `codex exec`, so the
		// detection is position-aware.
		{
			"codex profile named resume",
			"codex --profile resume",
			"codex resume --last --profile resume",
		},
		{
			"codex profile=resume equals form",
			"codex --profile=resume",
			"codex resume --last --profile=resume",
		},
		{
			"codex exec with profile named resume",
			"codex exec --profile resume",
			"codex exec resume --last --profile resume",
		},
		{
			"codex model value named resume",
			"codex --model resume",
			"codex resume --last --model resume",
		},

		// Aider: append --restore-chat-history at the end. Position-
		// independent flag, so the original program string is preserved
		// verbatim except for the appended token. Aider silently falls
		// back to a fresh chat when .aider.chat.history.md is absent.
		{"aider bare", "aider", "aider --restore-chat-history"},
		{
			"aider with model flag",
			"aider --model x",
			"aider --model x --restore-chat-history",
		},
		{
			"aider absolute path",
			"/usr/local/bin/aider --foo",
			"/usr/local/bin/aider --foo --restore-chat-history",
		},
		{
			// Quoted path with spaces (regression for #569): existing
			// quoting on the executable token must survive untouched.
			"aider quoted path with spaces",
			"'/path with space/aider' --foo",
			"'/path with space/aider' --foo --restore-chat-history",
		},
		// Aider: already-has-resume cases — no-op.
		{
			"aider already --restore-chat-history",
			"aider --restore-chat-history",
			"aider --restore-chat-history",
		},
		// Aider: explicit opt-out — respect the user's choice and leave
		// the program string alone.
		{
			"aider explicit --no-restore-chat-history",
			"aider --no-restore-chat-history",
			"aider --no-restore-chat-history",
		},

		// Gemini: append --resume latest at the end. "latest" resumes
		// the most recent session in cwd and silently falls back to a
		// fresh session if none exists.
		{"gemini bare", "gemini", "gemini --resume latest"},
		{
			"gemini with flag",
			"gemini --model x",
			"gemini --model x --resume latest",
		},
		{
			"gemini absolute path",
			"/usr/local/bin/gemini --foo",
			"/usr/local/bin/gemini --foo --resume latest",
		},
		{
			// Quoted path with spaces (regression for #569).
			"gemini quoted path with spaces",
			"'/path with space/gemini' --foo",
			"'/path with space/gemini' --foo --resume latest",
		},
		// Gemini: already-has-resume cases — no-op so repeated Restore
		// calls don't accumulate flags.
		{
			"gemini already --resume latest",
			"gemini --resume latest",
			"gemini --resume latest",
		},
		{
			// User picked a specific session by index — respect it,
			// don't clobber with "latest".
			"gemini already --resume numeric",
			"gemini --resume 5",
			"gemini --resume 5",
		},
		{"gemini already -r", "gemini -r latest", "gemini -r latest"},
		// Long-option = syntax: gemini accepts `--resume=<value>` and would
		// concatenate duplicate values with commas ("5,latest") and exit 42
		// if we appended a second flag (#604).
		{
			"gemini already --resume=numeric",
			"gemini --resume=5",
			"gemini --resume=5",
		},
		{
			"gemini already --resume=latest",
			"gemini --resume=latest",
			"gemini --resume=latest",
		},
		{
			"gemini already --resume=arbitrary",
			"gemini --resume=anything",
			"gemini --resume=anything",
		},
		// Short-option = syntax: gemini accepts `-r=<value>` and would
		// concatenate duplicate values with commas ("5,latest") and exit 1
		// if we appended a second flag (#633).
		{"gemini already -r=numeric", "gemini -r=5", "gemini -r=5"},
		{"gemini already -r=latest", "gemini -r=latest", "gemini -r=latest"},
		{"gemini already -r=arbitrary", "gemini -r=anything", "gemini -r=anything"},

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

// TestResumeProgram_CodexProfileResumeFalsePositive guards #632: the codex
// "already has resume" check must be position-aware. A profile named "resume"
// (or any other flag value of "resume") must not be mistaken for the resume
// subcommand, otherwise the user loses their conversation on respawn.
func TestResumeProgram_CodexProfileResumeFalsePositive(t *testing.T) {
	in := "codex --profile resume"
	want := "codex resume --last --profile resume"
	got := resumeProgram(in)
	require.Equal(t, want, got, "BUG: 'resume' in flag value position should not be detected as subcommand")
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
		"aider --model x",
		"aider --no-restore-chat-history",
		"gemini",
		"gemini --model x",
		"gemini --resume 5",
		"gemini --resume=5",
		"gemini -r=5",
		"gemini -r=latest",
		"claude --resume=abc123",
		"claude -r=abc123",
		"codex --profile resume",
		"codex --profile=resume",
		"codex exec --profile resume",
	} {
		once := resumeProgram(in)
		twice := resumeProgram(once)
		require.Equal(t, once, twice, "resumeProgram should be idempotent for %q", in)
	}
}
