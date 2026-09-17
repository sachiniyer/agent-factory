package app

import (
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/session"
)

// recordRenameTab stubs the daemon RenameTab seam and captures the requests the
// TUI sends, so tests assert the RPC carried the identity rather than a title
// the prompt happened to be opened over.
func recordRenameTab(t *testing.T, resolved string) (*[]daemon.RenameTabRequest, func()) {
	t.Helper()
	var calls []daemon.RenameTabRequest
	restore := SetTabRenamerForTest(func(req daemon.RenameTabRequest) (string, error) {
		calls = append(calls, req)
		return resolved, nil
	})
	t.Cleanup(restore)
	return &calls, restore
}

// typeIntoPrompt feeds runes through the prompt overlay the way a typing user
// does: each KeyRunes message is text, not a control key.
func typeIntoPrompt(h *home, text string) {
	for _, r := range text {
		h.handleStateRenameTab(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

// TestRenameTabRoutesThroughDaemon is the TUI half of #1904's rename verb: `R`
// on a renameable tab opens a prompt, Enter sends the daemon's RenameTab RPC
// addressed by stable ids, and the RESOLVED name lands on the local projection.
func TestRenameTabRoutesThroughDaemon(t *testing.T) {
	h := newTestHome(t)
	inst := freshLocalInstance(t, "rename-route")
	inst.AddWebTabForTest("web", "https://example.com")
	selectInstance(h, inst)
	resizeHome(h, 200, 40)
	h.store.SetActiveTab(1)
	tab := inst.GetTabs()[1]
	require.NotEmpty(t, tab.ID, "the fixture tab must carry the stable id the request relies on")

	calls, _ := recordRenameTab(t, "preview")
	_, _ = h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'R'}}, keys.KeyRenameTab)
	require.Equal(t, stateRenameTab, h.state)
	require.NotNil(t, h.promptOverlay)
	require.Equal(t, "web", h.promptOverlay.Value(), "the prompt seeds the tab's current name for editing")

	typeIntoPrompt(h, "-preview")
	_, _ = h.handleStateRenameTab(tea.KeyMsg{Type: tea.KeyEnter})

	require.Equal(t, stateDefault, h.state)
	require.Len(t, *calls, 1, "the rename must cross the daemon RenameTab seam")
	req := (*calls)[0]
	require.Equal(t, inst.ID, req.ID)
	require.Equal(t, inst.Title, req.Title)
	require.Equal(t, h.repoID, req.RepoID)
	require.Equal(t, tab.ID, req.TabID, "the tab is addressed by its stable id, not the reusable name")
	require.Equal(t, "web", req.TabName)
	require.Equal(t, "web-preview", req.NewName)
	require.Equal(t, "preview", inst.GetTabs()[1].Name,
		"the local projection adopts the daemon's resolved name before the next snapshot")
}

// TestRenameTabAgentAndShellRefused: the daemon refuses kinds that never
// display a name, so the TUI refuses before opening the prompt — with the same
// reason the RPC would give, not a bare no-op.
func TestRenameTabAgentAndShellRefused(t *testing.T) {
	h := newTestHome(t)
	inst := startedLocalInstance(t, "rename-refused")
	selectInstance(h, inst)
	resizeHome(h, 200, 40)
	calls, _ := recordRenameTab(t, "unused")

	h.store.SetActiveTab(0)
	_, _ = h.showRenameTabPrompt()
	require.Nil(t, h.promptOverlay, "the agent tab must not open the prompt")
	require.NotEqual(t, stateRenameTab, h.state)
	h.errBox.SetSize(200, 1)
	require.Contains(t, h.errBox.String(), "agent tab can't be renamed")

	h.store.SetActiveTab(1)
	_, _ = h.showRenameTabPrompt()
	require.Nil(t, h.promptOverlay, "a shell tab must not open the prompt — it always displays as Terminal")
	h.errBox.SetSize(200, 1)
	require.Contains(t, h.errBox.String(), "Terminal")
	require.Empty(t, *calls)
}

