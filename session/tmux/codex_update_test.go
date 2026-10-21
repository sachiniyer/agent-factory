package tmux

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const codexUpdatePicker = `  ✨ Update available! 0.0.0 -> 9.9.9

  Release notes: https://github.com/openai/codex/releases/latest

› 1. Update now (runs ` + "`npm install -g @openai/codex@latest`" + `)
  2. Skip
  3. Skip until next version

  Press enter to continue`

const codexUpdatePickerSkipSelected = `  ✨ Update available! 0.0.0 -> 9.9.9

  Release notes: https://github.com/openai/codex/releases/latest

  1. Update now (runs ` + "`npm install -g @openai/codex@latest`" + `)
› 2. Skip
  3. Skip until next version

  Press enter to continue`

const codexCurrentUpdatePicker = `  Update available · 0.0.0 → 9.9.9
  Release notes: https://github.com/openai/codex/releases/latest

› 1. Update now (runs ` + "`npm install -g @openai/codex`" + `)
  2. Skip
  3. Skip until next version

  enter continue · esc skip`

const codexCurrentUpdatePickerSkipSelected = `  Update available · 0.0.0 → 9.9.9
  Release notes: https://github.com/openai/codex/releases/latest

  1. Update now (runs ` + "`npm install -g @openai/codex`" + `)
› 2. Skip
  3. Skip until next version

  enter continue · esc skip`

const codexUpdatePickerHeaderAndURL = `  Update available · 0.0.0 → 9.9.9
  Release notes: https://github.com/openai/codex/releases/latest`

const codexClippedUpdatePicker = `  Update available · 0.0.0
  → 9.9.9
  Release notes:
  https://github.com/opena
  i/codex/releases/latest
↓
› 1. Update now (runs

  enter continue · esc
  skip`

const codexClippedUpdatePickerSkipSelected = `  Update available · 0.0.0
  → 9.9.9
  Release notes:
  https://github.com/opena
  i/codex/releases/latest
↑
› 2. Skip
↓

  enter continue · esc
  skip`

const codexPersistentUpdateBanner = `╭─────────────────────────────────────────────────╮
│ ✨ Update available! 0.154.0 -> 0.155.1         │
│ Run npm install -g @openai/codex to update.     │
│                                                 │
│ See full release notes:                         │
│ https://github.com/openai/codex/releases/latest │
╰─────────────────────────────────────────────────╯
  …
› Ask Codex to do anything`

func TestCheckAndHandleTrustPrompt_CodexUpdatePickerSkipsWithoutRunningUpdate(t *testing.T) {
	tests := []struct {
		name     string
		initial  string
		selected string
	}{
		{name: "original update picker", initial: codexUpdatePicker, selected: codexUpdatePickerSkipSelected},
		{name: "current update picker", initial: codexCurrentUpdatePicker, selected: codexCurrentUpdatePickerSkipSelected},
		{name: "clipped current picker", initial: codexClippedUpdatePicker, selected: codexClippedUpdatePickerSkipSelected},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session, commands := runTrustPromptSequence(t, ProgramCodex, test.initial, test.selected)

			require.True(t, session.CheckAndHandleTrustPrompt())
			keys := sentKeystrokes(*commands)
			require.Equal(t, []string{
				"tmux send-keys -t =af_trust: Down",
				"tmux send-keys -t =af_trust: Enter",
			}, keys, "af must navigate, observe Skip highlighted, then confirm")
			require.NotContains(t, strings.Join(keys, "\n"), " 2",
				"a picker ordinal is not a substitute for verified navigation")
		})
	}
}

func TestCheckAndHandleTrustPrompt_CodexUpdateWaitsForSkipHighlight(t *testing.T) {
	session, commands := runTrustPromptSequence(t, ProgramCodex,
		codexUpdatePicker,
		codexUpdatePicker, // immediate post-navigation capture is stale
		codexUpdatePickerSkipSelected,
	)

	require.True(t, session.CheckAndHandleTrustPrompt())
	require.Equal(t, []string{
		"tmux send-keys -t =af_trust: Down",
	}, sentKeystrokes(*commands), "Enter must not confirm the still-highlighted Update now row")

	require.True(t, session.CheckAndHandleTrustPrompt())
	require.Equal(t, []string{
		"tmux send-keys -t =af_trust: Down",
		"tmux send-keys -t =af_trust: Enter",
	}, sentKeystrokes(*commands), "a later poll may confirm only after observing Skip selected")
}

