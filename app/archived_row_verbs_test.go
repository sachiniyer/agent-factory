package app

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout/zones"
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

// TestPaneSelectionHint_SkipsRestoringBinding: MarkRestoring keeps liveness
// LiveArchived while raising OpRestoring — ShownArchived flips false the
// moment restore starts, but no pane can show the session until the daemon's
// restore lands a live tmux. The hint must stay suppressed through the whole
// restore window, so it gates on liveness, not the render predicate (#4755
// review).
func TestPaneSelectionHint_SkipsRestoringBinding(t *testing.T) {
	h := newTestHome(t)
	live := archiveActionInstance(t, "alpha", session.Ready)
	restoring := archiveActionInstance(t, "beta", session.Archived)
	h.store.AddInstance(live)
	h.store.AddInstance(restoring)

	p := openTestPane(t, h, live, 0)

	h.store.SetSelectedInstance(restoring)
	require.Empty(t, h.paneSelectionHint(p), "precondition: archived binding hidden")

	restoring.SetInFlightOpForTest(session.OpRestoring)
	require.False(t, restoring.ShownArchived(),
		"precondition: the eager re-home flips ShownArchived during OpRestoring")
	require.Empty(t, h.paneSelectionHint(p),
		"the restore window still owns no pane surface — keep the hint hidden")
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

// TestShowNewTabPicker_FencedRestingRowRefuses: a startup-unknown Lost row has
// LifecycleAction None — the lifecycle gate alone lets `t` through even though
// no runtime exists — so the refusal must read resting liveness (#4755
// review).
func TestShowNewTabPicker_FencedRestingRowRefuses(t *testing.T) {
	h := newTestHome(t)
	inst := archiveActionInstance(t, "worker", session.Lost)
	inst.MarkStartupStateUnknown()
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	require.Equal(t, session.LifecycleActionNone, inst.LifecycleAction(),
		"precondition: the startup-unknown fence reports no lifecycle verb")

	model, _ := h.showNewTabPicker()
	h = model.(*home)

	require.Nil(t, h.selectionOverlay,
		"the tab picker must not open for a fenced resting row")
	text, _ := h.errBox.RetainedNotice()
	require.Contains(t, text, "restore",
		"the refusal must point at restore — the resting row's verb")
}

// capsBackend wraps a backend with an explicit capability set — used to pin
// the refusal-precedence test without dragging a real remote backend into the
// fixture.
type capsBackend struct {
	session.Backend
	caps session.Capabilities
}

func (b capsBackend) Capabilities() session.Capabilities { return b.caps }

// TestPaneSelectionHint_LostBindingKeepsDivergenceHint: a Lost (or Dead) row
// can still own a preview pane — ui/tab_pane.go renders their fallback
// content — so when the workspace pane shows a different session, the
// — selected: hint is an honest divergence marker and must stay (#4755
// review). Only archived/restoring bindings (no pane surface, ever) suppress
// it.
func TestPaneSelectionHint_LostBindingKeepsDivergenceHint(t *testing.T) {
	h := newTestHome(t)
	live := archiveActionInstance(t, "alpha", session.Ready)
	lost := archiveActionInstance(t, "beta", session.Lost)
	h.store.AddInstance(live)
	h.store.AddInstance(lost)

	p := openTestPane(t, h, live, 0)

	h.store.SetSelectedInstance(lost)
	require.NotEmpty(t, h.paneSelectionHint(p),
		"a lost selection diverging from the pane is real — keep the hint")
}

// TestTabPickerRefusal_PrefersCapabilityGate: on a backend that can never
// manage tabs (docker/ssh/hook), a resting row's refusal must name the
// permanent reason — "only local sessions" — not "restore it first", which
// would promise a restore can enable `t` when it cannot (#4755 review).
func TestTabPickerRefusal_PrefersCapabilityGate(t *testing.T) {
	inst := archiveActionInstance(t, "remote-arch", session.Archived)
	inst.SetBackend(capsBackend{Backend: session.NewFakeBackend(),
		caps: session.Capabilities{TabManagement: false}})

	err := tabPickerRefusal(inst)
	require.Error(t, err)
	require.Contains(t, err.Error(), "only local sessions",
		"the permanent capability refusal must outrank the restore hint")
}

// restingFoldHome arranges the #5259 reproduction: the display binding on the
// OLDEST archived row — the newest-first folder's tail — with the tree cursor
// parked on the expanded Archived folder header. A tall terminal keeps the
// bound row inside the fitted window; a short one pushes it below the fold
// with no cursor move and no row-list rebuild.
func restingFoldHome(t *testing.T) (*home, *session.Instance) {
	t.Helper()
	h := newTestHome(t)
	h.store.AddInstance(archiveActionInstance(t, "live-one", session.Ready))
	var archivedRows []*session.Instance
	for i := 0; i < 12; i++ {
		inst := archiveActionInstance(t, fmt.Sprintf("arch-%02d", i), session.Archived)
		archivedRows = append(archivedRows, inst)
		h.store.AddInstance(inst)
	}
	bound := archivedRows[0]
	archivedHeaderState(t, h, bound)
	return h, bound
}

// TestResize_RestFooterDropsOffscreenBoundRow is the #5259 shrink direction: a
// resize that moves the header-adopted resting row below the fold must drop
// the footer's `r`/`D` hints (and their click zones) in the SAME update —
// pre-fix the menu kept the stale target until the next selectionChanged,
// ~100ms of the footer naming verbs for a row no longer on screen.
func TestResize_RestFooterDropsOffscreenBoundRow(t *testing.T) {
	h, bound := restingFoldHome(t)

	resizeHome(h, 80, 40)
	require.Same(t, bound, h.sidebar.RowVerbTarget(),
		"precondition: the bound tail row fits at the tall layout")
	_ = h.selectionChanged()
	require.Contains(t, h.menu.String(), "restore",
		"precondition: the footer advertises the bound row's verb")

	resizeHome(h, 80, 15)
	require.Nil(t, h.sidebar.RowVerbTarget(),
		"precondition: the bound row is off-screen at the short layout")
	out := h.menu.String()
	require.NotContains(t, out, "restore",
		"the footer must drop the off-screen row's verb in the same update")
	require.NotContains(t, out, "delete session")

	// The hint's click zone dies with it — zones register per rendered hint.
	_ = h.View()
	_, ok := h.zones.Find(zones.StatusHint("r"))
	require.False(t, ok, "the stale restore hint must not keep a live click zone")
}

// TestResize_RestFooterAdoptsBoundRowOnGrow is the symmetric grow direction:
// when a resize brings the bound resting row back inside the fitted window,
// the footer must name its verbs in the same update — not after the next
// ~100ms preview tick runs selectionChanged.
func TestResize_RestFooterAdoptsBoundRowOnGrow(t *testing.T) {
	h, bound := restingFoldHome(t)

	resizeHome(h, 80, 15)
	require.Nil(t, h.sidebar.RowVerbTarget(),
		"precondition: the bound tail row starts below the fold")
	_ = h.selectionChanged()
	out := h.menu.String()
	require.NotContains(t, out, "restore",
		"precondition: nothing is advertised while the bound row is off-screen")
	require.NotContains(t, out, "delete session")

	resizeHome(h, 80, 40)
	require.Same(t, bound, h.sidebar.RowVerbTarget(),
		"precondition: the bound row is back inside the fitted window")
	out = h.menu.String()
	require.Contains(t, out, "restore",
		"the footer must pick the bound row's verb back up in the same update")
	require.Contains(t, out, "delete session")
}

// TestSelectionChanged_RestFooterOpenPaneByLiveness is the #5260 truth table
// at the app seam: Lost and Dead rows still own a pane surface — tab_pane.go
// renders their fallback content and `s` opens it — so the compact resting
// footer must name it; an archived row's panes are pruned on sight, so the
// key stays withheld there.
func TestSelectionChanged_RestFooterOpenPaneByLiveness(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    session.Status
		wantOffer bool
	}{
		{name: "archived", status: session.Archived, wantOffer: false},
		{name: "lost", status: session.Lost, wantOffer: true},
		{name: "dead", status: session.Dead, wantOffer: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHome(t)
			h.store.AddInstance(archiveActionInstance(t, "live-one", session.Ready))
			inst := archiveActionInstance(t, "resting", tc.status)
			h.store.AddInstance(inst)
			h.sidebar.SetSelectedInstance(1)
			require.Same(t, inst, h.sidebar.GetSelectedInstance(),
				"precondition: the cursor is on the resting row")

			resizeHome(h, 80, 24)
			_ = h.selectionChanged()

			out := h.menu.String()
			require.Contains(t, out, "restore")
			if tc.wantOffer {
				require.Contains(t, out, "open pane",
					"a %s row's pane renders its fallback content — `s` works and must be advertised (#5260)", tc.name)
			} else {
				require.NotContains(t, out, "open pane",
					"an archived row can never show a pane — `s` must stay withheld")
			}
		})
	}
}

