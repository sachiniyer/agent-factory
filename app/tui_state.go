package app

import (
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
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
	restored := m.restoreTUIViewPanes(state.OpenPanes)
	m.restoreTUISelection(state)
	if state.Focus != nil {
		focusCopy := *state.Focus
		m.pendingTUIViewFocus = &focusCopy
	}
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
		tab, ok := tabIndexByName(inst, paneState.TabName)
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
	tabName := state.Selected.TabName
	if state.ActiveTab != nil && sameTUIStateInstance(*state.Selected, *state.ActiveTab) {
		tabName = state.ActiveTab.TabName
	}
	tab := tabIndexOrDefault(inst, tabName)
	m.sidebar.SelectInstance(inst)
	m.store.SetActiveTab(tab)
	m.menu.SetInstance(inst)
	m.menu.SetActiveTab(tab)
	m.sidebar.SelectTabRow(inst.Title, tab)
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
		tabName, ok := tabNameAt(inst, p.Tab())
		if !ok {
			continue
		}
		openPanes = append(openPanes, config.TUIStateOpenPane{
			Key:        tuiPaneKeyForInstance(inst, tabName),
			InstanceID: inst.ID,
			Title:      inst.Title,
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
	tabName, _ := tabNameAt(inst, m.store.ActiveTab())
	return &config.TUIStateTarget{
		InstanceID: inst.ID,
		Title:      inst.Title,
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
	case previewTickMsg, panesRefreshedMsg, repaintAfterDetachMsg, spinner.TickMsg, keyupMsg, hideErrMsg:
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
	tabs := inst.GetTabs()
	if idx < 0 || idx >= len(tabs) {
		return "", false
	}
	return tabs[idx].Name, true
}

func tabIndexByName(inst *session.Instance, name string) (int, bool) {
	for i, tab := range inst.GetTabs() {
		if tab.Name == name {
			return i, true
		}
	}
	return 0, false
}

func tabIndexOrDefault(inst *session.Instance, name string) int {
	if idx, ok := tabIndexByName(inst, name); ok {
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
