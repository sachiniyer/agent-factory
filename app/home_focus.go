// Focus-ring navigation for the home model: which region holds focus and how
// keys move it. Split out of home_model.go (#1145).

package app

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/ui/layout"
)

// syncFocus applies the focus ring's active region to the panes and the
// status-bar hints, and stamps a focused pane most recently focused so the
// §2.6 auto-hide order tracks real attention.
func (m *home) syncFocus() {
	active := m.ring.Active()
	panes := map[string]layout.Pane{
		layout.RegionTree:        m.sidebar,
		layout.RegionAutomations: m.automations,
		layout.RegionProjects:    m.projects,
	}
	for _, p := range m.visiblePanes {
		if w := m.paneWindows[p.ID()]; w != nil {
			panes[layout.PaneRegion(p.ID())] = w
		}
	}
	for id, pane := range panes {
		if id == active {
			pane.Focus()
		} else {
			pane.Blur()
		}
	}
	if p := m.focusedOpenPane(); p != nil {
		m.store.TouchOpenPane(p)
		m.lastFocusedPaneID = p.ID()
	}
	m.menu.SetFocusRegion(active)
	m.syncSplitPaneHint()
	m.syncScrollHint()
	// ←/→ move focus between VISIBLE panes, and focusAdjacentPane returns early
	// below two of them, so the pair is inert with one pane open. Advertising it
	// there is the same dead-affordance class as #2830/#2864.
	m.menu.SetPaneCycleAvailable(len(m.visiblePanes) > 1)
	m.syncProjectsHint()
}

// syncProjectsHint keeps the Projects footer's row-scoped verbs (Enter switch,
// D delete) in step with whether the section HAS a row. Both act on the cursor,
// and handleProjectsFocus consumes each as a no-op when SelectedProject reports
// false, so an empty section must advertise neither.
//
// Called from syncFocus AND from both project refreshes: rows change on a
// background poll, which is not guaranteed to reach a relayout, and a stale
// gate here would re-advertise a verb that had just gone inert.
func (m *home) syncProjectsHint() {
	m.menu.SetProjectRowsAvailable(m.projects.HasProjects())
}

// focusRegion moves focus directly to the given region and re-solves the
// layout so ring visibility and pane rects stay coherent.
func (m *home) focusRegion(region string) {
	m.ring.Focus(region)
	m.relayout()
}

// cycleFocus advances the focus ring (Tab / Shift-Tab). Task edits are made
// only inside the tasks overlay, which saves on close (handleStateTasks) —
// the in-rail automations section is read-only, so no save is needed here.
func (m *home) cycleFocus(back bool) tea.Cmd {
	// A live pane preview is transient chrome, not a focus stop. Tab / Shift-Tab
	// must DISMISS it and still advance the ring one step — never swallow the
	// keystroke (#1705). Earlier this early-returned after cancelling the
	// preview, so with a preview live (which the idle tick creates whenever the
	// tree cursor names a tab the owner pane isn't bound to) reverse traversal
	// out of a pane was impossible: the ring never moved.
	//
	// The step anchors on ring.Active() — cancelPanePreview(false), NOT
	// cancelPanePreview(true) — so a Tab pressed from the tree while a
	// background preview happens to be owned by some pane still steps from the
	// tree, not from that pane.
	var refresh tea.Cmd
	if m.panePreviewTxn != nil {
		m.suppressActivePanePreview()
		m.cancelPanePreview(false)
		refresh = m.panesRefresh(m.attached.Load())
	}
	if back {
		m.ring.Prev()
	} else {
		m.ring.Next()
	}
	m.relayout()
	return refresh
}

// focusAdjacentSection moves focus to the next / previous SECTION region — the
// non-pane focus-ring stops: the instances tree, the automations rail, and the
// projects rail. Workspace panes are skipped (Tab steps through those, 1-9 jump
// to tabs). This is what the `]` / `[` "next / prev section" bindings drive
// (#1706): automations and projects became their own Tab-focusable rail
// sections (#1470 / #1588 / #1590) rather than sidebar section headers, so the
// old header-walking JumpNextSection could never reach them — from an instance
// row `]` was a silent no-op. Stepping the real focus ring makes `]` land on
// Automations (dropping the instance-only `D kill` from the footer) exactly as
// the binding advertises, and `[` walks back.
func (m *home) focusAdjacentSection(back bool) tea.Cmd {
	// Mirror cycleFocus: a live preview is transient chrome and must be
	// dismissed without swallowing the keystroke (#1705 class).
	var refresh tea.Cmd
	if m.panePreviewTxn != nil {
		m.suppressActivePanePreview()
		m.cancelPanePreview(false)
		refresh = m.panesRefresh(m.attached.Load())
	}
	// Step the ring, skipping workspace panes, until it rests on a section
	// region. The tree is always a visible non-pane stop, so this always
	// terminates; bound the loop by the ring size as a backstop.
	for i := 0; i < len(m.visiblePanes)+3; i++ {
		var id string
		if back {
			id = m.ring.Prev()
		} else {
			id = m.ring.Next()
		}
		if id == "" || !layout.IsPaneRegion(id) {
			break
		}
	}
	m.relayout()
	return refresh
}
