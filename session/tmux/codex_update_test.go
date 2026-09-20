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
