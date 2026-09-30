package daemon

import (
	"context"
	"testing"
	"time"

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

// TestOffBoxCreateDoesNotReclaimAnArchivedLocalBranch: an archived local "foo"
// holds global/foo and a Docker create titled "foo" derives that same string —
// but it provisions inside the container and never runs `git worktree add` on
// the host, so the archived branch is not in its way. The title reuse still
// happens; the branch must stay put (#4562 review).
func TestOffBoxCreateDoesNotReclaimAnArchivedLocalBranch(t *testing.T) {
	m, first, _, _ := projectBranchPrefixFixture(t)
	repoID := repoIDFor(t, first)
	archived, _ := seedArchivedSession(t, m, repoID, first, "foo", "foo")
	require.Equal(t, "global/foo", archived.GetBranch())

	res, err := m.reserveCreateWithWorktreeAdmission(CreateSessionRequest{
		RepoPath: first, Title: "foo", Program: "claude", Backend: string(session.BackendDocker),
	}, m.Config(), false)
	require.NoError(t, err)
	defer res.release()
	require.NotNil(t, res.renamedArchived,
		"the archived session still gives up its title for an off-box reuse")
	assert.Equal(t, "global/foo", res.renamedArchived.Branch,
		"an off-box create never needs a host branch, so the archived one must not be moved")
	assert.Equal(t, "global/foo", archived.GetBranch())
}

// TestOffBoxReservationKeepsThePrefixItReservedUnder: an in-flight off-box
// create is judged by the branch it derived when admitted, not re-derived under
// the global branch_prefix a later create's snapshot carries. "#x" reserved
// global/x; the global value then changes to "new-", under which BOTH "#x" and
// "-x" derive "new-x". Judging the reservation under the new prefix would
// refuse a pair admission already cleared (#4562 review).
func TestOffBoxReservationKeepsThePrefixItReservedUnder(t *testing.T) {
	m, first, _, _ := projectBranchPrefixFixture(t)

	firstRes, err := m.reserveCreateWithWorktreeAdmission(CreateSessionRequest{
		RepoPath: first, Title: "#x", Program: "claude", Backend: string(session.BackendDocker),
	}, m.Config(), false)
	require.NoError(t, err)
	defer firstRes.release()

	// The op-entry snapshot a create takes after a branch_prefix save carries
	// the new value; pass it the way CreateSession would.
	changed := *m.Config()
	changed.BranchPrefix = "new-"
	second, err := m.reserveCreateWithWorktreeAdmission(CreateSessionRequest{
		RepoPath: first, Title: "-x", Program: "claude", Backend: string(session.BackendDocker),
	}, &changed, false)
	require.NoError(t, err,
		"#x reserved global/x and -x derives new-x under the live prefix; re-deriving the reservation under it would collide the pair")
	second.release()
}

// TestPendingCreateRowCarriesItsBackendAndBranch: the daemon publishes the
// pending-create projection before the backend exists. The row must carry the
// resolved runtime and the branch the reservation pinned, or a client
// materializes it as a host-local claim with no branch and judges it under a
// project override the daemon never applied (#4562 review).
func TestPendingCreateRowCarriesItsBackendAndBranch(t *testing.T) {
	m, first, _, _ := projectBranchPrefixFixture(t)
	repoID := repoIDFor(t, first)

	// Hold provisioning so the pending row stays visible while asserted. The
	// create goroutine must finish before the factory seam is restored —
	// NewInstance reads the package var inside it — so the deferred teardown
	// opens the gate, drains the create, and only then swaps the factory back.
	done := make(chan error, 1)
	gate := make(chan struct{})
	restore := session.SetBackendFactoryForTest(func(session.InstanceOptions, string) (session.Backend, error) {
		<-gate
		backend := session.NewFakeBackend()
		backend.CompleteStart()
		return readyFakeBackend{backend}, nil
	})
	defer func() {
		close(gate)
		<-done
		restore()
	}()

	go func() {
		_, err := m.CreateSession(context.Background(), CreateSessionRequest{
			RepoPath: first, Title: "dock", Program: "claude", Backend: string(session.BackendDocker),
		})
		done <- err
	}()

	var pending session.InstanceData
	require.Eventually(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		row, ok := m.pendingCreates[daemonInstanceKey(repoID, "dock")]
		pending = row
		return ok
	}, 5*time.Second, 5*time.Millisecond, "the pending-create row must be published while provisioning runs")

	assert.Equal(t, "docker", pending.BackendType,
		"without the resolved backend a client materializes the pending row as host-local")
	assert.Equal(t, "global/dock", pending.Branch,
		"the row must carry the branch the reservation pinned, not an empty one to re-derive")
	assert.False(t, pending.BranchClaim().Local)
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
