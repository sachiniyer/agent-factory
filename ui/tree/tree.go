// Package tree models the sidebar's instances+tabs tree (#1024 PR 3): every
// instance is a row whose tabs render as expandable children, so the left rail
// shows the same tab set the content pane's tab bar does and selection gains a
// tab dimension. The package holds the pure, session-agnostic pieces — row
// flattening, tab labels, and row rendering (absorbed from ui/list.go) — while
// the Sidebar keeps its local UI state (cursor, windowing, expansion), reading
// everything else from the ui/store projection per the #1024 architecture.
package tree

import (
	"github.com/sachiniyer/agent-factory/session"
)

// Row is one visible row of the instances tree: the instance itself
// (TabIndex == -1) or one of its tab children.
type Row struct {
	InstanceIndex int
	TabIndex      int
}

// IsTab reports whether the row is a tab child rather than an instance row.
func (r Row) IsTab() bool { return r.TabIndex >= 0 }

// Flatten returns the visible tree rows for n instances in order: each
// instance row followed, when expanded(i), by one child row per tab slot
// (tabCount(i)). Pure — expansion policy and tab counts are the caller's.
func Flatten(n int, expanded func(int) bool, tabCount func(int) int) []Row {
	rows := make([]Row, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, Row{InstanceIndex: i, TabIndex: -1})
		if !expanded(i) {
			continue
		}
		for t := 0; t < tabCount(i); t++ {
			rows = append(rows, Row{InstanceIndex: i, TabIndex: t})
		}
	}
	return rows
}

// Expandable reports whether an instance's tab children may be shown.
// Transient rows — a Loading creation (#808) or a Deleting kill (#844) — are
// never expandable: an in-flight TUI operation owns the row and its tabs are
// not meaningfully attachable, so the tree pins them to a single (spinner +
// marker) row. This is the tree equivalent of the flat list's transient-row
// treatment: the reconcile already leaves such rows alone, and the tree keeps
// them out of the tab dimension entirely.
func Expandable(inst *session.Instance) bool {
	if inst == nil {
		return false
	}
	status := inst.GetStatus()
	return status != session.Loading && status != session.Deleting
}
