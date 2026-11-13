package app

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
)

// archiveLandingHome builds the #7229 play-test shape at its size (100x30, one
// pane fits): alpha with its Agent and Terminal tabs open as panes, then beta's
// Agent tab opened last, so it is the visible pane and alpha's two are
// open-but-hidden. Each pane is opened the way the play-test did, with the tree
// cursor on that tab's row and `s`. The cursor then rests on beta, on its
// instance row or its Agent tab row, ready for `a`. With trailing, a third
// session sits below beta, so a cursor that slips off beta's rows has a live
// row to land on.
func archiveLandingHome(t *testing.T, cursorOnTab, trailing bool) (h *home, beta *session.Instance) {
	t.Helper()
	h = newTestHome(t)
	alpha := instanceWithFakeBackend(t, "alpha")
	alpha.AddTabForTest("agent", session.TabKindAgent)
	alpha.AddTabForTest("shell", session.TabKindShell)
	beta = instanceWithFakeBackend(t, "beta")
	beta.AddTabForTest("agent", session.TabKindAgent)
	h.store.AddInstance(alpha)()
	h.store.AddInstance(beta)()
	if trailing {
		gamma := instanceWithFakeBackend(t, "gamma")
		gamma.AddTabForTest("agent", session.TabKindAgent)
		h.store.AddInstance(gamma)()
	}
	resizeHome(h, 100, 30)

	openFromTabRow := func(inst *session.Instance, tab int) {
		t.Helper()
		h.sidebar.SelectInstance(inst)
		h.sidebar.SelectTabRow(inst.Title, tab)
		_ = h.selectionChanged()
		_, _ = h.openOrFocusPane(inst, tab)
	}
	openFromTabRow(alpha, 0)
	openFromTabRow(alpha, 1)
	openFromTabRow(beta, 0)

	require.Equal(t, 3, h.store.NumOpenPanes(), "precondition: alpha·Agent, alpha·Terminal and beta·Agent are open")
	require.Equal(t, []string{"beta:0"}, visiblePaneTabs(h), "precondition: one pane fits, and it is the last opened")

	h.sidebar.SelectInstance(beta)
	if cursorOnTab {
		h.sidebar.SelectTabRow("beta", 0)
	}
	_ = h.selectionChanged()
	require.Same(t, beta, h.sidebar.GetSelectedInstance(), "precondition: the cursor is on beta")
	return h, beta
}

// visiblePaneTabs names the visible panes as "<title>:<tab>", since alpha owns
// two panes and the title alone cannot say which one is showing.
func visiblePaneTabs(h *home) []string {
	out := make([]string, 0, len(h.visiblePanes))
	for _, p := range h.visiblePanes {
		title := ""
		if p.Instance() != nil {
			title = p.Instance().Title
		}
		out = append(out, fmt.Sprintf("%s:%d", title, p.Tab()))
	}
	return out
}

// previewTick runs the idle 100ms refresh the event loop interleaves between
// every other message.
func previewTick(h *home) {
	h.inPreviewTick = true
	_ = h.selectionChanged()
	h.inPreviewTick = false
}

// archivedSnapshot is the daemon's snapshot once the archive has committed:
// every session as the TUI holds it, with archived reported Archived.
func archivedSnapshot(h *home, archived *session.Instance) []session.InstanceData {
	var out []session.InstanceData
	for _, inst := range h.store.GetInstances() {
		d := inst.ToInstanceData()
		if inst == archived {
			d.Liveness = session.LiveArchived
			d.InFlightOp = session.OpNone
		}
		out = append(out, d)
	}
	return out
}

// requireFrameConsistent is #7229's per-frame property, checked after every
// message: the hidden-pane notice never names a pane the workspace is showing,
// and while any pane is open the workspace paints one.
func requireFrameConsistent(t *testing.T, h *home, step string) {
	t.Helper()
	notice := h.errBox.FullError()
	for _, p := range h.visiblePanes {
		if label, ok := paneStatusLabel(p); ok {
			assert.NotContainsf(t, notice, label+" hidden",
				"%s: the status line calls the visible pane %q hidden (#7229)", step, label)
		}
	}
	if h.store.NumOpenPanes() == 0 {
		return
	}
	if assert.NotEmptyf(t, h.visiblePanes, "%s: panes are open but the workspace shows none (#7229)", step) {
		for _, p := range h.visiblePanes {
			w := h.paneWindows[p.ID()]
			if assert.NotNilf(t, w, "%s: visible pane %d has no window", step, p.ID()) {
				assert.NotEmptyf(t, strings.TrimSpace(w.View()),
					"%s: visible pane %d paints blank (#7229)", step, p.ID())
			}
		}
	}
}

