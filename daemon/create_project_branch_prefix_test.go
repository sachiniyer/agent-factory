package daemon

import (
	"context"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// Per-project branch_prefix (#4539). `af config set --project <p> branch_prefix`
// writes the project's personal layer, and the resolver has always ranked it
// above the global value — `af config get branch_prefix --repo <p> --explain`
// names it the winner — yet every create named its branch from the daemon's
// global value, so the override changed nothing a session got.

// branchPrefixRecorder captures the BranchPrefix each create hands NewInstance,
// keyed by title. That value is what LocalBackend names the new worktree's branch
// with (session's TestLocalBackendProvisionUsesResolvedBranchPrefixSnapshot pins
// that half), so it is the create's answer to "which branch does this session
// get". A nil pointer tells the backend to read config again when it provisions,
// which is a second read by definition, so it is recorded as its own value.
type branchPrefixRecorder struct {
	mu      sync.Mutex
	byTitle map[string]string
}

const unresolvedBranchPrefix = "<resolved again at provision>"

func installBranchPrefixRecorder(t *testing.T) *branchPrefixRecorder {
	t.Helper()
	rec := &branchPrefixRecorder{byTitle: map[string]string{}}
	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, _ string) (session.Backend, error) {
		rec.mu.Lock()
		if opts.BranchPrefix == nil {
			rec.byTitle[opts.Title] = unresolvedBranchPrefix
		} else {
			rec.byTitle[opts.Title] = *opts.BranchPrefix
		}
		rec.mu.Unlock()
		backend := session.NewFakeBackend()
		backend.CompleteStart()
		return readyFakeBackend{backend}, nil
	})
	t.Cleanup(restore)
	return rec
}

func (r *branchPrefixRecorder) prefix(t *testing.T, title string) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	got, ok := r.byTitle[title]
	require.True(t, ok, "the create for %q never reached NewInstance", title)
	return got
}

// projectBranchPrefixFixture is a daemon whose global branch_prefix is "global/",
// plus two registered projects, neither of which overrides it yet.
func projectBranchPrefixFixture(t *testing.T) (m *Manager, first, second string, firstProject config.Project) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	first = setupControlRepo(t)
	second = setupControlRepo(t)
	var err error
	firstProject, err = config.RegisterProject(first)
	require.NoError(t, err)
	_, err = config.RegisterProject(second)
	require.NoError(t, err)
	cfg := config.DefaultConfig()
	cfg.BranchPrefix = "global/"
	m, err = NewManager(cfg)
	require.NoError(t, err)
	return m, first, second, firstProject
}

// setProjectBranchPrefix makes the same write `af config set --project <p>
// branch_prefix <value>` makes.
func setProjectBranchPrefix(t *testing.T, project config.Project, value string) {
	t.Helper()
	_, err := config.SetProjectConfigValue(project.ID, "branch_prefix", value)
	require.NoError(t, err)
}

func createWithTitle(m *Manager, repoPath, title string) error {
	_, err := m.CreateSession(context.Background(), CreateSessionRequest{
		Title: title, RepoPath: repoPath, Program: "claude",
	})
	return err
}

// TestProjectBranchPrefixNamesTheNextSessionInThatProjectOnly is the issue's
// acceptance: an override changes the branch of the next session created in that
// project, on the daemon that is already running, and clearing it hands the
// project back to the global prefix. Another project keeps the global prefix the
// whole time.
func TestProjectBranchPrefixNamesTheNextSessionInThatProjectOnly(t *testing.T) {
	m, first, second, firstProject := projectBranchPrefixFixture(t)
	rec := installBranchPrefixRecorder(t)

	require.NoError(t, createWithTitle(m, first, "before-override"))
	assert.Equal(t, "global/", rec.prefix(t, "before-override"),
		"with no override, the project names branches with the global prefix")

	setProjectBranchPrefix(t, firstProject, "proj/")

	require.NoError(t, createWithTitle(m, first, "after-override"))
	assert.Equal(t, "proj/", rec.prefix(t, "after-override"),
		"the next session in the overridden project must take the project's branch_prefix, with no daemon restart")
	require.NoError(t, createWithTitle(m, second, "other-project"))
	assert.Equal(t, "global/", rec.prefix(t, "other-project"),
		"a project that sets no override keeps the global prefix")

	// The same write `af config unset --project <p> branch_prefix` makes.
	_, err := config.UnsetProjectConfigValue(firstProject.ID, "branch_prefix")
	require.NoError(t, err)
	require.NoError(t, createWithTitle(m, first, "after-unset"))
	assert.Equal(t, "global/", rec.prefix(t, "after-unset"),
		"clearing the override returns the project's next session to the global prefix")
}

