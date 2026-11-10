package ui

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/store"
)

// namingRail is a rail of started-shaped sessions (agent + shell tabs, no op,
// so the selected one auto-expands) plus a naming row with the given draft
// title, marked in the store and selected the way the create form does it:
// marked first, then selected.
func namingRail(t *testing.T, draft string) (*Sidebar, *store.Projection, *session.Instance) {
	t.Helper()
	proj := store.NewProjection()
	s := NewSidebar(proj)
	s.SetSize(40, 24)
	dir := t.TempDir()
	for _, title := range []string{"alpha", "gamma", "delta", "epsilon"} {
		inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: dir, Program: "test"})
		require.NoError(t, err)
		addAgentShellTabs(inst)
		addTestInstance(s, inst)
	}
	naming, err := session.NewInstance(session.InstanceOptions{Title: draft, Path: dir, Program: "test"})
	require.NoError(t, err)
	require.NoError(t, naming.Transition(session.BeginCreate()))
	addTestInstance(s, naming)
	proj.SetNaming(naming)
	s.SelectInstance(naming)
	return s, proj, naming
}

func requireRailOnNamingRow(t *testing.T, s *Sidebar, proj *store.Projection, naming *session.Instance, step string) {
	t.Helper()
	_ = s.String()
	got := s.GetSelectedInstance()
	require.Truef(t, got == naming, "%s: the rail cursor left the naming row for %q", step, titleOrNil(got))
	bound := proj.GetSelectedInstance()
	require.Truef(t, bound == naming, "%s: the display binding left the naming row for %q", step, titleOrNil(bound))
	require.Zerof(t, tabRowCount(s), "%s: no session expands while the naming row is the selection", step)
}

func titleOrNil(inst *session.Instance) string {
	if inst == nil {
		return "<nil>"
	}
	return inst.Title
}

// TestNamingRowDraftTitleKeepsTheRailCursor is #7060 at the rail: a draft that
// spells an existing session's title used to auto-expand that session as if it
// were the selection. Its tab rows pushed the naming row down, and the cursor,
// which keeps its flat index across a rebuild, committed the row that slid into
// that index.
func TestNamingRowDraftTitleKeepsTheRailCursor(t *testing.T) {
	s, proj, naming := namingRail(t, "")
	requireRailOnNamingRow(t, s, proj, naming, "form opened")
	for _, draft := range []string{"alph", "alpha", "alphax", "delta", "d"} {
		require.NoError(t, naming.SetTitle(draft))
		requireRailOnNamingRow(t, s, proj, naming, "draft "+draft)
	}
}

// TestRestoredDraftWithATakenTitleSelectsTheNamingRow is the retry after a
// daemon-side conflict: the form reopens on a draft that already carries an
// existing session's title, so the select itself is the first rebuild that
// must not read the two as one row.
func TestRestoredDraftWithATakenTitleSelectsTheNamingRow(t *testing.T) {
	s, proj, naming := namingRail(t, "delta")
	requireRailOnNamingRow(t, s, proj, naming, "restored draft")
}