// TestRenameTabCancelKeepsName: Esc closes the prompt without a daemon call and
// without touching the roster.
func TestRenameTabCancelKeepsName(t *testing.T) {
	h := newTestHome(t)
	inst := freshLocalInstance(t, "rename-cancel")
	inst.AddWebTabForTest("web", "https://example.com")
	selectInstance(h, inst)
	h.store.SetActiveTab(1)
	calls, _ := recordRenameTab(t, "unused")

	_, _ = h.showRenameTabPrompt()
	typeIntoPrompt(h, "-x")
	_, _ = h.handleStateRenameTab(tea.KeyMsg{Type: tea.KeyEsc})

	require.Equal(t, stateDefault, h.state)
	require.Empty(t, *calls)
	require.Equal(t, "web", inst.GetTabs()[1].Name)
}

// TestRenameTabEmptySubmissionAbandons: Enter on a cleared prompt is how a user
// backs out of a field opened by accident — it must not reach the daemon, which
// would only answer "no usable characters".
func TestRenameTabEmptySubmissionAbandons(t *testing.T) {
	h := newTestHome(t)
	inst := freshLocalInstance(t, "rename-empty")
	inst.AddWebTabForTest("web", "https://example.com")
	selectInstance(h, inst)
	h.store.SetActiveTab(1)
	calls, _ := recordRenameTab(t, "unused")

	_, _ = h.showRenameTabPrompt()
	// Select-all-delete: overwrite the seeded name with nothing.
	for range len("web") {
		h.handleStateRenameTab(tea.KeyMsg{Type: tea.KeyBackspace})
	}
	_, _ = h.handleStateRenameTab(tea.KeyMsg{Type: tea.KeyEnter})

	require.Equal(t, stateDefault, h.state)
	require.Empty(t, *calls)
	require.Equal(t, "web", inst.GetTabs()[1].Name)
}

// TestRenameTabDoesNotTargetSameTitleReplacement: the prompt retains intent
// about one session while its modal owns the keyboard. A different session that
// reuses the title in that window must not inherit the pending rename — the
// captured stable id is the whole guard (#2322's class, applied one level down).
func TestRenameTabDoesNotTargetSameTitleReplacement(t *testing.T) {
	h := newTestHome(t)
	stale := freshLocalInstance(t, "rename-stale")
	stale.AddWebTabForTest("web", "https://example.com")
	selectInstance(h, stale)
	h.store.SetActiveTab(1)
	_, _ = h.showRenameTabPrompt()

	replacement := freshLocalInstanceNamed(t, stale.Title)
	replacement.AddWebTabForTest("web", "https://example.com")
	require.NotEqual(t, stale.ID, replacement.ID)
	require.True(t, h.store.ReplaceInstanceByTitle(stale.Title, replacement))
	calls, _ := recordRenameTab(t, "unused")

	_, _ = h.handleStateRenameTab(tea.KeyMsg{Type: tea.KeyEnter})

	require.Empty(t, *calls, "a stale prompt target must fail closed before the daemon request")
	require.Equal(t, "web", replacement.GetTabs()[1].Name,
		"the replacement's tab must not inherit the pending rename")
	h.errBox.SetSize(200, 1)
	require.Contains(t, h.errBox.String(), "no longer available")
}

// TestRenameTabSurvivesSameIdentityRebuild: a pointer rebuild of the SAME
// session (same stable id) keeps the captured target, so the rename reaches the
// daemon addressed by id — mirroring TestNewTabPickerFollowsSameIdentitySnapshotRebuild.
func TestRenameTabSurvivesSameIdentityRebuild(t *testing.T) {
	h := newTestHome(t)
	stale := freshLocalInstance(t, "rename-rebuilt")
	stale.AddWebTabForTest("web", "https://example.com")
	selectInstance(h, stale)
	h.store.SetActiveTab(1)
	tabID := stale.GetTabs()[1].ID
	_, _ = h.showRenameTabPrompt()

	rebuilt := freshLocalInstanceNamed(t, stale.Title)
	rebuilt.ID = stale.ID
	rebuilt.CreatedAt = stale.CreatedAt
	rebuilt.AddWebTabForTest("web", "https://example.com")
	rebuilt.GetTabs()[1].ID = tabID
	require.True(t, h.store.ReplaceInstanceByTitle(stale.Title, rebuilt))
	calls, _ := recordRenameTab(t, "docs")

	typeIntoPrompt(h, "-docs")
	_, _ = h.handleStateRenameTab(tea.KeyMsg{Type: tea.KeyEnter})

	require.Len(t, *calls, 1)
	require.Equal(t, stale.ID, (*calls)[0].ID,
		"a pointer rebuild of the same session retains the captured stable ID")
	require.Equal(t, tabID, (*calls)[0].TabID)
	require.Equal(t, "docs", rebuilt.GetTabs()[1].Name)
}

