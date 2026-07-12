package app

import (
	"context"
	"errors"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
)

// TestHandleStateNewKeySpaceWidthLimit ensures that pressing the spacebar
// while naming a new instance respects the same 32-cell width cap that
// regular character input enforces.
func TestHandleStateNewKeySpaceWidthLimit(t *testing.T) {
	h := &home{
		ctx:       context.Background(),
		state:     stateNew,
		appConfig: config.DefaultConfig(),
		errBox:    ui.NewErrBox(),
	}

	// Build an instance whose title is exactly at the 32-cell limit.
	instance, err := session.NewInstance(session.InstanceOptions{
		Title:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", // 32 characters
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)

	h.namingInstance = instance

	keyMsg := tea.KeyMsg{Type: tea.KeySpace}
	model, _ := h.handleStateNew(keyMsg)
	homeModel, ok := model.(*home)
	require.True(t, ok)

	assert.Equal(t, 32, len(homeModel.namingInstance.Title),
		"Title should remain at 32 characters, but got %d: %q",
		len(homeModel.namingInstance.Title), homeModel.namingInstance.Title)
}

// TestHandleStateNewKeySpaceUnderLimit ensures that a spacebar press is
// still accepted when the resulting title stays within the 32-cell cap.
func TestHandleStateNewKeySpaceUnderLimit(t *testing.T) {
	h := &home{
		ctx:       context.Background(),
		state:     stateNew,
		appConfig: config.DefaultConfig(),
		errBox:    ui.NewErrBox(),
	}

	instance, err := session.NewInstance(session.InstanceOptions{
		Title:   "hello",
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)

	h.namingInstance = instance

	keyMsg := tea.KeyMsg{Type: tea.KeySpace}
	model, _ := h.handleStateNew(keyMsg)
	homeModel, ok := model.(*home)
	require.True(t, ok)

	assert.Equal(t, "hello ", homeModel.namingInstance.Title)
}

func TestHandleMenuHighlightingDoesNotInterceptNamingText(t *testing.T) {
	h := newTestHome(t)
	h.state = stateNew

	cmd, returnEarly := h.handleMenuHighlighting(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})

	assert.False(t, returnEarly)
	assert.Nil(t, cmd)
}

// TestHandleMenuHighlightingNewInstanceActions is the regression guard for
// issue #691/#1413: pressing Enter, Tab, or Esc while naming a new instance must drive the
// menu highlight animation. The bug (commit f294e5b) folded stateNew into the
// early-return filter, which made the Enter→KeySubmitName / Tab→KeyChangeProgram
// remapping — and thus the highlight render path — unreachable.
func TestHandleMenuHighlightingNewInstanceActions(t *testing.T) {
	// Force a real color profile so lipgloss emits the underline escape that
	// signals a highlighted menu option; the Ascii profile used by default in
	// non-TTY test runs strips all styling and would hide the highlight.
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	// lipgloss folds the underline attribute (SGR 4) into a combined escape
	// such as "\x1b[4;38;5;99;4m" rather than a bare "\x1b[4m", so match the
	// underline parameter at the head of a sequence.
	const underline = "\x1b[4;"

	cases := []struct {
		name string
		key  tea.KeyMsg
	}{
		{"enter", tea.KeyMsg{Type: tea.KeyEnter}},
		{"tab", tea.KeyMsg{Type: tea.KeyTab}},
		{"esc", tea.KeyMsg{Type: tea.KeyEsc}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			h.state = stateNew
			h.menu.SetState(ui.StateNewInstance)
			h.menu.SetSize(200, 3)

			// Baseline: nothing highlighted before the keypress.
			require.NotContains(t, h.menu.String(), underline,
				"menu should not be highlighted before the keypress")

			cmd, returnEarly := h.handleMenuHighlighting(tc.key)

			// The keypress is intercepted so the highlight + re-emit fire.
			assert.True(t, returnEarly, "naming action keys should be intercepted during stateNew")
			assert.NotNil(t, cmd)
			assert.True(t, h.keySent, "keySent guards the re-emitted key from re-highlighting")

			// keydownCallback runs synchronously when the batch is built, so the
			// menu now renders the matching option underlined.
			assert.Contains(t, h.menu.String(), underline,
				"menu highlight render path should run for %s during stateNew", tc.name)
		})
	}
}

