package app

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

// The unbounded jump-to-tab prompt (#3021).
//
// The number keys reach the first nine tabs, and nothing capped tab CREATION — so
// from the tenth tab on, the fastest path to a tab simply disappeared, with nothing
// saying more tabs existed. The missing affordance is why it read as a nine-tab
// limit. Binding more digits would move the wall rather than remove it; a prompt that
// takes a number OR a name has no wall, and answers the question someone with fifteen
// tabs actually has, which is "where is the one called deploy" rather than "which
// ordinal is it".
//
// Deliberately not a fuzzy picker with a live-filtered list: this is a jump, and the
// existing PromptOverlay already handles a single line of text with a caret. A list
// would be a second way to browse tabs alongside the tree, which is not the gap.

// showJumpTabPrompt opens the prompt. Empty-seeded rather than remembering the last
// query: a jump is a fresh intent, and a stale query one keystroke from Enter is a
// jump to the wrong tab.
func (m *home) showJumpTabPrompt() (tea.Model, tea.Cmd) {
	// Gated on workspace focus, exactly as the digit jumps are (#3067 review). The
	// digits check this before dispatching; g is routed through the global key map
	// and so had no gate, which meant it retargeted the workspace while the
	// Automations or Projects rail was focused — the pre-cutover behaviour those
	// gates exist to prevent. A rail that owns the keyboard keeps it.
	if active := m.ring.Active(); active != layout.RegionTree && !layout.IsPaneRegion(active) {
		return m, nil
	}
	if m.store.GetSelectedInstance() == nil {
		return m, nil // nothing to jump within
	}
	m.promptOverlay = overlay.NewPromptOverlay("Jump to tab (number or name)", "")
	m.layoutPromptOverlay()
	m.state = stateJumpTab
	return m, nil
}

// handleStateJumpTab drives the prompt and performs the jump.
//
// Returns to stateDefault on both paths — unlike statePromptInput, which belongs to
// the naming form and returns to stateNew. The two states share one promptOverlay
// field; what differs is who owns the answer.
func (m *home) handleStateJumpTab(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Submit and cancel are decided HERE, not delegated (#3067 review).
	// PromptOverlay is the naming form's multi-line initial-prompt field: Enter is
	// TEXT to it, Tab and Esc merely close, and only ctrl+c marks a cancel. Handing it
	// this prompt unchanged gave a jump box where Enter typed a newline and ESC
	// PERFORMED THE JUMP — the opposite of what esc means everywhere else in the app.
	// A one-line jump has different semantics than a prose field, so it owns them.
	switch msg.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		m.promptOverlay = nil
		m.state = stateDefault
		return m, nil
	case tea.KeyEnter:
		// fall through to resolve
	default:
		m.promptOverlay.HandleKeyPress(msg)
		return m, nil
	}
	query := m.promptOverlay.Value()
	m.promptOverlay = nil
	m.state = stateDefault

	// Resolved against the instance the jump will actually TARGET (#3067 review).
	//
	// Two distinct mistakes were here. handleTabJump acts on the focused pane when
	// there is one, so resolving against the sidebar selection could name a tab in a
	// different session than the one about to move. And tree.TabLabels returns
	// DECORATED text — "◆ Agent", "› Terminal" — including glyphs nobody types. The
	// prompt resolves the canonical Name first, then the undecorated session.TabLabel
	// as a UI-local alias so the text a user sees remains discoverable without
	// changing what name-based CLI and wire operations accept (#3997).
	//
	// Read at submit time rather than when the prompt opened: a tab can be created or
	// closed while it is up, and a stale list would jump by an ordinal that no longer
	// means what the user saw.
	tabs := m.jumpTargetTabs()
	idx := resolveTabJump(query, tabs)
	if idx == 0 {
		// Said out loud rather than swallowed. "No such tab" and "ambiguous" are both
		// answers the user can act on; a prompt that closes with nothing happening is
		// indistinguishable from a bug, which is the #3021 shape all over again.
		m.errBox.SetNotice(jumpTabMiss(query, tabs))
		return m, nil
	}
	return m.handleTabJump(idx)
}

// jumpTargetTabs returns the tabs of whichever instance a jump would act on: the
// focused pane's, or the sidebar selection when no pane is
// focused. Mirrors handleTabJump's own target choice deliberately — a resolver that
// disagrees with the mover is how a jump lands somewhere the user did not name.
func (m *home) jumpTargetTabs() []*session.Tab {
	inst := m.store.GetSelectedInstance()
	if p := m.focusedOpenPane(); p != nil && p.Instance() != nil {
		inst = p.Instance()
	}
	if inst == nil {
		return nil
	}
	return inst.GetTabs()
}

// resolveTabJump preserves canonical names as the first-choice identity, then
// accepts the undecorated display labels as a TUI-only discoverability alias. If
// the name tier is ambiguous it stays ambiguous rather than letting a label pick a
// different winner.
func resolveTabJump(query string, tabs []*session.Tab) int {
	names, labels := tabJumpNamesAndLabels(tabs)
	if idx := ui.ResolveTabJump(query, names); idx != 0 {
		return idx
	}
	if ui.ResolveTabJumpCandidates(query, names) > 0 {
		return 0
	}
	return ui.ResolveTabJump(query, labels)
}

// jumpTabMiss explains WHY nothing happened, distinguishing the two reasons so the
// next keystroke can be the right one: a typo wants retyping, an ambiguous prefix
// wants more characters.
func jumpTabMiss(query string, tabs []*session.Tab) error {
	names, labels := tabJumpNamesAndLabels(tabs)
	candidates := ui.ResolveTabJumpCandidates(query, names)
	if candidates == 0 {
		candidates = ui.ResolveTabJumpCandidates(query, labels)
	}
	available := tabJumpAvailableTabs(tabs)
	if available != "" {
		available = "; available tabs: " + available
	}
	if candidates > 1 {
		return fmt.Errorf("more than one tab matches %q; type more of the name%s", query, available)
	}
	return fmt.Errorf("no tab matches %q%s", query, available)
}

func tabJumpNamesAndLabels(tabs []*session.Tab) ([]string, []string) {
	names := make([]string, len(tabs))
	labels := make([]string, len(tabs))
	for i, tab := range tabs {
		if tab != nil {
			names[i] = tab.Name
		}
		labels[i] = session.TabLabel(tab)
	}
	return names, labels
}

func tabJumpAvailableTabs(tabs []*session.Tab) string {
	available := make([]string, 0, len(tabs))
	for _, tab := range tabs {
		if tab == nil {
			continue
		}
		label := session.TabLabel(tab)
		if tab.Name != "" && label != tab.Name {
			available = append(available, fmt.Sprintf("%s (%s)", label, tab.Name))
		} else if label != "" {
			available = append(available, label)
		}
	}
	return strings.Join(available, ", ")
}