// TestProjectBranchPrefixCollisionCheckUsesTheProjectsPrefix: the title-collision
// rule has to derive branches with the prefix the worktree will be created with.
// "x" and "-x" derive ONE branch under "proj-" (the dash run collapses, so both
// are "proj-x") but two under "global/" ("global/x" and "global/-x"). A check that
// still read the global prefix admits the second create, and its worktree then
// asks git for the branch the first session already has.
func TestProjectBranchPrefixCollisionCheckUsesTheProjectsPrefix(t *testing.T) {
	m, first, _, firstProject := projectBranchPrefixFixture(t)
	rec := installBranchPrefixRecorder(t)
	setProjectBranchPrefix(t, firstProject, "proj-")

	require.NoError(t, createWithTitle(m, first, "x"))
	require.Equal(t, "proj-", rec.prefix(t, "x"), "precondition: the first session is named under the project's prefix")

	err := createWithTitle(m, first, "-x")
	require.Error(t, err, "under the project's prefix both titles derive one branch, so the second create must be refused")
	assert.Contains(t, err.Error(), `both sanitize to the same git branch "proj-x"`,
		"the refusal must name the branch the create would actually have made")
}

// TestProjectBranchPrefixWorktreeUsesTheSnapshotAdmissionChecked pins the
// one-read rule: the worktree is named from the prefix admission checked, never
// from a later read. The override is rewritten from inside the held-branch probe,
// which admission reaches after its record-collision check and while it still
// holds the manager lock. A create that resolved branch_prefix a second time for
// the worktree would name a branch its checks never cleared.
func TestProjectBranchPrefixWorktreeUsesTheSnapshotAdmissionChecked(t *testing.T) {
	m, first, _, firstProject := projectBranchPrefixFixture(t)
	rec := installBranchPrefixRecorder(t)
	setProjectBranchPrefix(t, firstProject, "proj/")

	rewrites := 0
	prev := branchesHeldByWorktrees
	branchesHeldByWorktrees = func(repoRoot string) (map[string][]string, error) {
		if rewrites == 0 {
			setProjectBranchPrefix(t, firstProject, "rewritten/")
		}
		rewrites++
		return prev(repoRoot)
	}
	t.Cleanup(func() { branchesHeldByWorktrees = prev })

	require.NoError(t, createWithTitle(m, first, "snapshot"))
	require.Positive(t, rewrites, "anti-vacuous: the override must be rewritten while admission is running")
	assert.Equal(t, "proj/", rec.prefix(t, "snapshot"),
		"the worktree must be named with the prefix admission checked, not one read after it")
}

// TestProjectBranchPrefixDerivedTitleSkipsTheProjectsHeldBranch covers the
// title_base walk (task deliveries, auto-named creates), which skips a suffix
// whose derived branch a worktree already holds. It must derive those branches
// under the project's prefix too, or it hands out a name whose branch is taken.
func TestProjectBranchPrefixDerivedTitleSkipsTheProjectsHeldBranch(t *testing.T) {
	m, first, _, firstProject := projectBranchPrefixFixture(t)
	setProjectBranchPrefix(t, firstProject, "proj/")
	holder := filepath.Join(testguard.CanonicalTempDir(t), "holder")
	out, err := exec.Command("git", "-C", first, "worktree", "add", "-q", "-b", "proj/daily", holder).CombinedOutput()
	require.NoError(t, err, string(out))

	_, title, release, _, err := m.reserveCreate(CreateSessionRequest{
		RepoPath: first, TitleBase: "daily", Program: "claude",
	})
	require.NoError(t, err)
	release()
	assert.Equal(t, "daily-2", title,
		"the bare base derives proj/daily, which a worktree holds, so the walk must move to the next suffix")
}
