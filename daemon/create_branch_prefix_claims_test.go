package daemon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// A session keeps the branch it was created with when branch_prefix changes
// later (#4539), so admission must judge existing claims by what they hold, not
// by re-deriving them under today's prefix. These tests reuse the "#x"/"-x" pair
// from TestProjectBranchPrefixCollisionCheckUsesTheProjectsPrefix: one branch
// under "proj-" ("proj-x"), two under "global/" ("global/x", "global/-x"), and
// distinct tmux and archive-directory names, so only the branch rule decides.

func repoIDFor(t *testing.T, repoPath string) string {
	t.Helper()
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	return repo.ID
}

// TestProjectBranchPrefixExistingSessionIsJudgedByItsRecordedBranch: "#x" was
// created under "global/" and holds global/x. After the project switches to
// "proj-", "-x" derives proj-x, which nothing holds, so the create goes ahead.
// Re-deriving "#x" under "proj-" would invent a proj-x claim and refuse it.
//
// TestProjectBranchPrefixCollisionCheckUsesTheProjectsPrefix is the control: its
// "#x" has no recorded branch (the fake backend records none), so it is still
// derived under the current prefix and still refuses "-x".
func TestProjectBranchPrefixExistingSessionIsJudgedByItsRecordedBranch(t *testing.T) {
	m, first, _, firstProject := projectBranchPrefixFixture(t)
	rec := installBranchPrefixRecorder(t)
	repoID := repoIDFor(t, first)

	require.NoError(t, createWithTitle(m, first, "#x"))
	require.Equal(t, "global/", rec.prefix(t, "#x"))
	m.mu.Lock()
	inst := m.instances[daemonInstanceKey(repoID, "#x")]
	m.mu.Unlock()
	require.NotNil(t, inst, "the first create must be loaded")
	// What a real local create records at provision (instance_backend.go); the
	// fake backend records nothing.
	inst.Branch = "global/x"
	require.NoError(t, m.persistInstanceErr(repoID, inst))

	setProjectBranchPrefix(t, firstProject, "proj-")

	require.NoError(t, createWithTitle(m, first, "-x"),
		"#x holds global/x and -x derives proj-x, so the two do not collide")
	assert.Equal(t, "proj-", rec.prefix(t, "-x"))
}

// TestProjectBranchPrefixReservationKeepsTheBranchItReserved: an in-flight
// create is judged by the branch it derived when admitted, not by a
// re-derivation under a prefix saved while it was still provisioning.
func TestProjectBranchPrefixReservationKeepsTheBranchItReserved(t *testing.T) {
	m, first, _, firstProject := projectBranchPrefixFixture(t)

	_, _, releaseFirst, _, err := m.reserveCreate(CreateSessionRequest{RepoPath: first, Title: "#x", Program: "claude"})
	require.NoError(t, err)
	defer releaseFirst()

	setProjectBranchPrefix(t, firstProject, "proj-")

	_, _, releaseSecond, _, err := m.reserveCreate(CreateSessionRequest{RepoPath: first, Title: "-x", Program: "claude"})
	require.NoError(t, err, "the in-flight #x reserved global/x and -x derives proj-x, so they do not collide")
	releaseSecond()
}

// TestProjectBranchPrefixUnclaimedReservationIsDerived is the control for the
// test above: a reservation with no recorded claim (the shape older unit tests
// seed) is derived under the current prefix, and proj-x collides.
func TestProjectBranchPrefixUnclaimedReservationIsDerived(t *testing.T) {
	m, first, _, _ := projectBranchPrefixFixture(t)
	repoID := repoIDFor(t, first)
	naming := branchNaming{prefix: "proj-", global: "global/", local: true}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.reservedTitles[daemonInstanceKey(repoID, "#x")] = struct{}{}
	existing, _, _, branch := m.findTitleConflictLocked(naming, repoID, first, "-x", true, nil)
	assert.Equal(t, "#x", existing)
	assert.Equal(t, "proj-x", branch)
}

// TestProjectBranchPrefixArchivedBranchMovesOnlyWhenItBlocks: an archived "foo"
// holds global/foo. After the project switches to "new/", reusing the title
// "foo" creates new/foo, which the archived branch is not in the way of, so the
// archived session is renamed (titles are unique) but its branch stays put.
func TestProjectBranchPrefixArchivedBranchMovesOnlyWhenItBlocks(t *testing.T) {
	m, first, _, firstProject := projectBranchPrefixFixture(t)
	repoID := repoIDFor(t, first)
	archived, _ := seedArchivedSession(t, m, repoID, first, "foo", "foo")
	require.Equal(t, "global/foo", archived.GetBranch())

	setProjectBranchPrefix(t, firstProject, "new/")

	_, title, release, renamed, err := m.reserveCreate(CreateSessionRequest{RepoPath: first, Title: "foo", Program: "claude"})
	require.NoError(t, err)
	defer release()
	assert.Equal(t, "foo", title)
	require.NotNil(t, renamed, "the archived session must still give up the title")
	assert.Equal(t, "global/foo", renamed.Branch,
		"the archived branch does not block new/foo, so it must not be renamed")
	assert.Equal(t, "global/foo", archived.GetBranch())
}

