package app

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/overlay"
	"github.com/sachiniyer/agent-factory/ui/tree"
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
	shouldClose := m.promptOverlay.HandleKeyPress(msg)
	if !shouldClose {
		return m, nil
	}
	canceled := m.promptOverlay.IsCanceled()
	query := m.promptOverlay.Value()
	m.promptOverlay = nil
	m.state = stateDefault
	if canceled {
		return m, nil
	}

	// Resolved against the SELECTION's tabs, read at submit time rather than when the
	// prompt opened: a tab can be created or closed while it is up, and resolving
	// against a stale list would jump by an ordinal that no longer means what the user
	// saw.
	labels := tree.TabLabels(m.store.GetSelectedInstance())
	idx := ui.ResolveTabJump(query, labels)
	if idx == 0 {
		// Said out loud rather than swallowed. "No such tab" and "ambiguous" are both
		// answers the user can act on; a prompt that closes with nothing happening is
		// indistinguishable from a bug, which is the #3021 shape all over again.
		m.errBox.SetNotice(jumpTabMiss(query, labels))
		return m, nil
	}
	return m.handleTabJump(idx)
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
