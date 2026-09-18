package ui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/config"
)

// scopeTestProjects is the registry the scope row offers in these tests.
var scopeTestProjects = []config.Project{
	{ID: "p1", Root: "/home/u/one", PathExists: true},
	{ID: "p2", Root: "/home/u/two", PathExists: true},
}

// scopedTestPane is newTestConfigPane with a project registry loaded, so the
// scope row exists to drive.
func scopedTestPane(t *testing.T) *ConfigPane {
	t.Helper()
	c := newTestConfigPane(t)
	c.SetScopes(scopeTestProjects)
	return c
}

func pressKey(c *ConfigPane, key string) {
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
}

// TestScopeRowOnlyRendersWithProjects: a global-only registry must not show a
// selector that goes nowhere — the row appears exactly when a project exists
// to scope to.
func TestScopeRowOnlyRendersWithProjects(t *testing.T) {
	c := newTestConfigPane(t)
	for _, row := range c.rows {
		if row.scope {
			t.Fatal("scope row rendered with no projects registered")
		}
	}
	c.SetScopes(scopeTestProjects)
	if !c.rows[0].scope {
		t.Fatal("scope row is not the first row once projects exist")
	}
}

// TestScopeRowRequestsTheProject: moving the scope row right asks the app for
// the project's read, and TakeScopeRequest hands it the project's root — the
// same selector `af config get --repo` takes.
func TestScopeRowRequestsTheProject(t *testing.T) {
	c := scopedTestPane(t)
	c.selectedIdx = 0
	if !c.onScopeRow() {
		t.Fatal("cursor is not on the scope row")
	}
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRight})
	idx, path, ok := c.TakeScopeRequest()
	if !ok || path != "/home/u/one" || idx != 1 {
		t.Fatalf("TakeScopeRequest = (%d, %q, %v), want (1, /home/u/one, true)", idx, path, ok)
	}
	// Read-and-clear: one keypress is one request.
	if _, _, ok := c.TakeScopeRequest(); ok {
		t.Fatal("TakeScopeRequest fired twice for one keypress")
	}
}

// TestScopeRowClampsAtTheEnds: the scope row never requests a scope that
// cannot exist — left of global and right of the last project are no-ops.
func TestScopeRowClampsAtTheEnds(t *testing.T) {
	c := scopedTestPane(t)
	c.selectedIdx = 0
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyLeft})
	if _, _, ok := c.TakeScopeRequest(); ok {
		t.Fatal("left of the global scope produced a request")
	}
	c.ApplyScope(2, nil, "/home/u/two")
	c.selectedIdx = 0
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyRight})
	if _, _, ok := c.TakeScopeRequest(); ok {
		t.Fatal("right of the last project produced a request")
	}
}

// TestProjectScopeIsReadOnly: an edit attempt in the project view must not
// open the value field — it would write the GLOBAL file the user is not
// looking at — and the refusal names the verb that does write a personal
// override.
func TestProjectScopeIsReadOnly(t *testing.T) {
	c := scopedTestPane(t)
	c.ApplyScope(1, config.ManifestWithValues(config.DefaultConfig()), "/home/u/one")
	if !c.IsProjectScope() {
		t.Fatal("ApplyScope did not enter the project scope")
	}
	selectKey(t, c, "default_program")
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	if c.IsEditing() {
		t.Fatal("the value field opened in the read-only project scope")
	}
	if !strings.Contains(c.status, "--project") {
		t.Fatalf("read-only refusal does not name the write verb: %q", c.status)
	}
}

// TestProjectScopeHidesTheAccountsSection: an account is a daemon-host
// credential, not a property of the repository being read — the section does
// not belong under the project view.
func TestProjectScopeHidesTheAccountsSection(t *testing.T) {
	c := scopedTestPane(t)
	c.accounts.loaded = true
	c.accounts.rows = []AccountRow{{Agent: "claude", Name: "work"}}
	c.rebuildRows()
	found := false
	for _, row := range c.rows {
		if row.heading == accountsHeading {
			found = true
		}
	}
	if !found {
		t.Fatal("accounts section missing in the global scope")
	}
	c.ApplyScope(1, config.ManifestWithValues(config.DefaultConfig()), "/home/u/one")
	for _, row := range c.rows {
		if row.heading == accountsHeading {
			t.Fatal("accounts section rendered in the project scope")
		}
	}
}

// TestScopeRequestFailedKeepsTheScope: a read the app could not complete
// leaves the pane on the scope it still has, with the reason in the status
// line rather than a silent refusal.
func TestScopeRequestFailedKeepsTheScope(t *testing.T) {
	c := scopedTestPane(t)
	c.ScopeRequestFailed(errors.New("no longer a git repository"))
	if c.IsProjectScope() {
		t.Fatal("a failed scope request changed the scope")
	}
	if !c.statusIsError || !strings.Contains(c.status, "no longer a git repository") {
		t.Fatalf("failure did not land in the status line: %q", c.status)
	}
}

// TestSetScopesResetsToGlobal: reopening the editor must show the predictable
// view — a pane that kept the last visit's project scope would surprise the
// user reading global config.
func TestSetScopesResetsToGlobal(t *testing.T) {
	c := scopedTestPane(t)
	c.ApplyScope(1, config.ManifestWithValues(config.DefaultConfig()), "/home/u/one")
	c.SetScopes(scopeTestProjects)
	if c.IsProjectScope() {
		t.Fatal("SetScopes kept a stale project scope")
	}
}