func TestCheckAndHandleTrustPrompt_CodexUpdatePartialPickerHoldsWithoutInput(t *testing.T) {
	partial := strings.TrimSuffix(codexUpdatePicker, "\n\n  Press enter to continue")
	handled, commands := runTrustPromptCheck(t, ProgramCodex, partial)

	require.True(t, handled, "a recognized picker without its footer is still a startup blocker")
	require.Empty(t, sentKeystrokes(commands), "an incompletely rendered picker authorizes no input")
}

func TestCheckAndHandleTrustPrompt_CodexUpdateHeaderAndURLHoldWithoutInput(t *testing.T) {
	handled, commands := runTrustPromptCheck(t, ProgramCodex, codexUpdatePickerHeaderAndURL)

	require.True(t, handled, "picker-only evidence with a hidden cursor must block prompt delivery")
	require.Empty(t, sentKeystrokes(commands), "a picker with no observable selected row authorizes no input")
}

func TestCheckAndHandleTrustPrompt_CodexUpdateWaitsForInitialPickerPaint(t *testing.T) {
	session, commands := runTrustPromptSequence(t, ProgramCodex,
		codexUpdatePickerHeaderAndURL,
		codexUpdatePickerHeaderAndURL,
		codexCurrentUpdatePicker,
		codexCurrentUpdatePickerSkipSelected,
	)

	require.True(t, session.CheckAndHandleTrustPrompt())
	require.Empty(t, sentKeystrokes(*commands), "the first partial frame authorizes no input")
	require.True(t, session.CheckAndHandleTrustPrompt())
	require.Empty(t, sentKeystrokes(*commands), "a persistent partial frame still authorizes no input")
	require.True(t, session.CheckAndHandleTrustPrompt())
	require.Equal(t, []string{
		"tmux send-keys -t =af_trust: Down",
		"tmux send-keys -t =af_trust: Enter",
	}, sentKeystrokes(*commands), "navigation may proceed only after the selected row becomes observable")
}

func TestCheckAndHandleTrustPrompt_CodexUpdateTextWithVisibleCursorIsNotModal(t *testing.T) {
	session, commands := runTrustPromptFrames(t, ProgramCodex,
		trustPromptFrame{content: codexUpdatePicker, cursorVisible: true},
	)

	require.False(t, session.CheckAndHandleTrustPrompt(),
		"quoted picker text in a live composer must not receive daemon input")
	require.Empty(t, sentKeystrokes(*commands))
}

func TestCheckAndHandleTrustPrompt_CodexPersistentUpdateBannerIsNotModal(t *testing.T) {
	for _, cursorVisible := range []bool{true, false} {
		name := "hidden cursor"
		if cursorVisible {
			name = "visible cursor"
		}
		t.Run(name, func(t *testing.T) {
			session, commands := runTrustPromptFrames(t, ProgramCodex,
				trustPromptFrame{content: codexPersistentUpdateBanner, cursorVisible: cursorVisible},
			)

			require.False(t, session.CheckAndHandleTrustPrompt(),
				"the persistent post-dismissal banner above a live composer is not a modal")
			require.Empty(t, sentKeystrokes(*commands))
		})
	}
}

func TestCheckAndHandleTrustPrompt_CodexUpdateConfirmsOnlyPreselectedSkip(t *testing.T) {
	handled, commands := runTrustPromptCheck(t, ProgramCodex, codexUpdatePickerSkipSelected)

	require.True(t, handled)
	require.Equal(t, []string{
		"tmux send-keys -t =af_trust: Enter",
	}, sentKeystrokes(commands), "Enter is safe only after the captured picker names Skip as selected")
}

func TestCheckAndHandleTrustPrompt_CodexUpdateHandsOffToDirectoryTrust(t *testing.T) {
	session, commands := runTrustPromptSequence(t, ProgramCodex,
		codexUpdatePicker,
		codexUpdatePickerSkipSelected,
		codexDirectoryTrustDialog,
	)

	require.True(t, session.CheckAndHandleTrustPrompt())
	require.True(t, session.CheckAndHandleTrustPrompt(),
		"the next recognized modal must reach its own handler despite sharing a hidden cursor")
	require.Equal(t, []string{
		"tmux send-keys -t =af_trust: Down",
		"tmux send-keys -t =af_trust: Enter",
		"tmux send-keys -t =af_trust: Enter",
	}, sentKeystrokes(*commands), "the second Enter belongs to the selected directory-trust affirmative")
}

