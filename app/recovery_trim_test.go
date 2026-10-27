package app

import (
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

// A failed create that carried a whitespace-only prompt must reopen the
// naming form with the bare "initial prompt" hint, not its "✓" variant.
// The recovery path (recovery.go) trims the prompt the same way the overlay
// close path (handle_overlay.go), the shift+tab highlight (home_update.go),
// and the submit itself (handle_input.go) all do, so a field holding only
// whitespace is "no prompt" rather than a confirmed prompt.
func TestRecoveryTrimSpaceWhitespacePromptNoCheck(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot = "/project"
	h.termWidth, h.termHeight = 140, 36
	h.relayout()

	inst := newLoadingInstance(t, "Retained session")
	inst.Path = h.repoRoot
	h.store.AddInstance(inst)
	h.sidebar.SelectInstance(inst)
	req := sessionStartRequest{Title: inst.Title, RepoPath: h.repoRoot, Program: "claude"}

	// Simulate a create that fails after the user typed only whitespace.
	_, _ = h.Update(instanceStartedMsg{
		instance:  inst,
		draft:     &req,
		rawPrompt: "   ", // whitespace-only — trimmed to "" on submit
		err:       errors.New("daemon refused"),
	})

	require.Equal(t, "   ", h.pendingPrompt, "raw whitespace prompt retained for re-edit")
	require.Equal(t, stateNew, h.state, "naming form reopened after failed create")

	h.menu.SetSize(140, 1)
	out := ansi.Strip(h.menu.String())

	// The recovery path must NOT show the "✓" variant for a whitespace-only
	// prompt: the create flow trims whitespace to empty (no prompt attached),
	// so the hint must reflect "no prompt".
	require.NotContains(t, out, "initial prompt ✓",
		"recovery path: whitespace-only prompt must not show ✓ (it is trimmed to empty on submit)")
	require.Contains(t, out, "initial prompt",
		"recovery path: bare hint present for whitespace-only prompt")

	// The recovery path must agree with the overlay close path for the same
	// whitespace prompt value: both use strings.TrimSpace(...) != "".
	h.menu.SetNamingHasPrompt(strings.TrimSpace(h.pendingPrompt) != "")
	overlayOut := ansi.Strip(h.menu.String())
	require.Equal(t, out, overlayOut,
		"recovery path and overlay close path must render the same hint for the same whitespace prompt")
	require.NotContains(t, overlayOut, "initial prompt ✓",
		"overlay path: TrimSpace correctly hides ✓ for whitespace-only")
}

// A failed create that carried a real (non-whitespace) prompt must still
// reopen the naming form with the "✓" variant — the trim must not regress the
// common case where the prompt has visible content.
func TestRecoveryTrimSpaceRealPromptShowsCheck(t *testing.T) {
	h := newTestHome(t)
	h.repoRoot = "/project"
	h.termWidth, h.termHeight = 140, 36
	h.relayout()

	inst := newLoadingInstance(t, "Retained session")
	inst.Path = h.repoRoot
	h.store.AddInstance(inst)
	h.sidebar.SelectInstance(inst)
	req := sessionStartRequest{Title: inst.Title, RepoPath: h.repoRoot, Program: "claude"}

	// A prompt with surrounding whitespace but visible content: trims to a
	// non-empty value, so the "✓" variant is correct.
	_, _ = h.Update(instanceStartedMsg{
		instance:  inst,
		draft:     &req,
		rawPrompt: " Keep my prompt\n",
		err:       errors.New("daemon refused"),
	})

	require.Equal(t, " Keep my prompt\n", h.pendingPrompt)
	require.Equal(t, stateNew, h.state)

	h.menu.SetSize(140, 1)
	out := ansi.Strip(h.menu.String())
	require.Contains(t, out, "initial prompt ✓",
		"recovery path: real prompt (trims to non-empty) shows ✓")
}
