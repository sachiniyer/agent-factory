package app

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
)

// #1936: the TUI's create flow could not send an initial prompt. The field
// existed on sessionStartRequest and was plumbed to the daemon
// (session_control.go), but the naming form — the only place in app/ that
// builds that request — never populated it, so it was dead from the TUI while
// `af sessions create --prompt` and the web modal both had it.
//
// These tests drive the REAL key path (handleKeyPress, not the sub-handlers)
// so they cover the state dispatch and the menu-highlight hop a key must
// survive during naming — a key that opens the field in handleStateNew but is
// swallowed on the way there is exactly the bug shape this flow invites. The
// highlight RENDER for the new hint is pinned separately, in
// TestNamingKeysHighlightMenu (handle_input_test.go).

// pressFormKey feeds msg through handleKeyPress the way the Bubble Tea event loop
// does, including the re-emit hop: during naming, handleMenuHighlighting
// intercepts the form's action keys, fires the highlight, and re-emits the key
// as a message. Replaying that hop here is what makes the tests cover the
// highlight path rather than route around it. Returns every non-key message the
// press produced.
func pressFormKey(t *testing.T, h *home, msg tea.KeyMsg) []tea.Msg {
	t.Helper()
	_, cmd := h.handleKeyPress(msg)
	if cmd == nil {
		return nil
	}
	var out []tea.Msg
	for _, produced := range drainCmd(t, cmd, time.Second) {
		km, ok := produced.(tea.KeyMsg)
		if !ok {
			out = append(out, produced)
			continue
		}
		if _, replayCmd := h.handleKeyPress(km); replayCmd != nil {
			out = append(out, drainCmd(t, replayCmd, time.Second)...)
		}
	}
	return out
}

func typeRunes(t *testing.T, h *home, s string) {
	t.Helper()
	for _, r := range s {
		if r == ' ' {
			// Bubble Tea overrides the type for a space but still carries the
			// rune (key.go: "If it's a space, override the type with KeySpace
			// (but still include the rune)"). Both halves matter: the naming
			// form's title branch switches on the TYPE, the prompt field's
			// textarea reads the RUNE.
			pressFormKey(t, h, tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
			continue
		}
		pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

// startNaming puts h in the naming state with a titled instance, the shape
// startNewInstance leaves behind, without needing a real repo on disk.
func startNaming(t *testing.T, h *home, title string) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   title,
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)
	h.store.AddInstance(inst)
	h.namingInstance = inst
	h.pendingProgram = "claude"
	h.state = stateNew
	h.menu.SetState(ui.StateNewInstance)
	return inst
}

// recordStartRequest captures the sessionStartRequest the naming form submits.
func recordStartRequest(t *testing.T) *sessionStartRequest {
	t.Helper()
	var got sessionStartRequest
	t.Cleanup(SetSessionStarterForTest(func(inst *session.Instance, req sessionStartRequest) (*session.Instance, error) {
		got = req
		return inst, nil
	}))
	return &got
}

// TestNamingFormPromptReachesSessionStartRequest is the #1936 regression guard:
// a prompt typed into the naming form's initial-prompt field must arrive on the
// request the TUI hands the daemon. Before the fix this asserted "" — the field
// had no way to be populated at all.
func TestNamingFormPromptReachesSessionStartRequest(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(120, 1)
	got := recordStartRequest(t)
	startNaming(t, h, "fix-flaky-test")

	// Open the initial-prompt field, type a prompt, close it, submit the form.
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyShiftTab})
	require.Equal(t, statePromptInput, h.state,
		"shift+tab during naming must open the initial-prompt field")
	require.NotNil(t, h.promptOverlay)

	const prompt = "fix the flaky test in session/tmux"
	typeRunes(t, h, prompt)
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyTab})
	require.Equal(t, stateNew, h.state, "tab must close the field back to naming")

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})

	assert.Equal(t, prompt, got.Prompt,
		"the typed prompt must ride the sessionStartRequest to the daemon")
	// Guard the rest of the request too: a change that populated Prompt by
	// rebuilding the struct must not drop a sibling field on the way.
	assert.Equal(t, "fix-flaky-test", got.Title)
	assert.Equal(t, "claude", got.Program)
}

// TestNamingFormWithoutPromptSendsEmpty pins the unchanged default: a user who
// never opens the field submits exactly what they submitted before #1936. This
// is the half that keeps "populate Prompt" from becoming "always send
// something".
func TestNamingFormWithoutPromptSendsEmpty(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(120, 1)
	got := recordStartRequest(t)
	startNaming(t, h, "no-prompt")

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})

	assert.Empty(t, got.Prompt, "an untouched prompt field must send no prompt")
	assert.Equal(t, "no-prompt", got.Title)
}