func TestStartNewInstanceSelectsNamingInstanceAfterSortedInsert(t *testing.T) {
	h := newTestHome(t)

	existing, err := session.NewInstance(session.InstanceOptions{
		Title:   "existing",
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)
	existing.CreatedAt = time.Now().Add(time.Hour)
	h.store.AddInstance(existing)
	h.sidebar.SetSelectedInstance(0)

	model, cmd := h.startNewInstance(false)
	require.Same(t, h, model)
	require.Nil(t, cmd)
	require.NotNil(t, h.namingInstance)

	require.Equal(t, []string{"", "existing"}, collectTitles(h.store.GetInstances()),
		"precondition: sorted insert placed the naming placeholder before the existing row")
	assert.Same(t, h.namingInstance, h.sidebar.GetSelectedInstance(),
		"sidebar highlight must track the naming placeholder after AddInstance sorts")
	assert.Same(t, h.namingInstance, h.store.GetSelectedInstance(),
		"store selection must agree with the highlighted naming placeholder")
	assert.Equal(t, stateNew, h.state)
	assert.Equal(t, session.OpCreating, h.namingInstance.GetInFlightOp(),
		"the #1350 BeginCreate chokepoint still fires exactly once when naming starts")
}

func TestStartNewRemoteWithoutHooksNoops(t *testing.T) {
	repoDir := setupRealRepo(t)
	t.Chdir(repoDir)

	h := newTestHome(t)
	h.errBox.SetSize(120, 1)

	model, cmd := h.startNewInstance(true)

	require.Same(t, h, model)
	require.Nil(t, cmd)
	assert.Equal(t, stateDefault, h.state)
	assert.Nil(t, h.namingInstance)
	assert.Equal(t, 0, h.store.NumInstances())
	assert.Empty(t, h.errBox.FullError())
}

func TestStartNewRemoteInvalidHooksStillErrors(t *testing.T) {
	repoDir := setupRealRepo(t)
	t.Chdir(repoDir)

	h := newTestHome(t)
	h.errBox.SetSize(120, 1)
	repo, err := config.CurrentRepo()
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoConfig(repo.ID, &config.RepoConfig{
		RemoteHooks: &config.RemoteHooks{
			DeleteCmd: "/bin/echo",
		},
	}))

	model, cmd := h.startNewInstance(true)

	require.Same(t, h, model)
	require.NotNil(t, cmd)
	assert.Equal(t, stateDefault, h.state)
	assert.Nil(t, h.namingInstance)
	assert.Equal(t, 0, h.store.NumInstances())
	assert.Contains(t, h.errBox.FullError(), "remote_hooks.launch_cmd")
}

// TestCancelNamingRemovesZombieAfterSelectionDrift is the regression guard for
// issue #717. While the user is naming a new instance, a background sidebar
// mutation can remove a *preceding* instance, which rebuilds the sidebar's
// visibleItems and drifts the selection off the naming row onto a section
// header. The old cancel handlers called selection-based
// sidebar.Kill(), which silently no-ops on a header — leaving the naming
// instance behind as a "Loading" zombie. The fix kills by the captured
// namingInstance pointer instead. Both cancel paths (Escape and ctrl+c) must
// remove the zombie regardless of where the selection drifted.
func TestCancelNamingRemovesZombieAfterSelectionDrift(t *testing.T) {
	cases := []struct {
		name string
		key  tea.KeyMsg
	}{
		{"escape", tea.KeyMsg{Type: tea.KeyEsc}},
		{"ctrl+c", tea.KeyMsg{Type: tea.KeyCtrlC}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			h.state = stateNew

			// A preceding instance that is NOT persisted to disk, so the next
			// sync removes it. It is not Loading, so sync is allowed to remove
			// it (Loading rows are protected as in-flight creations).
			preceding := newLoadingInstance(t, "preceding")
			preceding.SetStatusForTest(session.Running)
			h.store.AddInstance(preceding)

			// The naming instance: Loading, empty title — exactly what
			// startNewInstance creates. It is selected.
			naming, err := session.NewInstance(session.InstanceOptions{
				Title:   "",
				Path:    t.TempDir(),
				Program: "claude",
			})
			require.NoError(t, err)
			naming.SetStatusForTest(session.Loading)
			h.store.AddInstance(naming)
			h.sidebar.SetSelectedInstance(h.store.NumInstances() - 1)
			h.namingInstance = naming

			// A trailing instance gives the drift somewhere to land now that
			// the rail has no trailing Tasks/Hooks headers (#1024 PR 4).
			trailing := newLoadingInstance(t, "trailing")
			trailing.SetStatusForTest(session.Running)
			h.store.AddInstance(trailing)

			// A background mutation removes `preceding` (a daemon-owned row the
			// snapshot no longer lists). RemoveInstanceByTitle rebuilds
			// visibleItems without adjusting selectedIdx, drifting the selection
			// off the naming row — here onto the trailing instance, which a
			// selection-based cancel would wrongly target.
			h.store.RemoveInstanceByTitle("preceding")
			require.NotSame(t, naming, h.sidebar.GetSelectedInstance(),
				"precondition: selection must have drifted off the naming row")

			_, _ = h.handleStateNew(tc.key)

			assert.Equal(t, stateDefault, h.state, "cancel must return to the default state")
			assert.Nil(t, h.namingInstance, "namingInstance pointer must be cleared on cancel")
			assert.Equal(t, 1, h.store.NumInstances(),
				"the naming instance must be removed on cancel, not left as a Loading zombie; remaining titles: %v",
				collectTitles(h.store.GetInstances()))
			assert.Equal(t, "trailing", h.store.GetInstances()[0].Title,
				"cancel must kill the captured naming instance, never the drifted selection")
		})
	}
}

