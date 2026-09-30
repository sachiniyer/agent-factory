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

// TestNamingPreCheckJudgesAnExistingSessionByItsRecordedBranch: a session made
// before the project's override keeps the branch it was created with, so the
// pre-check must compare against that branch, as the daemon does, rather than
// re-derive it under the override. "x" holds the global branch; "-x" derives
// proj-x, which nothing holds.
func TestNamingPreCheckJudgesAnExistingSessionByItsRecordedBranch(t *testing.T) {
	h, overridden := namingHomeWithProjectPrefix(t)
	recorded := git.BranchForTitle(h.appConfig.BranchPrefix, "x")

	_, notice := submitNamingBesideClaim(t, h, overridden, "x", recorded, "-x")
	assert.NotContains(t, notice, "conflicts with existing session",
		"x holds %q and -x derives proj-x, so the pre-check must not refuse", recorded)
}

// TestNamingPreCheckKeepsOffBoxCreatesOnTheGlobalPrefix: a Docker, SSH, hook, or
// sandbox create makes its branch inside the sandbox, which the host project's
// override does not reach, so the pre-check judges it under the global prefix,
// as the daemon does.
func TestNamingPreCheckKeepsOffBoxCreatesOnTheGlobalPrefix(t *testing.T) {
	h, overridden := namingHomeWithProjectPrefix(t)
	h.pendingBackend = string(session.BackendDocker)

	_, notice := submitNamingBesideClaim(t, h, overridden, "x", "", "-x")
	assert.NotContains(t, notice, "conflicts with existing session",
		"under the global prefix x and -x derive different branches; only the host override made them collide")
}

// TestNamingPreCheckJudgesAnUnclassifiedPendingRowAsOffBox: a pending-create
// row reaches a client before its backend exists, and from an older daemon it
// can arrive carrying no backend discriminator at all. Materialized without
// one it reads as a host-local claim, which would let the project's override
// refuse a title the daemon admits under the global off-box rule — here, a
// pending Docker "x" beside a local candidate "-x" (#4562 review).
func TestNamingPreCheckJudgesAnUnclassifiedPendingRowAsOffBox(t *testing.T) {
	h, overridden := namingHomeWithProjectPrefix(t)

	pending, err := session.FromInstanceData(session.InstanceData{
		Title:      "x",
		Path:       overridden,
		Status:     session.Loading,
		Liveness:   session.LiveReady,
		InFlightOp: session.OpCreating,
		Worktree:   session.GitWorktreeData{RepoPath: overridden, WorktreePath: overridden, BranchName: "global/x"},
	})
	require.NoError(t, err)
	h.store.AddInstance(pending)

	naming, err := session.NewInstance(session.InstanceOptions{Title: "-x", Path: overridden, Program: "claude"})
	require.NoError(t, err)
	h.namingInstance = naming
	h.state = stateNew

	_, _ = h.handleStateNew(tea.KeyMsg{Type: tea.KeyEnter})
	assert.NotContains(t, h.errBox.String(), "conflicts with existing session",
		"an unclassified pending row is judged under the global prefix, under which x and -x derive different branches")
}

// namingHomeWithProjectPrefix is a home switched into a project whose personal
// config sets branch_prefix "proj-".
func namingHomeWithProjectPrefix(t *testing.T) (*home, string) {
	t.Helper()
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
	h.switchProject(&config.RepoContext{Root: overridden, ID: config.RepoIDFromRoot(overridden)})
	require.True(t, git.TitlesCollide("x", "-x", h.namingBranchPrefix()),
		"precondition: under the project's prefix the two titles derive one branch")
	return h, overridden
}

// submitNamingBesideClaim is submitNamingBeside with the existing session's
// recorded branch set, as a started session's is.
func submitNamingBesideClaim(t *testing.T, h *home, repoRoot, existing, branch, naming string) (bool, string) {
	t.Helper()
	prior, err := session.NewInstance(session.InstanceOptions{Title: existing, Path: repoRoot, Program: "claude"})
	require.NoError(t, err)
	prior.Branch = branch
	h.store.AddInstance(prior)
	pending, err := session.NewInstance(session.InstanceOptions{Title: naming, Path: repoRoot, Program: "claude"})
	require.NoError(t, err)
	h.namingInstance = pending
	h.state = stateNew

	_, _ = h.handleStateNew(tea.KeyMsg{Type: tea.KeyEnter})
	return h.state == stateNew, h.errBox.String()
}

// submitNamingBeside presses Enter on a naming form titled naming while a session
// titled existing is in the rail, and reports whether the pre-check kept the form
// open, along with the notice it showed.
func submitNamingBeside(t *testing.T, h *home, repoRoot, existing, naming string) (bool, string) {
	t.Helper()
	return submitNamingBesideClaim(t, h, repoRoot, existing, "", naming)
}