// TestRenameTabIdLessRosterGenerationGuard: a pre-#1738 tab has no stable id,
// so the captured name is only trusted while the roster provably has not
// changed — the same rule the delete consent applies. A snapshot that lands
// while the prompt is open turns the submit into a refusal, never a rename of
// whatever now answers to the old name.
func TestRenameTabIdLessRosterGenerationGuard(t *testing.T) {
	h := newTestHome(t)
	inst := freshLocalInstance(t, "rename-legacy")
	inst.AddTabForTest("vscode", session.TabKindVSCode) // id-less: the legacy shape
	selectInstance(h, inst)
	h.store.SetActiveTab(1)
	_, _ = h.showRenameTabPrompt()

	data := inst.ToInstanceData()
	data.Tabs = append(data.Tabs, session.TabData{ID: "new-web", Name: "web", Kind: session.TabKindWeb})
	h.updateInstanceFromSnapshot(inst, data)

	calls, _ := recordRenameTab(t, "unused")
	_, _ = h.handleStateRenameTab(tea.KeyMsg{Type: tea.KeyEnter})

	require.Empty(t, *calls, "a changed id-less roster cannot prove the name still names the same tab")
	h.errBox.SetSize(200, 1)
	require.Contains(t, h.errBox.String(), "changed while the prompt was open")
}

// TestRenameTabDaemonErrorSurfaces: a refusal from the daemon — the name
// sanitizes to nothing, the tab vanished — reaches the user verbatim rather
// than vanishing with the prompt.
func TestRenameTabDaemonErrorSurfaces(t *testing.T) {
	h := newTestHome(t)
	inst := freshLocalInstance(t, "rename-err")
	inst.AddWebTabForTest("web", "https://example.com")
	selectInstance(h, inst)
	h.store.SetActiveTab(1)
	t.Cleanup(SetTabRenamerForTest(func(daemon.RenameTabRequest) (string, error) {
		return "", fmt.Errorf("tab name %q has no usable characters", "!!!")
	}))

	_, _ = h.showRenameTabPrompt()
	typeIntoPrompt(h, "!!!") // appended to the seeded "web" — fine; the stub refuses regardless
	_, _ = h.handleStateRenameTab(tea.KeyMsg{Type: tea.KeyEnter})

	require.Equal(t, stateDefault, h.state)
	h.errBox.SetSize(200, 1)
	require.Contains(t, h.errBox.String(), "no usable characters")
	require.Equal(t, "web", inst.GetTabs()[1].Name, "a refused rename leaves the roster alone")
}

// TestRenameTabResolvedNameNotice: when the daemon sanitizes or suffixes the
// request ("my name" -> "my-name", or "web" -> "web-2"), the status bar names
// what the tab is ACTUALLY called — the resolved name is what the other verbs
// now address it by.
func TestRenameTabResolvedNameNotice(t *testing.T) {
	h := newTestHome(t)
	inst := freshLocalInstance(t, "rename-notice")
	inst.AddWebTabForTest("web", "https://example.com")
	inst.AddWebTabForTest("web-2", "https://example.com/2")
	selectInstance(h, inst)
	h.store.SetActiveTab(1)
	calls, _ := recordRenameTab(t, "web-2")

	typeIntoPrompt(h, "-2")
	_, _ = h.handleStateRenameTab(tea.KeyMsg{Type: tea.KeyEnter})

	require.Len(t, *calls, 1)
	h.errBox.SetSize(200, 1)
	require.Contains(t, h.errBox.String(), "web-2",
		"the notice reports the resolved name, not the typed one")
}
