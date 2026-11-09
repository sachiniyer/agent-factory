package ui

import (
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/config"
)

// The `,` editor's project rows (#7009). The pane used to render the global
// value of a project-overridden key as though it were authoritative. Now the row
// shows the effective value with a scope marker, the selected row says where the
// global value went, and the field still edits the GLOBAL value — so these pin
// both halves: what the user reads, and what an edit would write.

// scopedEntries is the default manifest with default_program marked the way
// the daemon marks a checked-in override. The marker fields are decoded from
// JSON, not assigned, so the case reads the wire exactly as a remote pane does.
func scopedEntries(t *testing.T, scope string) []config.ConfigEntry {
	t.Helper()
	entries := config.ManifestWithValues(config.DefaultConfig())
	for i := range entries {
		if entries[i].Key != "default_program" {
			continue
		}
		wire := `{"key":"default_program","value":"aider","scope":"` + scope + `","global_value":"codex"}`
		var marked config.ConfigEntry
		if err := json.Unmarshal([]byte(wire), &marked); err != nil {
			t.Fatal(err)
		}
		entries[i].Value = marked.Value
		entries[i].Scope = marked.Scope
		entries[i].GlobalValue = marked.GlobalValue
		return entries
	}
	t.Fatal("default_program is not in the manifest")
	return nil
}

func scopedPane(t *testing.T, scope string, project *config.RepoContext) *ConfigPane {
	t.Helper()
	c := NewConfigPane()
	c.SetSize(200, 200)
	c.SetEntries(scopedEntries(t, scope), "/tmp/config.toml", project)
	c.SetFocus(true)
	c.showAdvanced = true
	c.rebuildRows()
	return c
}

func scopedEntry(t *testing.T, c *ConfigPane) config.ConfigEntry {
	t.Helper()
	for _, e := range c.entries {
		if e.Key == "default_program" {
			return e
		}
	}
	t.Fatal("default_program is not among the pane's entries")
	return config.ConfigEntry{}
}

func TestConfigPaneScopeMarkerRidesTheEffectiveValue(t *testing.T) {
	for scope, marker := range map[string]string{
		config.ScopeProject:         "aider (project)",
		config.ScopePersonal:        "aider (personal)",
		config.ScopeProjectPersonal: "aider (project+personal)",
		config.ScopePersonalIgnored: "aider (personal: stored)",
		// An open set: a layer a future daemon adds must still read as "not
		// the global value", never as an unmarked global row.
		"team": "aider (project-scoped)",
	} {
		t.Run(scope, func(t *testing.T) {
			c := scopedPane(t, scope, nil)
			if out := c.String(); !strings.Contains(out, marker) {
				t.Fatalf("the row must show %q:\n%s", marker, out)
			}
		})
	}
}

func TestConfigPaneUnmarkedRowCarriesNoScopeMarker(t *testing.T) {
	c := newTestConfigPane(t)
	out := c.String()
	for _, marker := range []string{"(project)", "(personal)", "(project-scoped)", "(personal: stored)"} {
		if strings.Contains(out, marker) {
			t.Fatalf("a global read must carry no scope marker, found %q:\n%s", marker, out)
		}
	}
}

func TestConfigPaneSelectedScopedRowSaysTheEditIsGlobal(t *testing.T) {
	for scope, note := range map[string]string{
		config.ScopeProject:         "project override · global: codex · enter edits the global value",
		config.ScopePersonal:        "personal override · global: codex · enter edits the global value",
		config.ScopeProjectPersonal: "project + personal overrides · global: codex · enter edits the global value",
		config.ScopePersonalIgnored: "a personal override is stored but not applied — this project uses the global value",
		"team":                      "project-scoped override · global: codex · enter edits the global value",
	} {
		t.Run(scope, func(t *testing.T) {
			c := scopedPane(t, scope, nil)
			selectKey(t, c, "default_program")
			if out := c.String(); !strings.Contains(out, note) {
				t.Fatalf("the selected row must say %q:\n%s", note, out)
			}
		})
	}
}

// TestConfigPaneScopedEditSeedsTheGlobalValue pins the write half. The field
// edits the global file, so it must open on the global value: seeding it with
// the project's effective value would let an untouched-looking save copy the
// override into the global file.
func TestConfigPaneScopedEditSeedsTheGlobalValue(t *testing.T) {
	c := scopedPane(t, config.ScopeProject, nil)
	selectKey(t, c, "default_program")
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	if !c.IsEditing() {
		t.Fatal("enter did not open the value field")
	}
	if got := c.EditValueForTest(); got != "codex" {
		t.Fatalf("the field opened on %q, want the global value %q it edits", got, "codex")
	}
}

