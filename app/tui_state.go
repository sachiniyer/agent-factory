package app

import (
	"fmt"
	"reflect"
	"sort"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/store"
)

const tuiFocusRegionPane = "pane"

// restoreTUIViewStateOnLaunch restores client-side pane/selection/focus state
// after the daemon snapshot has populated the projection. It never fails the
// launch; unreadable state files fall back to the ordinary cold-start path.
func (m *home) restoreTUIViewStateOnLaunch() int {
	state, ok := config.LoadTUIRepoViewState(m.repoID)
	restored := 0
	if ok {
		restored = m.applyTUIViewState(state)
		if restored > 0 {
			m.initialPaneOpened = true
		}
	}
	m.rememberTUIViewState()
	return restored
}

func (m *home) applyTUIViewState(state config.TUIRepoViewState) int {
	if state.Focus != nil {
		focusCopy := *state.Focus
		m.pendingTUIViewFocus = &focusCopy
	}
	restored := m.restoreTUIViewPanes(state.OpenPanes)
	m.restoreTUISelection(state)
	// A persisted selection that resolved to an archived row (or to nothing)
	// leaves the store with no display selection, yet a live pane may have been
	// restored above — an incoherent mix where the sidebar highlights nothing (or
	// a stale archived row) while the workspace renders a live session, with
	// keyboard nav stuck (#1559). Reconcile now, before the relayout, so launch
	// selects the live session whose pane is actually shown.
	m.reconcileRestoredSelectionWithWorkspace()
	// The relayout below runs at term (0,0) → fallback → visiblePanes=nil, so
	// capture the restored panes as the baseline the first real relayout uses to
	// surface an auto-hide status for a pane the terminal can't fit (#1535).
	if restored > 0 {
		m.restoredPaneBaseline = append([]*store.OpenPane(nil), m.store.OpenPanes()...)
	}
	m.relayout()
	return restored
}

