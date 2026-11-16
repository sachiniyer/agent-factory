package app

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

// TestNamingFormPreviewClaimsSetupOnlyWhileACreateIsInFlight pins #7407's
// property: no optimistic "in progress" signal outlives the request it stood
// for. The create form's row carries OpCreating from the moment the form opens,
// so its preview said "Setting up workspace…" while the user was still typing,
// and kept saying it beside "already exists" after a refused name, and again on
// the form a daemon refusal restored. Only the interval between a submit and
// its answer is a create in flight.
func TestNamingFormPreviewClaimsSetupOnlyWhileACreateIsInFlight(t *testing.T) {
	const setup = "Setting up workspace…"
	h := activeProjectHome(t)
	resizeHome(h, 120, 30)
	existing, err := session.NewInstance(session.InstanceOptions{Title: "alpha", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	h.store.AddInstance(existing)

	preview := func(inst *session.Instance) string {
		t.Helper()
		p := h.openPaneWindow(inst, 0)
		require.NotNil(t, p)
		h.relayout()
		w := h.paneWindows[p.ID()]
		require.NoError(t, w.UpdateContent(inst))
		_ = h.View()
		out := xansi.Strip(w.String())
		require.NotEmpty(t, strings.TrimSpace(out), "precondition: the pane renders")
		return out
	}
	const namingCopy = "Please enter a name for the session."
	formPreview := func(inst *session.Instance, why string) {
		t.Helper()
		out := preview(inst)
		require.NotContains(t, out, setup, why)
		require.Contains(t, out, namingCopy, "the form's row reads as the form")
	}

	_, _ = h.startNewInstance()
	require.Equal(t, stateNew, h.state, "precondition: the form opened")
	naming := h.namingInstance
	require.NotNil(t, naming)
	require.True(t, naming.IsCreating(), "precondition: the form row carries the create op")
	formPreview(naming, "an open form has sent no request")

	// A taken name is refused before any request is sent; the form stays open.
	_, _ = h.handleStateNew(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("alpha")})
	_, _ = h.handleStateNew(tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, stateNew, h.state, "precondition: the form refuses the name and stays open")
	require.Contains(t, h.errBox.FullError(), `session with title "alpha" already exists`)
	formPreview(naming, "the refusal is the only state the form shows")

	// A fresh name submits: from here until the daemon answers, the create is
	// in flight and the optimistic copy is true.
	require.NoError(t, naming.SetTitle("beta"))
	_, _ = h.handleStateNew(tea.KeyMsg{Type: tea.KeyEnter})
	require.Equal(t, stateDefault, h.state, "precondition: the form submitted")
	require.True(t, naming.IsCreating(), "precondition: the placeholder holds the in-flight create")
	require.Contains(t, preview(naming), setup, "a create in flight reads as one")

	// The daemon refuses it while the user is still on the row. The request is
	// over; the form it restores is a form.
	h.sidebar.SelectInstance(naming)
	req := sessionStartRequest{Title: "beta", RepoPath: h.repoRoot, Program: "claude"}
	_, _ = h.Update(instanceStartedMsg{
		repoID: h.repoID, repoRoot: h.repoRoot, instance: naming, draft: &req,
		err: errors.New(`session with title "beta" already exists`),
	})
	require.Same(t, naming, h.namingInstance, "precondition: the refused draft reopened the form")
	require.True(t, naming.IsCreating(), "precondition: the restored form row carries the create op again")
	formPreview(naming, "a refused create is no longer in flight")
}
