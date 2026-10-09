package app

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
)

// #4755: the archived row the user sees as selected must be the row the verbs
// and the footer act on. The TUI keeps two selections — the sidebar tree cursor
// (nil on a section header) and the store's sticky display binding (the ▾
// marker). After `l` expands "▶ Archived (1)", the cursor stays on the header
// while the binding still marks the archived row, so a cursor-only `r` silently
// no-ops. These tests pin the composed rule: resting binding + expanded folder
// resolves to the row; live binding or collapsed folder resolves to nothing.

// archivedHeaderState arranges the issue's exact reproduction state: the
// display binding on the archived session (bound while it was live) and the
// tree cursor parked on the expanded Archived folder header.
func archivedHeaderState(t *testing.T, h *home, inst *session.Instance) {
	t.Helper()
	h.store.SelectInstance(inst)
	h.sidebar.ClickHeaderKind(ui.SectionArchived)

	sel := h.sidebar.GetSelection()
	require.True(t, sel.IsHeader, "the cursor must rest on the Archived section header")
	require.Equal(t, ui.SectionArchived, sel.Kind)
	require.Nil(t, h.sidebar.GetSelectedInstance(),
		"the cursor selection resolves nil on a header — the #4755 divergence")
	require.Same(t, inst, h.store.GetSelectedInstance(),
		"the display binding must still mark the archived row")
}

// TestHandleRestore_ExpandedArchivedHeader: `r` pressed while the cursor rests
// on the expanded Archived header must restore the ▾-marked row — the row the
// user sees as selected — rather than no-op on the header's nil cursor.
func TestHandleRestore_ExpandedArchivedHeader(t *testing.T) {
	h := newTestHome(t)
	inst := archiveActionInstance(t, "worker", session.Archived)
	h.store.AddInstance(inst)
	archivedHeaderState(t, h, inst)

	var gotRequest daemon.RestoreSessionRequest
	prev := restoreSessionThroughDaemon
	restoreSessionThroughDaemon = func(request daemon.RestoreSessionRequest) (string, error) {
		gotRequest = request
		return "/worktree/path", nil
	}
	defer func() { restoreSessionThroughDaemon = prev }()

	model, cmd := h.handleRestore()
	h = model.(*home)

	require.Equal(t, session.OpRestoring, inst.GetInFlightOp(),
		"`r` on the expanded Archived header must restore the marked row")
	require.NotNil(t, cmd, "the restore must dispatch its command")
	msg := cmd()
	require.Equal(t, inst.ID, gotRequest.ID)
	done, ok := msg.(instanceRestoredMsg)
	require.True(t, ok)
	require.Equal(t, inst.ID, done.target.id)
}

// TestHandleRestore_CollapsedArchivedHeaderIsInert: while the folder is
// collapsed no row carries the ▾ marker, so `r` must not reach the hidden
// binding — the footer is the header menu there and advertises no restore.
func TestHandleRestore_CollapsedArchivedHeaderIsInert(t *testing.T) {
	h := newTestHome(t)
	inst := archiveActionInstance(t, "worker", session.Archived)
	h.store.AddInstance(inst)
	h.store.SelectInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	h.sidebar.CollapseSection()

	sel := h.sidebar.GetSelection()
	require.True(t, sel.IsHeader, "the cursor must rest on the collapsed Archived header")
	require.Equal(t, ui.SectionArchived, sel.Kind)

	model, cmd := h.handleRestore()
	h = model.(*home)

	require.Equal(t, session.OpNone, inst.GetInFlightOp(),
		"`r` on a collapsed Archived header must not reach a hidden binding")
	require.Nil(t, cmd)
}

// TestSelectionChanged_ExpandedArchivedHeaderAdvertisesRestore: the footer must
// name the verbs that work on the ▾-marked row — `r restore`, `D delete` — and
// none of the live-instance verbs that cannot run on it.
func TestSelectionChanged_ExpandedArchivedHeaderAdvertisesRestore(t *testing.T) {
	h := newTestHome(t)
	inst := archiveActionInstance(t, "worker", session.Archived)
	h.store.AddInstance(inst)
	archivedHeaderState(t, h, inst)

	h.menu.SetSize(80, 1)
	h.selectionChanged()

	out := h.menu.String()
	require.Contains(t, out, "restore", "the footer must offer restore for the marked archived row")
	require.Contains(t, out, "delete session")
	for _, dead := range []string{"interact", "attach", "new tab", "del tab", "archive", "open pane"} {
		require.NotContains(t, out, dead, "a resting row has no live surface — %q cannot run on it", dead)
	}
}