func (m *home) restoreTUIViewPanes(saved []config.TUIStateOpenPane) int {
	type restoredPane struct {
		saved config.TUIStateOpenPane
		pane  *store.OpenPane
	}
	var restored []restoredPane
	seen := make(map[string]bool, len(saved))
	for _, paneState := range saved {
		inst := m.resolveTUIStateInstance(paneState.InstanceID, paneState.Title)
		if inst == nil || inst.GetLiveness() == session.LiveArchived {
			continue
		}
		tab, ok := tabIndexByIdentity(inst, paneState.TabID, paneState.TabName)
		if !ok {
			continue
		}
		key := paneState.Key
		if key == "" {
			key = tuiPaneKey(paneState.InstanceID, paneState.Title, paneState.TabName)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		pane := m.store.FindOpenPane(inst, tab)
		if pane == nil {
			pane = m.openPaneWindow(inst, tab)
		}
		if pane == nil {
			continue
		}
		// Pane keys intentionally stay name-based for persisted-state backward
		// compatibility. If ID-first restore followed a renamed tab, translate
		// the pending focus from its saved key to that pane's current name key so
		// the first sized relayout can still restore keyboard focus.
		if m.pendingTUIViewFocus != nil && m.pendingTUIViewFocus.Region == tuiFocusRegionPane &&
			m.pendingTUIViewFocus.PaneKey == key {
			m.pendingTUIViewFocus.PaneKey = tuiPaneKeyForOpenPane(pane)
		}
		restored = append(restored, restoredPane{saved: paneState, pane: pane})
	}
	sort.SliceStable(restored, func(i, j int) bool {
		return restored[i].saved.FocusRank < restored[j].saved.FocusRank
	})
	for _, item := range restored {
		m.store.TouchOpenPane(item.pane)
	}
	return len(restored)
}

func (m *home) restoreTUISelection(state config.TUIRepoViewState) {
	if state.Selected == nil {
		return
	}
	inst := m.resolveTUIStateInstance(state.Selected.InstanceID, state.Selected.Title)
	if inst == nil || inst.GetLiveness() == session.LiveArchived {
		return
	}
	tabID := state.Selected.TabID
	tabName := state.Selected.TabName
	if state.ActiveTab != nil && sameTUIStateInstance(*state.Selected, *state.ActiveTab) {
		tabID = state.ActiveTab.TabID
		tabName = state.ActiveTab.TabName
	}
	tab := tabIndexOrDefault(inst, tabID, tabName)
	m.sidebar.SelectInstance(inst)
	m.store.SetActiveTab(tab)
	m.menu.SetInstance(inst)
	m.sidebar.SelectTabRow(inst.Title, tab)
}

// reconcileRestoredSelectionWithWorkspace keeps the restored sidebar selection
// coherent with the panes brought back on launch (#1559). restoreTUISelection
// deliberately refuses to bind an archived (or vanished) persisted selection, so
// the store can be left with no live display selection while restoreTUIViewPanes
// still put a live pane on screen. That mismatch is the #1559 bug: the workspace
// shows a live session but the sidebar highlights nothing (or an archived row),
// and Down from the archived tail row has nowhere to go so keyboard nav stalls.
// When the store has no live selection but a live pane is displayed, select that
// live session — preferring the pane the restored focus points at, else the
// most-recently-focused open pane — so the selected row and the shown pane always
// refer to the same instance and the tree is immediately navigable.
func (m *home) reconcileRestoredSelectionWithWorkspace() {
	if sel := m.store.GetSelectedInstance(); sel != nil && !sel.ShownArchived() {
		return // a live selection was restored — already coherent
	}
	pane := m.restoredFocusPane()
	if pane == nil {
		pane = m.mostRecentlyFocusedOpenPane()
	}
	if pane == nil {
		return // no pane on screen — leave the ordinary cold-start selection
	}
	inst := pane.Instance()
	if inst == nil || inst.ShownArchived() {
		return
	}
	tab := pane.Tab()
	m.sidebar.SelectInstance(inst)
	m.store.SetActiveTab(tab)
	m.menu.SetInstance(inst)
	m.sidebar.SelectTabRow(inst.Title, tab)
}

// restoredFocusPane resolves the open pane the persisted focus (loaded into
// pendingTUIViewFocus by applyTUIViewState) points at, or nil when the focus is
// not on a pane / the pane was not restored. Used to pick which live session the
// launch selection should follow when the persisted selection can't be honored.
func (m *home) restoredFocusPane() *store.OpenPane {
	if m.pendingTUIViewFocus == nil || m.pendingTUIViewFocus.Region != tuiFocusRegionPane {
		return nil
	}
	return m.openPaneByKey(m.pendingTUIViewFocus.PaneKey)
}

// mostRecentlyFocusedOpenPane returns the open pane with the highest focus-
// recency stamp — the pane most likely to be the one the user was last looking
// at, and the best proxy for "the shown workspace" when no explicit focus pane
// survives the restore.
func (m *home) mostRecentlyFocusedOpenPane() *store.OpenPane {
	var best *store.OpenPane
	for _, p := range m.store.OpenPanes() {
		if best == nil || p.LastFocus() > best.LastFocus() {
			best = p
		}
	}
	return best
}

func (m *home) applyPendingTUIViewFocus() {
	if m.pendingTUIViewFocus == nil {
		return
	}
	focus := m.pendingTUIViewFocus
	m.pendingTUIViewFocus = nil
	switch focus.Region {
	case layout.RegionTree:
		m.ring.Focus(layout.RegionTree)
	case layout.RegionAutomations:
		if !m.ring.Focus(layout.RegionAutomations) {
			m.ring.Focus(layout.RegionTree)
		}
	case tuiFocusRegionPane:
		if p := m.openPaneByKey(focus.PaneKey); p != nil {
			if m.ring.Focus(layout.PaneRegion(p.ID())) {
				return
			}
		}
		m.ring.Focus(layout.RegionTree)
	default:
		m.ring.Focus(layout.RegionTree)
	}
}

func (m *home) captureTUIViewState() config.TUIRepoViewState {
	var openPanes []config.TUIStateOpenPane
	for _, p := range m.store.OpenPanes() {
		inst := p.Instance()
		if inst == nil || !m.store.ContainsInstance(inst) {
			continue
		}
		tabID, tabName, ok := tabIdentityAt(inst, p.Tab())
		if !ok {
			continue
		}
		openPanes = append(openPanes, config.TUIStateOpenPane{
			Key:        tuiPaneKeyForInstance(inst, tabName),
			InstanceID: inst.ID,
			Title:      inst.Title,
			TabID:      tabID,
			TabName:    tabName,
			FocusRank:  p.LastFocus(),
		})
	}
	state := config.TUIRepoViewState{
		Selected:  m.captureTUISelectedTarget(),
		ActiveTab: m.captureTUISelectedTarget(),
		Focus:     m.captureTUIFocus(),
		OpenPanes: openPanes,
	}
	return state
}

func (m *home) captureTUISelectedTarget() *config.TUIStateTarget {
	inst := m.store.GetSelectedInstance()
	if inst == nil || !m.store.ContainsInstance(inst) {
		return nil
	}
	tabID, tabName, _ := tabIdentityAt(inst, m.store.ActiveTab())
	return &config.TUIStateTarget{
		InstanceID: inst.ID,
		Title:      inst.Title,
		TabID:      tabID,
		TabName:    tabName,
	}
}

func (m *home) captureTUIFocus() *config.TUIStateFocus {
	active := m.ring.Active()
	if layout.IsPaneRegion(active) {
		if p := m.focusedOpenPane(); p != nil {
			return &config.TUIStateFocus{
				Region:  tuiFocusRegionPane,
				PaneKey: tuiPaneKeyForOpenPane(p),
			}
		}
		return &config.TUIStateFocus{Region: layout.RegionTree}
	}
	if active == "" {
		return nil
	}
	return &config.TUIStateFocus{Region: active}
}

func (m *home) persistTUIViewStateIfChanged() {
	next := normalizeTUIViewState(m.captureTUIViewState())
	if m.hasLastTUIViewState && reflect.DeepEqual(m.lastTUIViewState, next) {
		return
	}
	if err := m.writeTUIViewState(next); err != nil {
		log.WarningLog.Printf("failed to save TUI state: %v", err)
		// Remember the attempted state so a preview tick does not retry the
		// same failing write forever. A later structural change, or quit flush,
		// will try again.
		m.lastTUIViewState = next
		m.hasLastTUIViewState = true
		return
	}
	m.lastTUIViewState = next
	m.hasLastTUIViewState = true
}

func (m *home) persistTUIViewStateAfter(msg tea.Msg) {
	if !shouldPersistTUIViewStateAfter(msg) {
		return
	}
	m.persistTUIViewStateIfChanged()
}

func shouldPersistTUIViewStateAfter(msg tea.Msg) bool {
	switch msg.(type) {
	case previewTickMsg, panesRefreshedMsg, panePreviewStaleExpiredMsg,
		repaintAfterDetachMsg, keyupMsg, hideErrMsg:
		return false
	default:
		return true
	}
}

func (m *home) flushTUIViewStateBestEffort() {
	next := normalizeTUIViewState(m.captureTUIViewState())
	if err := m.writeTUIViewState(next); err != nil {
		log.WarningLog.Printf("failed to flush TUI state on quit: %v", err)
		return
	}
	m.lastTUIViewState = next
	m.hasLastTUIViewState = true
}

func (m *home) rememberTUIViewState() {
	m.lastTUIViewState = normalizeTUIViewState(m.captureTUIViewState())
	m.hasLastTUIViewState = true
}

func (m *home) writeTUIViewState(state config.TUIRepoViewState) error {
	if m.repoID == "" {
		return nil
	}
	state.UpdatedAt = time.Now().UTC()
	return config.SaveTUIRepoViewState(m.repoID, state)
}

func normalizeTUIViewState(state config.TUIRepoViewState) config.TUIRepoViewState {
	state.UpdatedAt = time.Time{}
	return state
}

func (m *home) resolveTUIStateInstance(instanceID, title string) *session.Instance {
	if instanceID != "" {
		for _, inst := range m.store.GetInstances() {
			if inst.ID == instanceID {
				return inst
			}
		}
	}
	if title != "" {
		return m.store.GetInstanceByTitle(title)
	}
	return nil
}

func (m *home) openPaneByKey(key string) *store.OpenPane {
	if key == "" {
		return nil
	}
	for _, p := range m.store.OpenPanes() {
		if tuiPaneKeyForOpenPane(p) == key {
			return p
		}
	}
	return nil
}

func tabNameAt(inst *session.Instance, idx int) (string, bool) {
	_, name, ok := tabIdentityAt(inst, idx)
	return name, ok
}

func tabIdentityAt(inst *session.Instance, idx int) (string, string, bool) {
	tabs := inst.GetTabs()
	if idx < 0 || idx >= len(tabs) {
		return "", "", false
	}
	return tabs[idx].ID, tabs[idx].Name, true
}

func tabIndexByIdentity(inst *session.Instance, id, name string) (int, bool) {
	tabs := inst.GetTabs()
	if id != "" {
		for i, tab := range tabs {
			if tab.ID == id {
				return i, true
			}
		}
	}
	for i, tab := range tabs {
		if tab.Name == name {
			return i, true
		}
	}
	return 0, false
}

func tabIndexOrDefault(inst *session.Instance, id, name string) int {
	if idx, ok := tabIndexByIdentity(inst, id, name); ok {
		return idx
	}
	return 0
}

func tuiPaneKeyForOpenPane(p *store.OpenPane) string {
	if p == nil || p.Instance() == nil {
		return ""
	}
	tabName, ok := tabNameAt(p.Instance(), p.Tab())
	if !ok {
		return ""
	}
	return tuiPaneKeyForInstance(p.Instance(), tabName)
}

func tuiPaneKeyForInstance(inst *session.Instance, tabName string) string {
	if inst == nil {
		return ""
	}
	return tuiPaneKey(inst.ID, inst.Title, tabName)
}

func tuiPaneKey(instanceID, title, tabName string) string {
	if title != "" {
		return fmt.Sprintf("title:%s:tab:%s", title, tabName)
	}
	return fmt.Sprintf("id:%s:tab:%s", instanceID, tabName)
}

func sameTUIStateInstance(a, b config.TUIStateTarget) bool {
	if a.InstanceID != "" && b.InstanceID != "" {
		return a.InstanceID == b.InstanceID
	}
	return a.Title != "" && a.Title == b.Title
}
