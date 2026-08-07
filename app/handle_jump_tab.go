package app

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

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

	// Resolved against the instance the jump will actually TARGET, and by CANONICAL
	// NAME (#3067 review).
	//
	// Two distinct mistakes were here. handleTabJump acts on the focused pane when
	// there is one, so resolving against the sidebar selection could name a tab in a
	// different session than the one about to move. And tree.TabLabels returns
	// DECORATED text — "◆ Agent", "› Terminal" — which session/tab.go says in as many
	// words is never resolved against: TabMatches keys on Name alone, so the label is
	// free to be the pretty string. Matching the pretty string would accept glyphs
	// nobody types and reject the name everything else in af accepts.
	//
	// Read at submit time rather than when the prompt opened: a tab can be created or
	// closed while it is up, and a stale list would jump by an ordinal that no longer
	// means what the user saw.
	names := m.jumpTargetTabNames()
	idx := ui.ResolveTabJump(query, names)
	if idx == 0 {
		// Said out loud rather than swallowed. "No such tab" and "ambiguous" are both
		// answers the user can act on; a prompt that closes with nothing happening is
		// indistinguishable from a bug, which is the #3021 shape all over again.
		m.errBox.SetNotice(jumpTabMiss(query, names))
		return m, nil
	}
	return m.handleTabJump(idx)
}

// jumpTargetTabNames returns the canonical tab names of whichever instance a jump
// would act on: the focused pane's, or the sidebar selection when no pane is
// focused. Mirrors handleTabJump's own target choice deliberately — a resolver that
// disagrees with the mover is how a jump lands somewhere the user did not name.
func (m *home) jumpTargetTabNames() []string {
	inst := m.store.GetSelectedInstance()
	if p := m.focusedOpenPane(); p != nil && p.Instance() != nil {
		inst = p.Instance()
	}
	if inst == nil {
		return nil
	}
	tabs := inst.GetTabs()
	names := make([]string, 0, len(tabs))
	for _, tab := range tabs {
		names = append(names, tab.Name)
	}
	return names
}

// jumpTabMiss explains WHY nothing happened, distinguishing the two reasons so the
// next keystroke can be the right one: a typo wants retyping, an ambiguous prefix
// wants more characters.
func jumpTabMiss(query string, labels []string) error {
	if ui.ResolveTabJumpCandidates(query, labels) > 1 {
		return fmt.Errorf("more than one tab matches %q; type more of the name", query)
	}
	return fmt.Errorf("no tab matches %q", query)
}