func TestCheckAndHandleTrustPrompt_CodexUpdateDoesNotRepeatConfirmation(t *testing.T) {
	session, commands := runTrustPromptFrames(t, ProgramCodex,
		trustPromptFrame{content: codexUpdatePicker},
		trustPromptFrame{content: codexUpdatePickerSkipSelected},
		trustPromptFrame{content: codexUpdatePickerSkipSelected}, // stale after Enter
		trustPromptFrame{content: "› ", cursorVisible: true},
	)

	require.True(t, session.CheckAndHandleTrustPrompt())
	require.True(t, session.CheckAndHandleTrustPrompt(),
		"a stale post-confirmation frame must remain blocked without another Enter")
	require.Equal(t, []string{
		"tmux send-keys -t =af_trust: Down",
		"tmux send-keys -t =af_trust: Enter",
	}, sentKeystrokes(*commands))
	require.False(t, session.CheckAndHandleTrustPrompt(),
		"a positively visible composer releases the confirmed picker state")
}

// TestEnsureCodexUpdateCheckDisabled is the shared-helper contract both launch
// paths depend on (#5238): injectSystemPrompt for ordinary sessions and the
// config-agent spawn both route codex's launch command through here so its
// startup update picker never renders — and so the two can never drift on how
// it is suppressed (the #729/#2097/#2416 class, same as the devin helper this
// mirrors).
//
// The cases mirror what TestInjectSystemPrompt_Codex already pins for the
// session path, asserted here against the function itself so the config-agent
// path is covered by the same contract rather than a copy of it.
func TestEnsureCodexUpdateCheckDisabled(t *testing.T) {
	const arg = "-c " + codexUpdateCheckConfigKey + "=false"
	tests := []struct {
		name    string
		command string
		want    string
	}{
		{"bare codex gets the flag", "codex", "codex " + arg},
		{
			"abs-path codex gets the flag",
			"/home/u/.local/bin/codex",
			"/home/u/.local/bin/codex " + arg,
		},
		{
			// The flag lands right after the executable — codex's global -c
			// position — so it is an option for every command shape, not
			// stranded at the tail (#5238 review).
			"existing flags keep the flag in the global position",
			"codex --model gpt-5 --full-auto",
			"codex " + arg + " --model gpt-5 --full-auto",
		},
		{
			// The flag precedes an existing resume subcommand; codex parses
			// global -c before any subcommand (0.163.0). af's own restore
			// splice lands `resume --last` behind the flag, in this same
			// global position (#5238 review).
			"a restore's resume --last keeps the flag global",
			"codex resume --last",
			"codex " + arg + " resume --last",
		},
		{
			"a recorded conversation resume keeps the flag global",
			"codex resume 0a0b0c0d-1e2f-4a3b-8c4d-5e6f7a8b9c0d",
			"codex " + arg + " resume 0a0b0c0d-1e2f-4a3b-8c4d-5e6f7a8b9c0d",
		},
		{
			"codex exec keeps its own -c scope",
			"codex exec --json",
			"codex " + arg + " exec --json",
		},
		// Wrappers the account boundary and findAgentToken both model: the
		// flag belongs to codex's argv, so it lands after the codex operand —
		// an append would hand it to the wrapper or strand it after `--`
		// (#5238 review).
		{
			"an exec wrapper carries the flag on codex's argv",
			"exec codex",
			"exec codex " + arg,
		},
		{
			// Trailing codex args are the shape that proves the position: an
			// append would leave the flag at the command's tail instead of
			// codex's own option position.
			"an exec wrapper's flags keep the flag on codex's argv",
			"exec codex --full-auto",
			"exec codex " + arg + " --full-auto",
		},
		{
			"an env wrapper carries the flag on codex's argv",
			"env codex",
			"env codex " + arg,
		},
		{
			"an env assignment wrapper carries the flag on codex's argv",
			"env FOO=1 codex",
			"env FOO=1 codex " + arg,
		},
		{
			"an env assignment wrapper's flags keep the flag on codex's argv",
			"env FOO=1 codex --full-auto",
			"env FOO=1 codex " + arg + " --full-auto",
		},
		// Only the wrapper grammar af models — assignments, a bare `exec`, an
		// `env` invocation — places the executable. Any other first word IS
		// the executable, so a later token that names codex is an argument of
		// a program af cannot prove is codex: splicing `-c` there hands the
		// flag to an arbitrary binary that may reject the launch (#5238
		// review). Those commands stay untouched — the update check runs and
		// af answers the picker, exactly as master does.
		{
			"a codex argument is not the executable",
			"my-chat --persona codex",
			"my-chat --persona codex",
		},
		{
			"a relative-path wrapper's codex argument is not the executable",
			"./collect codex",
			"./collect codex",
		},
		{
			"an unmodeled wrapper's codex operand is not the executable",
			"sudo codex",
			"sudo codex",
		},
		{
			// ionice's `-c` is its scheduling-class option, not codex's —
			// and ionice is not a wrapper af models, so the command is left
			// alone entirely.
			"an unmodeled wrapper's own -c changes nothing",
			"ionice -c 3 codex",
			"ionice -c 3 codex",
		},
		{
			"an unmodeled wrapper's -c operand naming the key changes nothing",
			"ionice -c check_for_update_on_startup=x codex",
			"ionice -c check_for_update_on_startup=x codex",
		},
		{
			// `exec --` is the separator dash cannot run (#3557) — the
			// boundary refuses it and so does the locator.
			"the non-portable exec separator is not modeled",
			"exec -- codex",
			"exec -- codex",
		},
		{
			// After `exec`, the next word is the program exec will run —
			// assignment-looking text is its NAME, not an environment pair.
			"exec's operand is the executable even when it looks assigned",
			"exec FOO=1 codex",
			"exec FOO=1 codex",
		},
		{
			// env's command operand is the executable — `my-chat` here —
			// and `codex` is just its argument.
			"env's operand is the executable, not its codex argument",
			"env my-chat codex",
			"env my-chat codex",
		},
		{
			"an assignment prefix still reaches the modeled executable",
			"FOO=1 exec codex",
			"FOO=1 exec codex " + arg,
		},
		// End-of-options: a `--` makes the rest a positional prompt. The flag
		// must land before it or clap reads it as prompt text and rejects the
		// launch (#5238 review) — and post-`--` operands are not an override.
		{
			"an option terminator keeps the flag in option position",
			"codex -- '-review this'",
			"codex " + arg + " -- '-review this'",
		},
		{
			"the key after -- is prompt text, not an override",
			"codex -- -c check_for_update_on_startup=true",
			"codex " + arg + " -- -c check_for_update_on_startup=true",
		},
		// A command already carrying the key — either value — is left alone,
		// so a user's explicit choice wins: that is the opt-back-in.
		{
			"an explicit false is left alone",
			"codex -c check_for_update_on_startup=false",
			"codex -c check_for_update_on_startup=false",
		},
		{
			"an explicit true wins",
			"codex -c check_for_update_on_startup=true",
			"codex -c check_for_update_on_startup=true",
		},
		{
			"the key inside a quoted -c value wins",
			`codex -c 'check_for_update_on_startup = true'`,
			`codex -c 'check_for_update_on_startup = true'`,
		},
		{
			"a --config assignment is left alone",
			"codex --config check_for_update_on_startup=true",
			"codex --config check_for_update_on_startup=true",
		},
		{
			"a --config=key assignment is left alone",
			"codex --config=check_for_update_on_startup=true",
			"codex --config=check_for_update_on_startup=true",
		},
		{
			"a glued -ckey assignment is left alone",
			"codex -ccheck_for_update_on_startup=false",
			"codex -ccheck_for_update_on_startup=false",
		},
		{
			"a -c=key assignment is left alone",
			"codex -c=check_for_update_on_startup=false",
			"codex -c=check_for_update_on_startup=false",
		},
		// The key's NAME appearing anywhere else proves nothing: an env
		// assignment's value, another key's operand, or a script name that
		// happens to contain it are not the operator choosing this override —
		// a substring match would leave these launchable commands exposed to
		// the picker (#5238 review).
		{
			"the key inside another -c operand is not the override",
			"codex -c model=check_for_update_on_startup",
			"codex " + arg + " -c model=check_for_update_on_startup",
		},
		{
			"the key inside an env assignment is not the override",
			"CODEX_HOME=/tmp/check_for_update_on_startup codex",
			"CODEX_HOME=/tmp/check_for_update_on_startup codex " + arg,
		},
		{
			"the key inside another --config operand is not the override",
			"codex --config sandbox_mode=check_for_update_on_startup",
			"codex " + arg + " --config sandbox_mode=check_for_update_on_startup",
		},
		{
			"a dangling -c at the end still gets the flag",
			"codex --model gpt-5 -c",
			"codex " + arg + " --model gpt-5 -c",
		},
		// A program_overrides entry can point "codex" at another binary
		// (#1116/#1131); appending a codex flag would make that binary exit on
		// an unknown argument.
		{"a non-codex command is unchanged", "bash", "bash"},
		{"a claude command is unchanged", "claude --dangerously-skip-permissions", "claude --dangerously-skip-permissions"},
		{"empty is unchanged", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EnsureCodexUpdateCheckDisabled(tt.command); got != tt.want {
				t.Errorf("EnsureCodexUpdateCheckDisabled(%q) = %q, want %q", tt.command, got, tt.want)
			}
			// Whatever the path, the key must never end up in codex's own
			// OPTION position twice — a second -c carrying it is the override
			// ambiguity the conditional exists to prevent. Count only tokens
			// after the executable and before any `--`: a wrapper's operand or
			// prompt text may legitimately spell the key without being the
			// option being duplicated.
			got := EnsureCodexUpdateCheckDisabled(tt.command)
			gotTokens, _ := splitShellTokens(got)
			gotIdx, _ := findAgentToken(gotTokens)
			seen := 0
			for _, tok := range gotTokens[gotIdx+1:] {
				if tok == "--" {
					break
				}
				seen += strings.Count(tok, codexUpdateCheckConfigKey+"=")
			}
			if seen > 1 {
				t.Errorf("EnsureCodexUpdateCheckDisabled(%q) = %q carries %q more than once in option position", tt.command, got, codexUpdateCheckConfigKey)
			}
		})
	}
}

