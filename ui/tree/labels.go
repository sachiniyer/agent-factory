package tree

import (
	"github.com/sachiniyer/agent-factory/session"
)

// placeholderTabLabels is the bar shown before an instance is selected or
// before its tabs have materialized (mid-start/Loading). It is a single slot:
// the agent tab is the only tab every instance is guaranteed to have —
// terminal tabs exist on demand only (#1100) — so promising a second slot
// before the real tab list exists would manufacture a phantom jump/attach
// target in every consumer of TabLabels.
var placeholderTabLabels = []string{"Agent"}

// TabLabels returns the display labels for an instance's tab slots — the
// single source of truth shared by the TabbedWindow's header, the sidebar
// tree (#1024 PR 3), and the 1-9 jump keys, so slot numbering can never
// disagree between them. Agent tabs render as "Agent", shell tabs as
// "Terminal"; any Process tab renders under its own name.
//
// Once an instance's tabs have materialized the labels mirror the real tab
// list exactly, one slot per tab — local and remote alike. Since #1100 a
// fresh local instance starts with only its agent tab, so a fresh instance
// shows a single slot until `t` adds a terminal; padding to a two-slot
// minimum (the pre-#1100 behavior) would advertise a dead "Terminal" slot.
// A nil instance or one with no tabs yet yields the single-slot placeholder;
// the result is never empty.
func TabLabels(instance *session.Instance) []string {
	if instance != nil {
		if tabs := instance.GetTabs(); len(tabs) > 0 {
			labels := make([]string, len(tabs))
			for i, tab := range tabs {
				labels[i] = labelForTab(tab)
			}
			return labels
		}
	}
	return append([]string(nil), placeholderTabLabels...)
}

func labelForTab(tab *session.Tab) string {
	switch tab.Kind {
	case session.TabKindAgent:
		return "Agent"
	case session.TabKindShell:
		return "Terminal"
	case session.TabKindWeb:
		if tab.Name != "" {
			return tab.Name
		}
		return "Web"
	case session.TabKindVSCode:
		if tab.Name != "" {
			return tab.Name
		}
		return "VS Code"
	default:
		if tab.Name != "" {
			return tab.Name
		}
		return "Tab"
	}
}
