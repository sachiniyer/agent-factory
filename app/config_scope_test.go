package app

import (
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// The config editor's scope picker (`p` inside `,`) — the TUI half of
// config.read-project. The daemon-side and pane-side contracts live in
// daemon/ and ui/ tests; these pin the app wiring: the union the picker
// offers, the state it returns to, and the read a submit performs.

// localConfigScopeTarget pins the test at the local target: a leaked
// --daemon-url/AF_DAEMON_URL from the developer's shell must not make the
// picker dial a real daemon.
func localConfigScopeTarget(t *testing.T) {
	t.Helper()
	prevURL, prevToken := apiclient.FlagDaemonURL, apiclient.FlagDaemonToken
	apiclient.FlagDaemonURL, apiclient.FlagDaemonToken = "", ""
	t.Cleanup(func() { apiclient.FlagDaemonURL, apiclient.FlagDaemonToken = prevURL, prevToken })
	t.Setenv("AF_DAEMON_URL", "")
	t.Setenv("AF_DAEMON_TOKEN", "")
}

// TestConfigScopePickerOffersTheProjectUnion: the picker rows are the global
// scope plus every project the daemon host knows — registry, session roots —
// sorted deterministically, with the currently-shown scope preselected.
func TestConfigScopePickerOffersTheProjectUnion(t *testing.T) {
	localConfigScopeTarget(t)
	h := newTestHome(t)

	registered := initTestGitRepo(t)
	_, err := config.RegisterProject(registered)
	require.NoError(t, err)

	sessionRepo := initTestGitRepo(t)
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func() ([]session.InstanceData, error) {
		return []session.InstanceData{{
			Title:    "live",
			Path:     sessionRepo,
			Worktree: session.GitWorktreeData{RepoPath: sessionRepo},
		}}, nil
	}))

	h.configPane.SetEntries(config.ManifestWithValues(config.DefaultConfig()), "/tmp/config.toml", "")
	h.configPane.SetFocus(true)
	h.state = stateConfigEditor

	model, _ := h.showConfigScopePicker()
	hm := model.(*home)
	require.Equal(t, stateConfigScope, hm.state)
	require.NotNil(t, hm.selectionOverlay)

	roots := make([]string, 0, len(hm.configScopePickerChoices))
	for _, c := range hm.configScopePickerChoices {
		roots = append(roots, c.root)
	}
	// Global first, then both project roots sorted — a registered-but-sessionless
	// project and a session-root the registry never saw must BOTH be offered.
	want := []string{"", registered, sessionRepo}
	if registered > sessionRepo {
		want = []string{"", sessionRepo, registered}
	}
	assert.Equal(t, want, roots)
}

// TestConfigScopeSubmitReadsTheProject: submitting a project row re-reads the
// editor through ReadConfigForEditor — the pane lands on the resolved root and
// shows the in-repo layer's value, not the global file's.
func TestConfigScopeSubmitReadsTheProject(t *testing.T) {
	localConfigScopeTarget(t)
	h := newTestHome(t)

	repo := initTestGitRepo(t)
	// Register it so the picker offers the row; an unregistered repo is only
	// reachable when a session's worktree names it.
	_, err := config.RegisterProject(repo)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".agent-factory"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(repo, ".agent-factory", config.TomlConfigFileName),
		[]byte("default_program = \"codex\"\n"), 0o644))

	// Global file carries a DIFFERENT program so a scoped read that silently
	// answered global fails on the value.
	homeDir, err := config.GetConfigDir()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(homeDir, config.TomlConfigFileName),
		[]byte("default_program = \"claude\"\n"), 0o600))

	h.configPane.SetEntries(config.ManifestWithValues(config.DefaultConfig()), "/tmp/config.toml", "")
	h.configPane.SetFocus(true)
	h.state = stateConfigEditor

	model, _ := h.showConfigScopePicker()
	hm := model.(*home)

	// Select the project row (index 1 — after the global row) and submit.
	hm.selectionOverlay.SetSelectedIndex(1)
	require.Equal(t, repo, hm.configScopePickerChoices[1].root)
	model, _ = hm.handleStateConfigScope(tea.KeyMsg{Type: tea.KeyEnter})
	hm = model.(*home)

	require.Equal(t, stateConfigEditor, hm.state, "submit returns to the editor")
	assert.Equal(t, repo, hm.configPane.ScopeRoot())
	// The pane shows the in-repo override, not the global file — a scoped read
	// that silently answered global would render "claude" here.
	hm.configPane.SetSize(160, 60)
	assert.Contains(t, hm.configPane.String(), "codex")
	assert.Contains(t, hm.configPane.String(), "read-only")
}

// TestConfigScopeCancelLeavesTheEditor: Esc closes only the picker — the pane
// keeps the scope it had, with no re-read.
func TestConfigScopeCancelLeavesTheEditor(t *testing.T) {
	localConfigScopeTarget(t)
	h := newTestHome(t)
	h.configPane.SetEntries(config.ManifestWithValues(config.DefaultConfig()), "/tmp/config.toml", "")
	h.configPane.SetFocus(true)
	h.state = stateConfigEditor

	model, _ := h.showConfigScopePicker()
	hm := model.(*home)
	require.Equal(t, stateConfigScope, hm.state)

	model, _ = hm.handleStateConfigScope(tea.KeyMsg{Type: tea.KeyEsc})
	hm = model.(*home)
	assert.Equal(t, stateConfigEditor, hm.state)
	assert.Nil(t, hm.selectionOverlay)
	assert.Equal(t, "", hm.configPane.ScopeRoot(), "a cancelled pick leaves the global scope")
}

// TestConfigScopeKeyOpensThePicker: the `p` key inside the editor surfaces as a
// scope request the app routes into the picker — the editor stays open
// underneath (a sub-state, not a close).
func TestConfigScopeKeyOpensThePicker(t *testing.T) {
	localConfigScopeTarget(t)
	h := newTestHome(t)
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func() ([]session.InstanceData, error) {
		return nil, nil
	}))
	h.configPane.SetEntries(config.ManifestWithValues(config.DefaultConfig()), "/tmp/config.toml", "")
	h.configPane.SetFocus(true)
	h.state = stateConfigEditor

	model, _ := h.handleStateConfigEditor(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	hm := model.(*home)
	assert.Equal(t, stateConfigScope, hm.state)
	assert.NotNil(t, hm.selectionOverlay)
}
