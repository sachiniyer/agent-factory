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
		// Short-option attached-value syntax (#685): claude parses `-r5` as
		// `--resume 5`, so a second resume flag must not be appended.
		{"claude already -rVALUE", "claude -r5", "claude -r5"},
		{"claude already -rlatest", "claude -rlatest", "claude -rlatest"},
		{"claude already -rabc123", "claude -rabc123", "claude -rabc123"},
		{
			"claude -rVALUE with other flags",
			"claude --dangerously-skip-permissions -r5",
			"claude --dangerously-skip-permissions -r5",
		},

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
		// Codex shell-expansion preservation (#640): the codex rewrite must
		// preserve user metacharacters ($VAR, ~, *, ?, globs) verbatim. A
		// prior implementation tokenize+rejoined the program string and
		// defensively single-quoted any token containing shell metachars,
		// turning `--model $MODEL` into `--model '$MODEL'` and breaking
		// expansion on respawn. The fix splices " resume --last" into the
		// original string at a known byte offset instead.
		{
			"codex preserves $VAR in flag value",
			"codex --model $MODEL",
			"codex resume --last --model $MODEL",
		},
		{
			"codex preserves ~ in flag value",
			"codex --profile ~/profiles/foo",
			"codex resume --last --profile ~/profiles/foo",
		},
		{
			"codex exec preserves $VAR after subcommand",
			"codex exec --model $MODEL ./prompt.md",
			"codex exec resume --last --model $MODEL ./prompt.md",
		},
		{
			"codex absolute path preserves $VAR",
			"/usr/local/bin/codex --model $MODEL",
			"/usr/local/bin/codex resume --last --model $MODEL",
		},
		{
			"codex double-quoted path with spaces preserves quotes",
			`"/path with spaces/codex" --model $MODEL`,
			`"/path with spaces/codex" resume --last --model $MODEL`,
		},
		{
			"codex preserves quoted glob in flag value",
			"codex --include 'models/*.txt'",
			"codex resume --last --include 'models/*.txt'",
		},
		{
			"codex preserves bare glob in flag value",
			"codex --include models/*.txt",
			"codex resume --last --include models/*.txt",
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
		// Short-option attached-value syntax (#685): gemini parses `-r5` as
		// `--resume 5`. Appending `--resume latest` would make gemini
		// concatenate "5,latest" and fail with "Invalid session identifier".
		{"gemini already -rVALUE", "gemini -r5", "gemini -r5"},
		{"gemini already -rlatest", "gemini -rlatest", "gemini -rlatest"},
		{"gemini already -rarbitrary", "gemini -ranything", "gemini -ranything"},
		{
			"gemini -rVALUE with other flags",
			"gemini --model x -r5",
			"gemini --model x -r5",
		},

		// Wrapper-command flags must not be mistaken for agent resume flags
		// (#742). The already-has-resume scan is position-aware: only tokens
		// AFTER the agent token are inspected, so a wrapper flag like
		// `ionice -c 3` or `taskset -c 0-3` (whose `-c` collides with claude's
		// `--continue` short flag and gemini's flag space) doesn't suppress
		// the legitimate resume append. Mirrors the codex position-aware
		// check (#632).
		//
		// Critical regression case: `ionice -c 3 claude` previously matched
		// the leading `-c` and returned the program unchanged, launching a
		// fresh session and losing conversation history on respawn.
		{"claude ionice wrapper", "ionice -c 3 claude", "ionice -c 3 claude --continue"},
		{"claude taskset wrapper", "taskset -c 0-3 claude", "taskset -c 0-3 claude --continue"},
		{
			"claude wrapper with agent resume flag",
			"ionice -c 3 claude --resume X",
			"ionice -c 3 claude --resume X",
		},
		{
			"claude wrapper with agent attached resume",
			"ionice -c 3 claude -r5",
			"ionice -c 3 claude -r5",
		},
		{
			"claude env wrapper",
			"env FOO=bar claude",
			"env FOO=bar claude --continue",
		},
		// Aider wrappers: aider has no `-c` resume flag, but a wrapper whose
		// flags happened to match would still be wrongly scanned pre-#742;
		// guard the position-aware behavior regardless.
		{
			"aider ionice wrapper",
			"ionice -c 3 aider",
			"ionice -c 3 aider --restore-chat-history",
		},
		{
			"aider wrapper with agent resume flag",
			"ionice -c 3 aider --restore-chat-history",
			"ionice -c 3 aider --restore-chat-history",
		},
		{
			"aider wrapper with opt-out",
			"ionice -c 3 aider --no-restore-chat-history",
			"ionice -c 3 aider --no-restore-chat-history",
		},
		// Gemini wrappers: `ionice -c 3 gemini` must still get
		// `--resume latest`; the `-c` belongs to ionice, not gemini.
		{
			"gemini ionice wrapper",
			"ionice -c 3 gemini",
			"ionice -c 3 gemini --resume latest",
		},
		{
			"gemini taskset wrapper",
			"taskset -c 0-3 gemini",
			"taskset -c 0-3 gemini --resume latest",
		},
		{
			"gemini wrapper with agent resume flag",
			"ionice -c 3 gemini --resume 5",
			"ionice -c 3 gemini --resume 5",
		},
		{
			"gemini wrapper with agent attached resume",
			"ionice -c 3 gemini -r5",
			"ionice -c 3 gemini -r5",
		},

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

func TestClaudeProgramWithSessionID(t *testing.T) {
	const id = "019f386f-7206-7fc2-803b-f7045e07a242"
	cases := []struct {
		name     string
		in       string
		want     string
		injected bool
	}{
		{"bare", "claude", "claude --session-id " + id, true},
		{"with flag", "claude --model sonnet", "claude --model sonnet --session-id " + id, true},
		{"wrapper", "ionice -c 3 claude", "ionice -c 3 claude --session-id " + id, true},
		{"quoted path", "'/Applications/Claude Code.app/Contents/MacOS/claude' --foo", "'/Applications/Claude Code.app/Contents/MacOS/claude' --foo --session-id " + id, true},
		{"already session id", "claude --session-id abc", "claude --session-id abc", false},
		{"already session id equals", "claude --session-id=abc", "claude --session-id=abc", false},
		{"already resume", "claude --resume abc", "claude --resume abc", false},
		{"already continue", "claude --continue", "claude --continue", false},
		{"unknown", "mytool --foo", "mytool --foo", false},
		{"empty id", "claude", "claude", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotID := id
			if tc.name == "empty id" {
				gotID = ""
			}
			got, injected := ClaudeProgramWithSessionID(tc.in, gotID)
			require.Equal(t, tc.want, got)
			require.Equal(t, tc.injected, injected)
		})
	}
}