func TestHandleStateNewRejectsDuplicateTitle(t *testing.T) {
	h := newTestHome(t)
	h.state = stateNew
	h.errBox.SetSize(120, 1)

	existing, err := session.NewInstance(session.InstanceOptions{
		Title:   "fix-bug",
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)
	h.store.AddInstance(existing)

	naming, err := session.NewInstance(session.InstanceOptions{
		Title:   "fix-bug",
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)
	h.namingInstance = naming

	_, _ = h.handleStateNew(tea.KeyMsg{Type: tea.KeyEnter})

	assert.Equal(t, stateNew, h.state)
	require.NotNil(t, h.namingInstance)
	assert.Contains(t, h.errBox.String(), "fix-bug")
}

func TestHandleStateNewPreflightErrorKeepsNamingFlow(t *testing.T) {
	h := newTestHome(t)
	h.state = stateNew
	h.errBox.SetSize(160, 1)
	h.pendingProgram = "claude"

	preflightErr := errors.New("Claude Code is not installed or not on PATH")
	t.Cleanup(SetLocalSessionPreflightForTest(func(*config.Config, string) error {
		return preflightErr
	}))

	naming, err := session.NewInstance(session.InstanceOptions{
		Title:   "first-agent",
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)
	h.namingInstance = naming

	_, _ = h.handleStateNew(tea.KeyMsg{Type: tea.KeyEnter})

	assert.Equal(t, stateNew, h.state)
	assert.Same(t, naming, h.namingInstance)
	assert.Contains(t, h.errBox.String(), "Claude Code is not installed")
}

// TestHandleStateNewWhitespaceViaRealInput is the regression guard for #973: a
// title built entirely from spaces is non-empty (len > 0 and != ""), so the old
// len()==0 check let it through to session creation, producing an invisible name
// in the sidebar. Typing spaces and then Enter must leave the naming overlay open
// and the namingInstance pointer intact (i.e. not submitted).
func TestHandleStateNewWhitespaceViaRealInput(t *testing.T) {
	h := newTestHome(t)
	h.state = stateNew
	h.errBox.SetSize(120, 1)

	naming, err := session.NewInstance(session.InstanceOptions{
		Title:   "",
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)
	h.namingInstance = naming

	// Simulate the user typing three spaces in the naming flow: each KeySpace
	// appends " " to instance.Title (the KeySpace branch in handle_input.go).
	for i := 0; i < 3; i++ {
		_, _ = h.handleStateNew(tea.KeyMsg{Type: tea.KeySpace})
	}
	require.Equal(t, "   ", h.namingInstance.Title, "precondition: title is whitespace-only")

	// Submit with Enter — must be rejected, keeping the flow open.
	_, _ = h.handleStateNew(tea.KeyMsg{Type: tea.KeyEnter})

	assert.Equal(t, stateNew, h.state, "naming flow must stay open for whitespace-only title")
	require.NotNil(t, h.namingInstance, "naming instance must not be submitted")
	assert.Contains(t, h.errBox.String(), "title cannot be empty")
}

// TestHandleStateNewRejectsCaseVariantTitle covers #936: the naming pre-check
// must reject a case variant of an existing title (e.g. "myapp" when "MyApp"
// exists), mirroring the daemon's case-insensitive collision rule. Before the
// fix the TUI compared titles with == and would accept this, only for the
// daemon to reject it after submit.
func TestHandleStateNewRejectsCaseVariantTitle(t *testing.T) {
	cases := []struct {
		name     string
		existing string
		naming   string
	}{
		{name: "case variant (#605)", existing: "MyApp", naming: "myapp"},
		{name: "space vs dash sanitize collision (#741)", existing: "fix bug", naming: "fix-bug"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			h.state = stateNew
			h.errBox.SetSize(120, 1)

			existing, err := session.NewInstance(session.InstanceOptions{
				Title:   tc.existing,
				Path:    t.TempDir(),
				Program: "claude",
			})
			require.NoError(t, err)
			h.store.AddInstance(existing)

			naming, err := session.NewInstance(session.InstanceOptions{
				Title:   tc.naming,
				Path:    t.TempDir(),
				Program: "claude",
			})
			require.NoError(t, err)
			h.namingInstance = naming

			_, _ = h.handleStateNew(tea.KeyMsg{Type: tea.KeyEnter})

			assert.Equal(t, stateNew, h.state, "naming flow must stay open on a collision")
			require.NotNil(t, h.namingInstance, "naming instance must not be submitted")
			assert.Equal(t, tc.naming, h.namingInstance.Title)
			assert.Contains(t, h.errBox.String(), tc.existing,
				"error must name the conflicting existing session")
		})
	}
}

func TestHandleStateNewRejectsRemoteSlugCollision(t *testing.T) {
	h := newTestHome(t)
	h.state = stateNew
	h.errBox.SetSize(120, 1)

	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, _ string) (session.Backend, error) {
		if opts.ForceRemote {
			return &session.HookBackend{}, nil
		}
		return &session.LocalBackend{}, nil
	})
	defer restore()

	existing, err := session.NewInstance(session.InstanceOptions{
		Title:       "myapp",
		Path:        t.TempDir(),
		Program:     "claude",
		ForceRemote: true,
	})
	require.NoError(t, err)
	h.store.AddInstance(existing)

	naming, err := session.NewInstance(session.InstanceOptions{
		Title:       "my_app",
		Path:        t.TempDir(),
		Program:     "claude",
		ForceRemote: true,
	})
	require.NoError(t, err)
	h.namingInstance = naming

	_, _ = h.handleStateNew(tea.KeyMsg{Type: tea.KeyEnter})

	assert.Equal(t, stateNew, h.state)
	require.NotNil(t, h.namingInstance)
	assert.Equal(t, "my_app", h.namingInstance.Title)
	assert.Contains(t, h.errBox.String(), "myapp")
}

// TestNamingCreateFlow_NoDoubleTransition guards the #1350 regression: the
// naming→create flow must raise the optimistic OpCreating exactly once. When the
// naming flow began, startNewInstance already raised BeginCreate; the Enter
// confirm handler must NOT re-raise it (a second BeginCreate from OpCreating is
// an illegal edge). With the app-test panic hook installed (transition_hook_test),
// a double-transition panics — so this drives the flow from the real precondition
// (op already OpCreating) and asserts it does not.
func TestNamingCreateFlow_NoDoubleTransition(t *testing.T) {
	h := newTestHome(t)
	h.state = stateNew
	h.pendingProgram = "claude"
	t.Cleanup(SetLocalSessionPreflightForTest(func(*config.Config, string) error { return nil }))

	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "valid-title",
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)
	// Precondition: startNewInstance already raised the optimistic create op.
	require.NoError(t, inst.Transition(session.BeginCreate()))
	require.Equal(t, session.OpCreating, inst.GetInFlightOp())
	h.namingInstance = inst

	// A second BeginCreate here would panic via the app illegal-transition hook.
	assert.NotPanics(t, func() {
		_, _ = h.handleStateNew(tea.KeyMsg{Type: tea.KeyEnter})
	})

	// Op raised exactly once — still Creating (the daemon start is deferred to the
	// returned async cmd, which this test does not run).
	assert.Equal(t, session.OpCreating, inst.GetInFlightOp())
}