// TestSelectionChanged_AdoptedLostRowAdvertisesOpenPane is the #5260 case
// through the header-adopted path (#4755): parked on the Sessions header
// while the bound lost row keeps its ▾ marker, the compact footer must name
// the same `s` it offers with the cursor on the row itself — the verb
// resolves the same bound instance either way.
func TestSelectionChanged_AdoptedLostRowAdvertisesOpenPane(t *testing.T) {
	h := newTestHome(t)
	lost := archiveActionInstance(t, "lost-one", session.Lost)
	h.store.AddInstance(archiveActionInstance(t, "live-one", session.Ready))
	h.store.AddInstance(lost)
	h.store.SelectInstance(lost)
	// Park the cursor on the Sessions header with the section expanded:
	// collapse it once and re-expand so the click lands back on the header.
	h.sidebar.ClickHeaderKind(ui.SectionInstances)
	h.sidebar.ClickHeaderKind(ui.SectionInstances)

	require.Same(t, lost, h.sidebar.RowVerbTarget(),
		"precondition: the Sessions header adopts the bound lost row")
	resizeHome(h, 80, 24)
	_ = h.selectionChanged()

	out := h.menu.String()
	require.Contains(t, out, "restore")
	require.Contains(t, out, "delete session")
	require.Contains(t, out, "open pane",
		"a lost row keeps a pane surface — `s` works and must be advertised (#5260)")
}