func TestResumeProgramWithConversationID(t *testing.T) {
	const id = "019f386f-7206-7fc2-803b-f7045e07a242"
	cases := []struct {
		name          string
		agent         string
		in            string
		want          string
		wantRewritten bool
	}{
		{"claude bare", ProgramClaude, "claude", "claude --resume " + id, true},
		{"claude with flag", ProgramClaude, "claude --model sonnet", "claude --model sonnet --resume " + id, true},
		{"claude wrapper", ProgramClaude, "ionice -c 3 claude", "ionice -c 3 claude --resume " + id, true},
		{"claude already resume", ProgramClaude, "claude --resume old", "claude --resume old", false},
		{"claude already continue", ProgramClaude, "claude --continue", "claude --continue", false},
		{"codex bare", ProgramCodex, "codex", "codex resume " + id, true},
		{"codex with flag", ProgramCodex, "codex --model gpt-5", "codex resume " + id + " --model gpt-5", true},
		{"codex exec", ProgramCodex, "codex exec --foo bar", "codex exec resume " + id + " --foo bar", true},
		{"codex quoted path", ProgramCodex, "'/path with spaces/codex' --model gpt-5", "'/path with spaces/codex' resume " + id + " --model gpt-5", true},
		{"codex already resume", ProgramCodex, "codex resume --last", "codex resume --last", false},
		{"provider mismatch", ProgramClaude, "codex --model gpt-5", "codex --model gpt-5", false},
		{"unsupported provider", ProgramGemini, "gemini", "gemini", false},
		{"empty id", ProgramCodex, "codex", "codex", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotID := id
			if tc.name == "empty id" {
				gotID = ""
			}
			got, rewritten := ResumeProgramWithConversationID(tc.in, tc.agent, gotID)
			require.Equal(t, tc.want, got)
			require.Equal(t, tc.wantRewritten, rewritten)
		})
	}
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
		"gemini -r5",
		"gemini -rlatest",
		"claude --resume=abc123",
		"claude -r=abc123",
		"claude -r5",
		"claude -rlatest",
		"codex --profile resume",
		"codex --profile=resume",
		"codex exec --profile resume",
		// #640 — shell-expansion-preserving splice must also be idempotent.
		"codex --model $MODEL",
		"codex exec --model $MODEL ./prompt.md",
		"codex --profile ~/profiles/foo",
		"codex --include 'models/*.txt'",
		`"/path with spaces/codex" --model $MODEL`,
		// #742 — wrapper-prefixed programs must be idempotent too.
		"ionice -c 3 claude",
		"taskset -c 0-3 claude",
		"env FOO=bar claude",
		"ionice -c 3 aider",
		"ionice -c 3 gemini",
		"taskset -c 0-3 gemini",
	} {
		once := resumeProgram(in)
		twice := resumeProgram(once)
		require.Equal(t, once, twice, "resumeProgram should be idempotent for %q", in)
	}
}