// TestInitialPromptFieldKeepsEnterForNewlines pins the property that made the
// field textarea-backed instead of a hand-rolled string: Enter is a newline,
// not a submit. A hand-rolled buffer would read a pasted newline as submit and
// silently drop the rest of a multi-line prompt.
func TestInitialPromptFieldKeepsEnterForNewlines(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(120, 1)
	got := recordStartRequest(t)
	startNaming(t, h, "multiline")

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyShiftTab})
	typeRunes(t, h, "first line")
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, statePromptInput, h.state,
		"enter inside the prompt field must insert a newline, not close the field")
	typeRunes(t, h, "second line")
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyTab})
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})

	assert.Equal(t, "first line\nsecond line", got.Prompt,
		"both lines of a multi-line prompt must survive to the request")
}

// TestInitialPromptFieldSurvivesAMultiLinePaste is the other half of the
// textarea rationale, and the one a future refactor is most likely to break.
// Bubble Tea delivers a bracketed paste as ONE KeyRunes message whose runes are
// "not to be interpreted further" (key_sequences.go detectBracketedPaste) — so
// the tabs and newlines inside a pasted prompt must land as text. A handler
// that switched on the key STRING, or grew a KeyRunes case of its own, would
// close the field on the pasted tab and spray the remainder into the title.
func TestInitialPromptFieldSurvivesAMultiLinePaste(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(120, 1)
	got := recordStartRequest(t)
	inst := startNaming(t, h, "pasted")

	const pasted = "refactor this:\n\tif err != nil {\n\t\treturn err\n\t}"
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyShiftTab})
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyRunes, Paste: true, Runes: []rune(pasted)})

	require.Equal(t, statePromptInput, h.state,
		"a pasted tab must not close the field")
	require.Equal(t, "pasted", inst.Title,
		"no part of the paste may leak into the title")

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyTab})
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})

	// Line structure survives; the tabs arrive as four spaces each. That is
	// bubbles' textarea sanitizing its input (runeutil.NewSanitizer defaults to
	// ReplaceTabs("    ") and the sanitizer is unexported in v0.20, so it is not
	// configurable), and it is exactly what the task form's prompt field already
	// does. Indentation is preserved, no text is dropped, and nothing reaches
	// the title — the properties that matter for a pasted prompt.
	assert.Equal(t, "refactor this:\n    if err != nil {\n        return err\n    }", got.Prompt,
		"every line of the paste must reach the request, tabs widened to spaces")
	assert.Equal(t, strings.Count(pasted, "\n"), strings.Count(got.Prompt, "\n"),
		"line structure must survive the paste")
}

// TestInitialPromptFieldReopensWithItsText pins that the field is an editable
// value rather than a write-once box: reopening shows what is already attached.
func TestInitialPromptFieldReopensWithItsText(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(120, 1)
	got := recordStartRequest(t)
	startNaming(t, h, "reopen")

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyShiftTab})
	typeRunes(t, h, "review the diff")
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEsc})
	require.Equal(t, stateNew, h.state, "esc must back out of the field, not out of naming")
	require.NotNil(t, h.namingInstance, "esc in the prompt field must not cancel the create")

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyShiftTab})
	require.NotNil(t, h.promptOverlay)
	require.Equal(t, "review the diff", h.promptOverlay.Value(),
		"reopening the field must show the prompt already typed")

	typeRunes(t, h, " carefully")
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyTab})
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})

	assert.Equal(t, "review the diff carefully", got.Prompt)
}

// TestInitialPromptDoesNotLeakIntoNextCreate is the leak guard: pendingPrompt
// is home-scoped state that outlives one create, so a cancelled or submitted
// create must not hand its prompt to the next session the user makes.
func TestInitialPromptDoesNotLeakIntoNextCreate(t *testing.T) {
	cases := []struct {
		name string
		end  tea.KeyMsg
	}{
		{"submitted", tea.KeyMsg{Type: tea.KeyEnter}},
		{"cancelled with esc", tea.KeyMsg{Type: tea.KeyEsc}},
		{"cancelled with ctrl+c", tea.KeyMsg{Type: tea.KeyCtrlC}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			h.errBox.SetSize(120, 1)
			got := recordStartRequest(t)

			startNaming(t, h, "first")
			pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyShiftTab})
			typeRunes(t, h, "prompt for the first session")
			pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyTab})
			pressFormKey(t, h, tc.end)
			require.Empty(t, h.pendingPrompt,
				"leaving the naming form must clear the pending prompt")

			// The next create submits without ever opening the field.
			*got = sessionStartRequest{}
			startNaming(t, h, "second")
			pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})

			assert.Empty(t, got.Prompt,
				"the previous create's prompt must not ride the next session's request")
			assert.Equal(t, "second", got.Title)
		})
	}
}

