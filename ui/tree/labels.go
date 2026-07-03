package tree

import (
	"github.com/sachiniyer/agent-factory/session"
)

// defaultTabLabels are the labels shown before an instance is selected, or for
// one whose tabs haven't materialized yet. They preserve the exact pre-#930
// two-tab bar so the UX is identical: slot 0 is the agent ("Preview") tab, slot
// 1 the terminal tab.
var defaultTabLabels = []string{"Preview", "Terminal"}

// TabLabels returns the display labels for an instance's tab slots — the
// single source of truth shared by the TabbedWindow's tab bar and the sidebar
// tree (#1024 PR 3), so the bar, the tree's child rows, and the 1-9 jump keys
// always agree on slot numbering. Agent tabs render as "Preview", shell tabs
// as "Terminal"; any Process tab renders under its own name. A nil instance
// yields the default two-slot bar.
//
// Remote instances are tab-driven too (#930 PR 6): their real tab set is the
// agent tab plus a terminal tab only when terminal_cmd is configured, so the
// labels reflect exactly those tabs — a terminal_cmd-less remote shows a single
// tab rather than the local two-tab default. Local instances keep the
// default-padded list (always at least the two slots) so the slot count never
// dips below two mid-start, identical to the pre-#930 UX. The result is never
// empty.
func TabLabels(instance *session.Instance) []string {
	if instance != nil && instance.IsRemote() {
		if tabs := instance.GetTabs(); len(tabs) > 0 {
			labels := make([]string, len(tabs))
			for i, tab := range tabs {
				labels[i] = labelForTab(tab)
			}
			return labels
		}
		// Pre-start remote (no tabs yet): fall through to the default bar.
	}

	labels := append([]string(nil), defaultTabLabels...)
	if instance == nil {
		return labels
	}
	for idx, tab := range instance.GetTabs() {
		label := labelForTab(tab)
		if idx < len(labels) {
			labels[idx] = label
		} else {
			labels = append(labels, label)
		}
	}
	return labels
}

func labelForTab(tab *session.Tab) string {
	switch tab.Kind {
	case session.TabKindAgent:
		return "Preview"
	case session.TabKindShell:
		return "Terminal"
	default:
		if tab.Name != "" {
			return tab.Name
		}
		return "Tab"
	}
}