// `-c check_for_update_on_startup=true` is the documented opt-back-in (#5238
// review): codex runs its startup check because the operator asked for it —
// and a picker that renders still gets af's ordinary dismissal, the same
// Down-then-Enter on Skip master sends for a launch the flag never touched.
// Holding the modal open for a human would spin DismissTrustPrompt to its
// attempt budget and fail the launch, and a config agent has nobody to
// answer it anyway (#5238 review).
func TestCheckAndHandleTrustPrompt_CodexUpdateOptedInStillAnswersSkip(t *testing.T) {
	for _, program := range []string{
		"codex -c check_for_update_on_startup=true",
		"codex -ccheck_for_update_on_startup=true",
		"codex -c=check_for_update_on_startup=true",
		"codex --config check_for_update_on_startup=true",
		"codex --config=check_for_update_on_startup=true",
	} {
		t.Run(program, func(t *testing.T) {
			session, commands := runTrustPromptSequence(t, program,
				codexCurrentUpdatePicker, codexCurrentUpdatePickerSkipSelected)

			require.True(t, session.CheckAndHandleTrustPrompt())
			require.Equal(t, []string{
				"tmux send-keys -t =af_trust: Down",
				"tmux send-keys -t =af_trust: Enter",
			}, sentKeystrokes(*commands),
				"an opted-in picker is answered like any other: Down to Skip, Enter — never held for a human")
		})
	}
}

// `=false` is the operator's explicit choice too, and a picker that renders
// anyway gets the same fallback navigation — the program's words change what
// codex does, never what af's dismisser does.
func TestCheckAndHandleTrustPrompt_CodexUpdateExplicitFalseStillAnswersSkip(t *testing.T) {
	session, commands := runTrustPromptSequence(t, "codex -c check_for_update_on_startup=false",
		codexCurrentUpdatePicker, codexCurrentUpdatePickerSkipSelected)

	require.True(t, session.CheckAndHandleTrustPrompt())
	require.Equal(t, []string{
		"tmux send-keys -t =af_trust: Down",
		"tmux send-keys -t =af_trust: Enter",
	}, sentKeystrokes(*commands),
		"the fallback dismisser navigates a rendered picker regardless of the command's -c words")
}
