package app

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
)

// TestNamingPreCheckUsesTheActiveProjectsBranchPrefix (#4539): the naming form's
// collision pre-check mirrors the daemon's rule, so it must derive branches with
// the prefix the daemon will use for this project — the personal override when
// the project sets one. "x" and "-x" derive one branch under "proj-" ("proj-x")
// but two under the global prefix. Switching to a project without an override
// must restore the global prefix rather than keep the previous project's, the
// same leak #2138 fixed for the default program.
func TestNamingPreCheckUsesTheActiveProjectsBranchPrefix(t *testing.T) {
	h := newTestHome(t)
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) {
		return daemon.SnapshotResponse{}, nil
	}
	h.errBox.SetSize(120, 1)
	t.Cleanup(SetSessionStarterForTest(func(*session.Instance, sessionStartRequest) (*session.Instance, error) {
		return nil, errors.New("test: the naming pre-check must not start a session")
	}))
	require.False(t, git.TitlesCollide("x", "-x", h.appConfig.BranchPrefix),
		"precondition: the global prefix keeps these two titles on different branches")

	overridden := initTestGitRepo(t)
	project, err := config.RegisterProject(overridden)
	require.NoError(t, err)
	_, err = config.SetProjectConfigValue(project.ID, "branch_prefix", "proj-")
	require.NoError(t, err)
	plain := initTestGitRepo(t)

	h.switchProject(&config.RepoContext{Root: overridden, ID: config.RepoIDFromRoot(overridden)})
	refused, notice := submitNamingBeside(t, h, overridden, "x", "-x")
	assert.True(t, refused, "under the project's prefix both titles derive proj-x, so the form must stay open")
	assert.Contains(t, notice, `"x"`, "the notice must name the session the title collides with")

	h.switchProject(&config.RepoContext{Root: plain, ID: config.RepoIDFromRoot(plain)})
	refused, _ = submitNamingBeside(t, h, plain, "x", "-x")
	assert.False(t, refused, "a project with no override must use the global prefix, not the previous project's")
}

// submitNamingBeside presses Enter on a naming form titled naming while a session
// titled existing is in the rail, and reports whether the pre-check kept the form
// open, along with the notice it showed.
func submitNamingBeside(t *testing.T, h *home, repoRoot, existing, naming string) (bool, string) {
	t.Helper()
	prior, err := session.NewInstance(session.InstanceOptions{Title: existing, Path: repoRoot, Program: "claude"})
	require.NoError(t, err)
	h.store.AddInstance(prior)
	pending, err := session.NewInstance(session.InstanceOptions{Title: naming, Path: repoRoot, Program: "claude"})
	require.NoError(t, err)
	h.namingInstance = pending
	h.state = stateNew

	_, _ = h.handleStateNew(tea.KeyMsg{Type: tea.KeyEnter})
	return h.state == stateNew, h.errBox.String()
}
