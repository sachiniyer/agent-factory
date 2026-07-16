package app

import (
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/ui"
	"github.com/sachiniyer/agent-factory/ui/layout"
)

// projectsHomeWithRows returns a home focused on the Projects section with two
// project rows and one session in the store, ready to exercise the #1620
// captive-vim-list input model.
func projectsHomeWithRows(t *testing.T) *home {
	t.Helper()
	h := newTestHome(t)
	// One session so the `/` search overlay has something to open on (it errors
	// out with zero sessions and never enters stateSearch).
	h.store.AddInstance(newSnapshotTestInstance(t, "alpha"))
	h.projects.SetProjects([]ui.SidebarProject{
		{Name: filepath.Base(h.repoRoot), Root: h.repoRoot, SessionCount: 1, Active: true},
		{Name: "zzz-other", Root: "/repos/zzz-other", SessionCount: 0},
	})
	h.focusRegion(layout.RegionProjects)
	require.Equal(t, layout.RegionProjects, h.ring.Active())
	require.Equal(t, stateDefault, h.state)
	return h
}

// TestProjectsSection_VimKeysScrollList: with the section focused, j/k (and the
// arrow aliases) move the cursor through the project list and are consumed by
// the section — the #1620 vim-scroll requirement, matching how the Instances
// tree navigates with the same up=k/down=j bindings.
func TestProjectsSection_VimKeysScrollList(t *testing.T) {
	h := projectsHomeWithRows(t)

	start, ok := h.projects.SelectedProject()
	require.True(t, ok)

	// `j` steps the cursor DOWN onto the next project and is consumed.
	_, _, consumed := h.handleProjectsFocus(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	require.True(t, consumed, "j is consumed by the focused Projects section")
	down, _ := h.projects.SelectedProject()
	require.NotEqual(t, start.Root, down.Root, "j moved the cursor to the next project")

	// `k` steps back UP to where it started and is consumed.
	_, _, consumed = h.handleProjectsFocus(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	require.True(t, consumed, "k is consumed by the focused Projects section")
	up, _ := h.projects.SelectedProject()
	require.Equal(t, start.Root, up.Root, "k moved the cursor back up")

	// The arrow aliases behave identically.
	_, _, consumed = h.handleProjectsFocus(tea.KeyMsg{Type: tea.KeyDown})
	require.True(t, consumed, "Down is consumed too")
	_, _, consumed = h.handleProjectsFocus(tea.KeyMsg{Type: tea.KeyUp})
	require.True(t, consumed, "Up is consumed too")

	assert.Equal(t, stateDefault, h.state, "nav never opens an overlay")
}

// TestProjectsSection_SearchOnlyOnSlash: `/` is the ONLY key that enters search
// from the Projects section (#1620). It opens the session search overlay; every
// other key is consumed as a no-op and can never start a search/filter.
func TestProjectsSection_SearchOnlyOnSlash(t *testing.T) {
	h := projectsHomeWithRows(t)

	// `/` enters search.
	_, _, consumed := h.handleProjectsFocus(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	require.True(t, consumed, "/ is consumed by the section")
	require.NotNil(t, h.searchOverlay, "/ opens the search overlay")
	assert.Equal(t, stateSearch, h.state, "/ is the search entry key")
}

// TestProjectsSection_OtherKeysAreInert: keys that used to leak through the old
// fall-through — the ctrl+p project-picker filter, `n` new session, `m` tasks,
// `h`/`l` collapse/expand — are now consumed as no-ops while Projects holds
// focus (#1620). None of them opens an overlay, changes state, or creates a
// session, so no key other than `/` can begin a search/filter from the section.
func TestProjectsSection_OtherKeysAreInert(t *testing.T) {
	inert := []struct {
		name string
		msg  tea.KeyMsg
	}{
		{"ctrl+p project picker (a filter)", tea.KeyMsg{Type: tea.KeyCtrlP}},
		{"n new session", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}}},
		{"m tasks", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}}},
		{"h collapse", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}}},
		{"l expand", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}}},
		{"e hooks", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}}},
		{"an unbound letter", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'z'}}},
	}
	for _, tc := range inert {
		t.Run(tc.name, func(t *testing.T) {
			h := projectsHomeWithRows(t)
			before := h.store.NumInstances()

			_, _, consumed := h.handleProjectsFocus(tc.msg)

			require.True(t, consumed, "%s is consumed as a no-op, not passed to the global map", tc.name)
			assert.Equal(t, stateDefault, h.state, "%s must not open any overlay", tc.name)
			assert.Nil(t, h.searchOverlay, "%s must not open a search overlay", tc.name)
			assert.Nil(t, h.projectPickerOverlay, "%s must not open the project-picker filter", tc.name)
			assert.Equal(t, before, h.store.NumInstances(), "%s must not create a session", tc.name)
			assert.Equal(t, layout.RegionProjects, h.ring.Active(), "%s must not steal focus off the section", tc.name)
		})
	}
}

// TestProjectsSection_ChromeFallsThrough: the focus-ring and hard-exit chrome
// (Tab, Shift-Tab, ? help, q quit, ctrl+c) must still fall through to the global
// handler so the user is never trapped in the captive section (#1620).
func TestProjectsSection_ChromeFallsThrough(t *testing.T) {
	chrome := []struct {
		name string
		msg  tea.KeyMsg
	}{
		{"Tab", tea.KeyMsg{Type: tea.KeyTab}},
		{"Shift-Tab", tea.KeyMsg{Type: tea.KeyShiftTab}},
		{"? help", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}}},
		{"q quit", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}}},
		{"ctrl+c", tea.KeyMsg{Type: tea.KeyCtrlC}},
	}
	for _, tc := range chrome {
		t.Run(tc.name, func(t *testing.T) {
			h := projectsHomeWithRows(t)
			_, _, consumed := h.handleProjectsFocus(tc.msg)
			require.False(t, consumed, "%s must fall through to the global handler", tc.name)
		})
	}
}