// TestSelectionChanged_CollapsedArchivedHeaderBare: the symmetric case — with
// the folder collapsed the header keeps the plain header menu, matching the
// collapsed-header key handling.
func TestSelectionChanged_CollapsedArchivedHeaderBare(t *testing.T) {
	h := newTestHome(t)
	inst := archiveActionInstance(t, "worker", session.Archived)
	h.store.AddInstance(inst)
	h.store.SelectInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	h.sidebar.CollapseSection()

	h.menu.SetSize(80, 1)
	h.selectionChanged()

	out := h.menu.String()
	require.NotContains(t, out, "restore")
	require.NotContains(t, out, "delete session")
}

// TestHandleKill_ExpandedArchivedHeaderConfirmsBoundRow: `D` on the expanded
// Archived header must open the kill confirmation for the ▾-marked row, the
// same row the footer now advertises the verb on.
func TestHandleKill_ExpandedArchivedHeaderConfirmsBoundRow(t *testing.T) {
	h := newTestHome(t)
	inst := archiveActionInstance(t, "worker", session.Archived)
	h.store.AddInstance(inst)
	archivedHeaderState(t, h, inst)

	model, _ := h.handleKill()
	h = model.(*home)

	require.Equal(t, stateConfirm, h.state, "`D` must reach the marked archived row")
	require.NotNil(t, h.confirmationOverlay)
	require.Contains(t, h.confirmationOverlay.Render(), "worker")
}

// TestPaneSelectionHint_SkipsArchivedBinding: after the selected session is
// archived, no pane can ever show it — the "— selected: <name>" clause must not
// name it. A live selection still renders the clause.
func TestPaneSelectionHint_SkipsArchivedBinding(t *testing.T) {
	h := newTestHome(t)
	live := archiveActionInstance(t, "alpha", session.Ready)
	archived := archiveActionInstance(t, "beta", session.Archived)
	other := archiveActionInstance(t, "gamma", session.Ready)
	h.store.AddInstance(live)
	h.store.AddInstance(archived)
	h.store.AddInstance(other)

	p := openTestPane(t, h, live, 0)

	h.store.SetSelectedInstance(archived)
	require.Empty(t, h.paneSelectionHint(p),
		"an archived selection can never be what the pane shows — the hint must drop it")

	h.store.SetSelectedInstance(other)
	require.NotEmpty(t, h.paneSelectionHint(p),
		"a live selection elsewhere must still be named")
}

// TestSelectionChanged_ArchivedRowFooterCompact: the primary reported symptom —
// the cursor directly on the archived row — must render the compact resting
// menu, where `r restore` survives at a real bar width instead of shedding
// behind dead live verbs.
func TestSelectionChanged_ArchivedRowFooterCompact(t *testing.T) {
	h := newTestHome(t)
	inst := archiveActionInstance(t, "worker", session.Archived)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	require.Same(t, inst, h.sidebar.GetSelectedInstance(),
		"precondition: the cursor is on the archived row")

	h.menu.SetSize(80, 1)
	h.selectionChanged()

	out := h.menu.String()
	require.Contains(t, out, "restore", "the footer must advertise restore on an archived row")
	for _, dead := range []string{"new tab", "del tab", "1-9/g go", "interact", "attach", "archive"} {
		require.NotContains(t, out, dead, "the archived row must not offer %q", dead)
	}
}

// TestShowNewTabPicker_ArchivedRowRefuses: `t` on an archived row must not open
// a picker that can never submit — archived sessions reject tab creation — so
// the refusal notice lands instead. (The footer's compact resting menu no
// longer advertises `t` at all; this guards the raw key press.)
func TestShowNewTabPicker_ArchivedRowRefuses(t *testing.T) {
	h := newTestHome(t)
	inst := archiveActionInstance(t, "worker", session.Archived)
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)

	model, _ := h.showNewTabPicker()
	h = model.(*home)

	require.Nil(t, h.selectionOverlay, "the tab picker must not open for an archived session")
	text, _ := h.errBox.RetainedNotice()
	require.Contains(t, text, "archived",
		"the refusal must say why — restore it first")
}

// TestShowNewTabPicker_RestingRowRefuses: the compact footer withholds `t`
// from EVERY resting row, so the raw key must refuse Lost/Dead rows too —
// TabSpawnBlocked only names the archived liveness, and a picker that opens
// for a session with no runtime can never submit (#4755 review).
func TestShowNewTabPicker_RestingRowRefuses(t *testing.T) {
	for _, status := range []session.Status{session.Lost, session.Dead} {
		h := newTestHome(t)
		inst := archiveActionInstance(t, "worker", status)
		h.store.AddInstance(inst)
		h.sidebar.SetSelectedInstance(0)
		require.Same(t, inst, h.sidebar.GetSelectedInstance(),
			"precondition: the cursor is on the resting row")

		model, _ := h.showNewTabPicker()
		h = model.(*home)

		require.Nil(t, h.selectionOverlay,
			"the tab picker must not open for a %s session", status)
		text, _ := h.errBox.RetainedNotice()
		require.Contains(t, text, "restore",
			"the refusal must point at restore — the resting row's verb (status %s)", status)
	}
}