// TestOffBoxCreateIgnoresTheHostProjectsBranchPrefix: a Docker, SSH, hook, or
// sandbox session makes its branch inside the sandbox from that machine's
// config. The host project's override does not reach it, so it must not decide
// admission either: an off-box create keeps the global prefix, as it did before
// per-project prefixes existed.
func TestOffBoxCreateIgnoresTheHostProjectsBranchPrefix(t *testing.T) {
	m, first, _, firstProject := projectBranchPrefixFixture(t)
	setProjectBranchPrefix(t, firstProject, "proj-")

	firstRes, err := m.reserveCreateWithWorktreeAdmission(CreateSessionRequest{
		RepoPath: first, Title: "#x", Program: "claude", Backend: string(session.BackendDocker),
	}, m.Config(), false)
	require.NoError(t, err)
	defer firstRes.release()
	assert.Equal(t, "global/", firstRes.naming.prefix,
		"an off-box create must not take the host project's override")

	second, err := m.reserveCreateWithWorktreeAdmission(CreateSessionRequest{
		RepoPath: first, Title: "-x", Program: "claude", Backend: string(session.BackendDocker),
	}, m.Config(), false)
	require.NoError(t, err,
		"under the global prefix #x and -x derive different branches; only the host override made them collide")
	second.release()

	local, err := m.reserveCreateWithWorktreeAdmission(CreateSessionRequest{
		RepoPath: first, Title: "local", Program: "claude",
	}, m.Config(), false)
	require.NoError(t, err)
	local.release()
	assert.Equal(t, "proj-", local.naming.prefix, "control: a local create in the same project does take it")
}

// TestCreateSessionProvisionsTheBackendAdmissionResolved: a create that leaves
// the backend to the repo's `backend` key resolves it ONCE, at admission, and
// must provision THAT answer. Re-resolving inside NewInstance reads the repo
// config a second time, so a save landing between the two would pair one
// runtime's naming snapshot with another's provisioning — a local worktree
// named under the global prefix, or the reverse (#4562 review). Stubbing the
// admission resolver to docker proves the pin: the instance factory receives
// the resolved kind even though the request's backend field is empty and the
// repo declares none.
func TestCreateSessionProvisionsTheBackendAdmissionResolved(t *testing.T) {
	m, first, _, _ := projectBranchPrefixFixture(t)

	prev := backendKindForCreate
	backendKindForCreate = func(session.InstanceOptions, string) (session.BackendKind, error) {
		return session.BackendDocker, nil
	}
	t.Cleanup(func() { backendKindForCreate = prev })

	var gotBackend session.BackendKind
	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, _ string) (session.Backend, error) {
		gotBackend = opts.Backend
		backend := session.NewFakeBackend()
		backend.CompleteStart()
		return readyFakeBackend{backend}, nil
	})
	defer restore()

	_, _ = m.CreateSession(context.Background(), CreateSessionRequest{
		RepoPath: first, Title: "dock", Program: "claude",
	})
	assert.Equal(t, session.BackendDocker, gotBackend,
		"NewInstance must receive the backend admission resolved, not re-resolve the request's empty value against live config")
}

// TestArchivedRowNeverRenamedDefendsItsRecordedBranch is the #4562-review half
// of relinquishment: an archived row that was NEVER renamed for title reuse
// still owns the branch its record points at. "#x" was created while the
// global prefix was "foo-", so it records "foo-x"; the prefix is now "foo",
// under which "-x" derives "foo-x" again. The row's title no longer derives
// its branch — but that mismatch is just the prefix change, not a yield, and
// treating it as relinquished would let the create adopt a branch the archived
// record still names, contaminating the archived history and making later
// restore ownership ambiguous.
func TestArchivedRowNeverRenamedDefendsItsRecordedBranch(t *testing.T) {
	m, first, _, _ := projectBranchPrefixFixture(t)
	repoID := repoIDFor(t, first)
	archived, _ := seedArchivedSession(t, m, repoID, first, "#x", "x")

	// The row was created under a different global prefix: its record keeps
	// the branch it was created with (#4539), which its title no longer
	// derives under the live one.
	archived.Branch = "foo-x"
	require.NoError(t, m.persistInstanceErr(repoID, archived))

	changed := *m.Config()
	changed.BranchPrefix = "foo"
	res, err := m.reserveCreateWithWorktreeAdmission(CreateSessionRequest{
		RepoPath: first, Title: "-x", Program: "claude",
	}, &changed, false)
	if res.release != nil {
		defer res.release()
	}
	require.Error(t, err,
		"-x derives foo-x, the branch the never-renamed archived row still records")
	assert.Contains(t, err.Error(), "foo-x")
}