func TestConfigPaneScopedUntouchedSaveWritesNothing(t *testing.T) {
	c := scopedPane(t, config.ScopeProject, nil)
	saves := 0
	c.save = func(k, v string) (*config.SetResult, string, error) {
		saves++
		return &config.SetResult{Key: k, Value: v}, "", nil
	}
	selectKey(t, c, "default_program")
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	if saves != 0 {
		t.Fatalf("an untouched save of a marked row wrote %d time(s)", saves)
	}
}

// TestConfigPaneScopedSaveRefreshesTheGlobalValueNotTheEffectiveOne: a global
// write does not change what the project uses while its override stands, so
// the row keeps the effective value and only the global half moves.
func TestConfigPaneScopedSaveRefreshesTheGlobalValueNotTheEffectiveOne(t *testing.T) {
	c := scopedPane(t, config.ScopeProject, nil)
	var wrote string
	c.save = func(k, v string) (*config.SetResult, string, error) {
		wrote = v
		return &config.SetResult{Key: k, Value: v}, "", nil
	}
	selectKey(t, c, "default_program")
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
	c.input.SetValue("")
	typeInto(c, "gemini")
	c.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})

	if wrote != "gemini" {
		t.Fatalf("the save carried %q, want the typed %q", wrote, "gemini")
	}
	e := scopedEntry(t, c)
	if e.Value != "aider" {
		t.Errorf("the row's effective value became %q; a global write does not change the project's %q", e.Value, "aider")
	}
	if e.GlobalValue != "gemini" {
		t.Errorf("the row's global value is %q, want the written %q", e.GlobalValue, "gemini")
	}
	if !strings.Contains(c.String(), "global: gemini") {
		t.Errorf("the selected row must show the new global value:\n%s", c.String())
	}
}

// TestConfigPaneExplainFollowsTheHonoredScope pins that 'e' explains the scope
// the rows were read in: a project trace beside project rows, a global trace
// beside a degraded (global) read. Answering a different scope than the rows
// show would contradict them.
func TestConfigPaneExplainFollowsTheHonoredScope(t *testing.T) {
	project := &config.RepoContext{Root: "/srv/app"}
	for _, tc := range []struct {
		name  string
		scope *config.RepoContext
	}{{"project", project}, {"global", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			c := scopedPane(t, config.ScopeProject, tc.scope)
			var asked *config.RepoContext
			called := false
			c.explain = func(key string, repo *config.RepoContext) (*config.ResolvedValue, error) {
				called = true
				asked = repo
				return explainFixture(), nil
			}
			selectKey(t, c, "default_program")
			pressKey(c, "e")
			if !called {
				t.Fatal("e did not explain the row")
			}
			if asked != tc.scope {
				t.Fatalf("explain was asked for scope %+v, want the rows' scope %+v", asked, tc.scope)
			}
			out := c.String()
			if got := strings.Contains(out, "project values"); got != (tc.scope != nil) {
				t.Fatalf("the explain subtitle's project note is %v, want %v:\n%s", got, tc.scope != nil, out)
			}
		})
	}
}

// TestConfigPaneScopeMarkerStaysInsideThePane extends the width invariant to the
// marker: the value preview yields cells to it, so a long effective value never
// pushes the row past the pane edge.
func TestConfigPaneScopeMarkerStaysInsideThePane(t *testing.T) {
	entries := scopedEntries(t, config.ScopeProjectPersonal)
	for i := range entries {
		if entries[i].Key == "default_program" {
			entries[i].Value = strings.Repeat("x", 300)
		}
	}
	for _, width := range []int{34, 60, 100} {
		c := NewConfigPane()
		c.SetSize(width, 200)
		c.SetEntries(entries, "/tmp/config.toml", nil)
		c.SetFocus(true)
		c.showAdvanced = true
		c.rebuildRows()
		for _, line := range strings.Split(c.String(), "\n") {
			if w := compositorWidth(line); w > width {
				t.Fatalf("width %d: a line is %d cells:\n%q", width, w, line)
			}
		}
		if width >= 60 && !strings.Contains(c.String(), "(project+personal)") {
			t.Fatalf("width %d: the marker was clipped off a long value:\n%s", width, c.String())
		}
	}
}