// TestStartNewInstanceResetsThePromptField covers the authoritative half of the
// leak guard: whatever route the previous create exited by, beginning a new one
// starts from an empty prompt field. The per-exit clears above are defence in
// depth on top of this.
func TestStartNewInstanceResetsThePromptField(t *testing.T) {
	repoDir := setupRealRepo(t)
	t.Chdir(repoDir)

	h := newTestHome(t)
	h.errBox.SetSize(120, 1)
	h.pendingPrompt = "a prompt stranded by some earlier create"

	model, cmd := h.startNewInstance(false)
	require.Same(t, h, model)
	require.Nil(t, cmd)
	require.Equal(t, stateNew, h.state)

	assert.Empty(t, h.pendingPrompt,
		"beginning a create must start with an empty initial-prompt field")
}

// TestInitialPromptCtrlCCancelsTheCreate pins that ctrl+c keeps meaning "cancel
// this create" inside the field, rather than being a dead key behind a modal.
func TestInitialPromptCtrlCCancelsTheCreate(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(120, 1)
	startNaming(t, h, "abandoned")

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyShiftTab})
	typeRunes(t, h, "never mind")
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyCtrlC})

	assert.Equal(t, stateDefault, h.state, "ctrl+c must leave the naming form entirely")
	assert.Nil(t, h.namingInstance, "ctrl+c must cancel the pending create")
	assert.Nil(t, h.promptOverlay)
}

// TestWhitespaceOnlyPromptSendsNoPrompt mirrors the #973 title rule: a field
// holding only whitespace is empty, not a prompt. Without this a stray space or
// newline would be delivered to the agent as its first instruction.
func TestWhitespaceOnlyPromptSendsNoPrompt(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(120, 1)
	got := recordStartRequest(t)
	startNaming(t, h, "blank-prompt")

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyShiftTab})
	typeRunes(t, h, "  ")
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyTab})
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyEnter})

	assert.Empty(t, got.Prompt, "a whitespace-only field must send no prompt")
}

// TestNamingMenuAdvertisesThePromptField covers discoverability: the field is
// behind a modal, so the status bar is the only thing that can tell a user it
// exists — and the only confirmation that a prompt is attached before they
// press Enter.
func TestNamingMenuAdvertisesThePromptField(t *testing.T) {
	h := newTestHome(t)
	h.errBox.SetSize(200, 1)
	h.menu.SetSize(200, 3)
	startNaming(t, h, "hints")

	require.Contains(t, h.menu.String(), "initial prompt",
		"the naming form must advertise its initial-prompt field")
	require.NotContains(t, h.menu.String(), "initial prompt ✓",
		"precondition: no prompt attached yet")

	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyShiftTab})
	typeRunes(t, h, "do the thing")
	pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyTab})

	assert.Contains(t, h.menu.String(), "initial prompt ✓",
		"the hint must confirm a prompt is attached once the field holds text")
}

// TestPromptOverlayRendersInsideTheTerminal guards the #1998 overlay class: an
// overlay whose content is wider or taller than the terminal spills raw text
// over the frame. The field takes free-form prose, so it is a natural candidate.
func TestPromptOverlayRendersInsideTheTerminal(t *testing.T) {
	for _, size := range []struct{ w, h int }{{120, 40}, {80, 24}, {60, 15}} {
		h := newTestHome(t)
		h.errBox.SetSize(size.w, 1)
		startNaming(t, h, "sized")
		h.termWidth, h.termHeight = size.w, size.h

		pressFormKey(t, h, tea.KeyMsg{Type: tea.KeyShiftTab})
		h.layoutPromptOverlay()
		typeRunes(t, h, strings.Repeat("a very long prompt that keeps going ", 8))

		rendered := h.promptOverlay.Render()
		for i, line := range strings.Split(rendered, "\n") {
			require.LessOrEqual(t, lipgloss.Width(line), size.w,
				"overlay line %d overflows a %dx%d terminal", i, size.w, size.h)
		}
		require.LessOrEqual(t, strings.Count(rendered, "\n")+1, size.h,
			"overlay is taller than a %dx%d terminal", size.w, size.h)
	}
}