// TestRenamedArchivedRowPersistsItsYieldedBranch is the other half: when the
// reuse rename deliberately leaves the recorded branch for the new session to
// adopt (#2127), that yield must persist — a daemon restart must not turn the
// renamed row back into a defender, or the next create that re-derives the
// branch is refused on a record that already gave it up.
func TestRenamedArchivedRowPersistsItsYieldedBranch(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	_, _ = seedArchivedSessionBranchFreed(t, manager, repoID, repoPath, "foo", "foo")

	_, _, release, renamed, err := manager.reserveCreate(CreateSessionRequest{
		RepoPath: repoPath, Title: "foo", Program: "claude",
	})
	require.NoError(t, err)
	defer release()
	require.NotNil(t, renamed, "the archived session must have been renamed to free the name")
	assert.True(t, renamed.RelinquishedBranch,
		"the rename left global/foo for the new session to adopt — that yield must ride the durable record")
	rec := recordFor(t, repoID, "foo (archived)")
	require.NotNil(t, rec)
	assert.True(t, rec.RelinquishedBranch,
		"and it must be on disk, not only on the live instance")
}

// TestMovedAsideArchivedBranchStaysDefended pins the middle case: a reuse
// rename that MOVES the branch aside gives the row a branch its new title
// derives again, but a later prefix change re-opens the mismatch arm — and the
// row still owns the moved branch, so RelinquishedBranch must stay false.
func TestMovedAsideArchivedBranchStaysDefended(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	_, _ = seedArchivedSession(t, manager, repoID, repoPath, "foo", "foo")

	_, _, release, renamed, err := manager.reserveCreate(CreateSessionRequest{
		RepoPath: repoPath, Title: "foo", Program: "claude",
	})
	require.NoError(t, err, "the reclaim moves the held branch aside with the title")
	defer release()
	require.NotNil(t, renamed)
	assert.False(t, renamed.RelinquishedBranch,
		"the rename moved global/foo to the row's new title — nothing was left for adoption")
	rec := recordFor(t, repoID, "foo (archived)")
	require.NotNil(t, rec)
	assert.False(t, rec.RelinquishedBranch)
}

// TestOffBoxRecordedBranchKeepsTheSanitizerCollision is the #4562-review
// sanitizer case: a settled off-box row can record a branch its sandbox derived
// under a prefix the host never saw. "A B" was archived, restored, and recorded
// "sandbox/a-b"; a new off-box "a-b" checked under the host's "global/" derives
// "global/a-b" — never the recorded string — yet a sandbox with the same
// configuration derives "sandbox/a-b" again, so admission must still refuse on
// the title collision rather than read the missed branch compare as "free".
func TestOffBoxRecordedBranchKeepsTheSanitizerCollision(t *testing.T) {
	m, first, _, _ := projectBranchPrefixFixture(t)
	repoID := repoIDFor(t, first)

	require.NoError(t, appendInstanceData(repoID, session.InstanceData{
		Title: "A B", Path: first, Branch: "sandbox/a-b",
		Status: session.Ready, Liveness: session.LiveReady, BackendType: "docker",
	}))

	res, err := m.reserveCreateWithWorktreeAdmission(CreateSessionRequest{
		RepoPath: first, Title: "a-b", Program: "claude", Backend: string(session.BackendDocker),
	}, m.Config(), false)
	if res.release != nil {
		defer res.release()
	}
	require.Error(t, err,
		"a second off-box session whose sandbox derives the same a-b ref must not be admitted")
}

// TestInPlaceCheckUsesTheAdmissionPinnedBackend is the #4562-review pin case:
// reserveCreate resolves the backend ONCE, and the in-place contradiction check
// must consume that answer. A repo-config save landing between the pin and a
// second resolution would split them — admission sees docker, a fresh read sees
// local — and the refusal would never fire, letting the archived-name rename
// stand for a create NewInstance then rejects.
func TestInPlaceCheckUsesTheAdmissionPinnedBackend(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	archived, _ := seedArchivedSessionBranchFreed(t, manager, repoID, repoPath, "foo", "foo")
	archivedPath := archived.GetWorktreePath()
	writeRepoBackendConfig(t, repoPath, map[string]any{"backend": "docker"})

	prev := backendKindForCreate
	backendKindForCreate = func(session.InstanceOptions, string) (session.BackendKind, error) {
		// The admission resolution reads docker; simulate the config save
		// landing between the pin and the contradiction check by flipping the
		// repo to local before any second resolution runs.
		writeRepoBackendConfig(t, repoPath, map[string]any{"backend": "local"})
		return session.BackendDocker, nil
	}
	t.Cleanup(func() { backendKindForCreate = prev })

	_, _, release, renamed, err := manager.reserveCreate(CreateSessionRequest{
		RepoPath: repoPath, Title: "foo", Program: "claude", InPlace: true,
	})
	if release != nil {
		defer release()
	}

	require.Error(t, err,
		"the admission pinned docker; the in-place check must refuse on THAT answer")
	assert.ErrorIs(t, err, session.ErrInPlaceRemoteBackend)
	assert.Nil(t, renamed, "the refusal must land before the archived-name reuse rename")
	assert.Equal(t, "foo", archived.Title)
	assert.Equal(t, archivedPath, archived.GetWorktreePath())
	assert.Nil(t, recordFor(t, repoID, "foo (archived)"),
		"no disambiguated archive row may exist: nothing was renamed")
}
