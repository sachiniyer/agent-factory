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

		// Amp: insert the resume subcommand after any leading Amp global
		// options. Explicit user subcommands are left unchanged so af does not
		// rewrite e.g. "amp review" into a different command.
		{"amp bare", "amp", "amp threads continue --last"},
		{"amp with global flag", "amp --no-ide", "amp --no-ide threads continue --last"},
		{"amp with global flag value", "amp --settings-file ~/.config/amp/test.json", "amp --settings-file ~/.config/amp/test.json threads continue --last"},
		{"amp with mode", "amp -m high --no-notifications", "amp -m high --no-notifications threads continue --last"},
		{"amp with optional global flag value", "amp --plugin-ready-timeout 30 --no-color", "amp --plugin-ready-timeout 30 --no-color threads continue --last"},
		{"amp with optional global flag attached value", "amp --plugin-ready-timeout=30", "amp --plugin-ready-timeout=30 threads continue --last"},
		{"amp with unknown value-taking global flag", "amp --future-setting workspace --no-ide", "amp --future-setting workspace --no-ide threads continue --last"},
		{"amp with unknown attached global flag value", "amp --future-setting=workspace --no-ide", "amp --future-setting=workspace --no-ide threads continue --last"},
		{
			"amp absolute path",
			"/home/user/.amp/bin/amp --no-ide",
			"/home/user/.amp/bin/amp --no-ide threads continue --last",
		},
		{
			"amp quoted path with spaces",
			"'/path with space/amp' --no-ide",
			"'/path with space/amp' --no-ide threads continue --last",
		},
		{"amp already last", "amp last", "amp last"},
		{"amp already l", "amp l", "amp l"},
		{"amp already threads continue", "amp threads continue --last", "amp threads continue --last"},
		{"amp already threads c", "amp threads c --last", "amp threads c --last"},
		{"amp explicit subcommand", "amp review", "amp review"},
		{"amp explicit subcommand after unknown boolean", "amp --future-bool review", "amp --future-bool review"},
		{"amp explicit threads list", "amp threads list --json", "amp threads list --json"},

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
		{
			"amp ionice wrapper",
			"ionice -c 3 amp --no-ide",
			"ionice -c 3 amp --no-ide threads continue --last",
		},

		// opencode: append --continue at the end. opencode's TUI is its DEFAULT
		// command ("opencode [project]") and --continue is a position-independent
		// boolean on it, so this is a tail append like claude/gemini/aider — NOT a
		// codex/amp-style subcommand splice.
		{"opencode bare", "opencode", "opencode --continue"},
		{"opencode with flag", "opencode --model anthropic/claude-opus-4-5", "opencode --model anthropic/claude-opus-4-5 --continue"},
		// opencode takes the project dir as a positional arg; the tail append must
		// not disturb it.
		{"opencode with project positional", "opencode /src/repo", "opencode /src/repo --continue"},
		{
			// The default install path, so this shape is the common case.
			"opencode absolute path",
			"/home/user/.opencode/bin/opencode --model anthropic/claude-opus-4-5",
			"/home/user/.opencode/bin/opencode --model anthropic/claude-opus-4-5 --continue",
		},
		{
			"opencode quoted path with spaces",
			`"/path with spaces/opencode" --model $MODEL`,
			`"/path with spaces/opencode" --model $MODEL --continue`,
		},

		// Already carrying a resume intent: leave the command untouched rather than
		// hand opencode two conflicting session selectors.
		{"opencode already --continue", "opencode --continue", "opencode --continue"},
		{"opencode already -c", "opencode -c", "opencode -c"},
		{"opencode already --session", "opencode --session ses_091dbcc41ffe", "opencode --session ses_091dbcc41ffe"},
		{"opencode already -s", "opencode -s ses_091dbcc41ffe", "opencode -s ses_091dbcc41ffe"},
		{"opencode already --session=", "opencode --session=ses_091dbcc41ffe", "opencode --session=ses_091dbcc41ffe"},
		{"opencode already -s=", "opencode -s=ses_091dbcc41ffe", "opencode -s=ses_091dbcc41ffe"},
		{"opencode already -s attached value", "opencode -sses_091dbcc41ffe", "opencode -sses_091dbcc41ffe"},
		{"opencode --continue --fork", "opencode --continue --fork", "opencode --continue --fork"},

		// #742: tokens BEFORE the agent token belong to a wrapper command, and
		// ionice/taskset's own "-c" must never be mistaken for opencode's
		// --continue. Scanning the whole token list would return these unchanged
		// and silently break opencode resume behind a wrapper.
		{
			"opencode ionice wrapper (-c is ionice's, not opencode's)",
			"ionice -c 3 opencode",
			"ionice -c 3 opencode --continue",
		},
		{
			"opencode taskset wrapper (-c is taskset's)",
			"taskset -c 0-3 opencode",
			"taskset -c 0-3 opencode --continue",
		},
		{"opencode env wrapper", "env FOO=bar opencode", "env FOO=bar opencode --continue"},
		{
			"opencode ionice wrapper with real --continue still idempotent",
			"ionice -c 3 opencode --continue",
			"ionice -c 3 opencode --continue",
		},

		// Restore feeds resumeProgram t.programCmd(), which is the INJECTED form —
		// and opencode's af-guidance seam is an OPENCODE_CONFIG env PREFIX
		// (injectSystemPrompt), not a flag. So on every restore of an opencode
		// session these two rewrites compose, and the resume append must survive the
		// prefix: the agent token is no longer token 0, and the af config path can
		// carry spaces (AGENT_FACTORY_HOME is user-controlled). If the prefix threw
		// the agent scan off, resume would silently drop and the session would come
		// back with NO context — the failure resume exists to prevent, arriving by a
		// route neither change owns alone.
		{
			"opencode with af's OPENCODE_CONFIG env prefix",
			"OPENCODE_CONFIG='/home/u/.agent-factory/opencode/af-config.jsonc' opencode",
			"OPENCODE_CONFIG='/home/u/.agent-factory/opencode/af-config.jsonc' opencode --continue",
		},
		{
			"opencode env prefix with a quoted path containing spaces",
			"OPENCODE_CONFIG='/af home/opencode/af-config.jsonc' opencode --model anthropic/claude-opus-4-5",
			"OPENCODE_CONFIG='/af home/opencode/af-config.jsonc' opencode --model anthropic/claude-opus-4-5 --continue",
		},
		{
			"opencode env prefix with an absolute agent path",
			"OPENCODE_CONFIG='/x/af.jsonc' /home/u/.opencode/bin/opencode",
			"OPENCODE_CONFIG='/x/af.jsonc' /home/u/.opencode/bin/opencode --continue",
		},
		{
			"opencode env prefix, already resuming, stays idempotent",
			"OPENCODE_CONFIG='/x/af.jsonc' opencode --continue",
			"OPENCODE_CONFIG='/x/af.jsonc' opencode --continue",
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
		{"amp bare", ProgramAmp, "amp", "amp threads continue " + id, true},
		{"amp with global flag", ProgramAmp, "amp --no-ide", "amp --no-ide threads continue " + id, true},
		{"amp quoted path", ProgramAmp, "'/path with spaces/amp' --no-ide", "'/path with spaces/amp' --no-ide threads continue " + id, true},
		{"amp already resume latest", ProgramAmp, "amp threads continue --last", "amp threads continue --last", false},
		{"amp explicit command", ProgramAmp, "amp review", "amp review", false},
		{"provider mismatch", ProgramClaude, "codex --model gpt-5", "codex --model gpt-5", false},
		{"unsupported provider", ProgramGemini, "gemini", "gemini", false},
		// opencode CAN resume an explicit id (`opencode --session <id>`), but af
		// never learns one: CaptureAgentConversation only implements codex (whose
		// rollout files expose an id on disk), and opencode has no --session-id
		// equivalent that would let af CHOOSE the id up front the way
		// ClaudeProgramWithSessionID does. With no id to pass, opencode
		// deliberately falls through to ok=false and Restore uses the
		// latest-session path (resumeProgram's --continue) — the same degradation
		// gemini and aider get. If opencode id capture is ever implemented, add a
		// ProgramOpencode case returning `program + " --session " + shellQuoteArg(id)`
		// and flip this row.
		{"opencode has no id-resume path in af", ProgramOpencode, "opencode", "opencode", false},
		{"opencode with flag still no id-resume", ProgramOpencode, "opencode --model anthropic/claude-opus-4-5", "opencode --model anthropic/claude-opus-4-5", false},
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

func TestResumeProgramWithConversationIDQuotesUnsafeID(t *testing.T) {
	const id = "thread id?x=1&y=2"
	cases := []struct {
		name  string
		agent string
		in    string
		want  string
	}{
		{"claude", ProgramClaude, "claude", "claude --resume 'thread id?x=1&y=2'"},
		{"codex", ProgramCodex, "codex", "codex resume 'thread id?x=1&y=2'"},
		{"amp", ProgramAmp, "amp", "amp threads continue 'thread id?x=1&y=2'"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, rewritten := ResumeProgramWithConversationID(tc.in, tc.agent, id)
			require.True(t, rewritten)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestShellQuoteArg(t *testing.T) {
	require.Equal(t, "abc-123_./:@%+=", shellQuoteArg("abc-123_./:@%+="))
	require.Equal(t, "'thread id?x=1&y=2'", shellQuoteArg("thread id?x=1&y=2"))
	require.Equal(t, `'it'"'"'s'`, shellQuoteArg("it's"))
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
		"amp",
		"amp --no-ide",
		"amp --settings-file ~/.config/amp/test.json",
		"ionice -c 3 amp --no-ide",
		"amp threads continue --last",
		"amp review",
		"opencode",
		"opencode --model anthropic/claude-opus-4-5",
		"opencode /src/repo",
		"opencode --continue",
		"opencode -c",
		"opencode --session ses_091dbcc41ffe",
		"opencode -s ses_091dbcc41ffe",
		"opencode --session=ses_091dbcc41ffe",
		"opencode -s=ses_091dbcc41ffe",
		"opencode -sses_091dbcc41ffe",
		"opencode --continue --fork",
		"ionice -c 3 opencode",
		"taskset -c 0-3 opencode",
		"env FOO=bar opencode",
		`"/path with spaces/opencode" --model $MODEL`,
		// The af-injected env-prefix form Restore actually re-spawns.
		"OPENCODE_CONFIG='/home/u/.agent-factory/opencode/af-config.jsonc' opencode",
		"OPENCODE_CONFIG='/af home/opencode/af-config.jsonc' opencode --model x",
		"OPENCODE_CONFIG='/x/af.jsonc' opencode --continue",
	} {
		once := resumeProgram(in)
		twice := resumeProgram(once)
		require.Equal(t, once, twice, "resumeProgram should be idempotent for %q", in)
	}
}