// archiveLanding is the post-archive state #7229 requires to be deterministic.
type archiveLanding struct {
	cursorKind     ui.SidebarSectionKind
	cursorOnHeader bool
	cursorInstance string
	archivedOpen   bool
	visible        []string
}

func landingOf(h *home) archiveLanding {
	sel := h.sidebar.GetSelection()
	title := ""
	if inst := h.sidebar.GetSelectedInstance(); inst != nil {
		title = inst.Title
	}
	return archiveLanding{
		cursorKind:     sel.Kind,
		cursorOnHeader: sel.IsHeader,
		cursorInstance: title,
		archivedOpen:   strings.Contains(h.sidebar.View(), "▼ Archived"),
		visible:        visiblePaneTabs(h),
	}
}

// TestArchive_LandingIsDeterministicAndNoticeTracksPanes is #7229. Archiving
// beta closes its pane, and alpha's most recently focused pane (Terminal) takes
// the workspace. The archive reaches the TUI twice, in either order: the RPC
// reply (instanceArchivedMsg, finalize) and the snapshot poll that reports beta
// Archived (reconcile). Before the fix the status line kept "alpha · › Terminal
// hidden" on screen beside that very pane, because the notice cleared only once
// every open pane fit. The cursor also kept its flat index across each rebuild,
// so it landed wherever that index now pointed: the Archived header, with the
// folder collapsed, or the archived row when an earlier visit had left the
// folder open. On a tab row it slipped sooner, at the confirm: the archiving op
// folds beta's tab rows, and the index fell onto the next session's row, which
// then took the selection. Both orders, from either cursor row, with or without
// a session below beta, must now land the same way: the cursor follows beta to
// its row in the opened Archived folder, and alpha's Terminal pane is visible
// with no notice naming it hidden.
func TestArchive_LandingIsDeterministicAndNoticeTracksPanes(t *testing.T) {
	want := archiveLanding{
		cursorKind:     ui.SectionArchived,
		cursorOnHeader: false,
		cursorInstance: "beta",
		archivedOpen:   true,
		visible:        []string{"alpha:1"},
	}
	for _, trailing := range []bool{false, true} {
		for _, cursorOnTab := range []bool{false, true} {
			for _, reconcileFirst := range []bool{false, true} {
				name := fmt.Sprintf("trailing=%t/cursorOnTab=%t/reconcileFirst=%t", trailing, cursorOnTab, reconcileFirst)
				t.Run(name, func(t *testing.T) {
					h, beta := archiveLandingHome(t, cursorOnTab, trailing)
					target := captureSessionActionTarget(beta, h.repoID)

					// `a`, `y`: the confirm raises the optimistic archiving op, and the
					// RPC runs off the loop while ticks keep arriving.
					require.NoError(t, beta.Transition(session.BeginArchive()))
					previewTick(h)
					requireFrameConsistent(t, h, "archiving")

					finalize := func() {
						_, _ = h.handleInstanceArchived(instanceArchivedMsg{target: target})
						requireFrameConsistent(t, h, "after the archive reply")
						previewTick(h)
						requireFrameConsistent(t, h, "tick after the archive reply")
					}
					reconcile := func() {
						h.reconcileSnapshot(archivedSnapshot(h, beta))
						requireFrameConsistent(t, h, "after the archived snapshot")
						previewTick(h)
						requireFrameConsistent(t, h, "tick after the archived snapshot")
					}
					if reconcileFirst {
						reconcile()
						finalize()
					} else {
						finalize()
						reconcile()
					}

					require.Equal(t, session.LiveArchived, beta.GetLiveness())
					assert.Equal(t, want, landingOf(h), "the post-archive landing state (#7229)")
				})
			}
		}
	}
}
