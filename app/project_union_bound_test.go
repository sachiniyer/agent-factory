package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/fsprobe"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

// #7397 review R1 and R3, on the event loop itself. A poll and a rebind reply
// each make one registry read and resolve every root they compare; with ten
// registered roots on a stalled mount, each of those must hold Update for one
// bound in total — not one per root — while each root keeps a bound of its own,
// so a slow mount that answers inside it is read exactly as it answered. And a
// probe that never returns must not be started again by the next poll.

// unionBound is the fsprobe flight bound these tests run under. It is long
// next to the probe layer's fixed gathers (150 ms + 250 ms), so "under two
// bounds" separates one phase (about 1 B plus the gathers) from two (2 B plus
// them) without leaning on a fast runner.
const unionBound = 1500 * time.Millisecond

// onStalledMount reports whether path is one of roots or lies under one.
func onStalledMount(roots []string, path string) bool {
	for _, root := range roots {
		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// mountForUnionTest stands in for the mount roots sit on, through fsprobe's
// stat, symlink-resolution and lstat seams, under bound. With delay < 0 it is
// dark: every lookup on it blocks until the test ends. Otherwise each lookup
// answers truthfully after delay. It returns the count of lookups the mount was
// asked for. The cleanup releases the mount and lets every released flight
// unpublish before the seams come back.
func mountForUnionTest(t *testing.T, roots []string, bound, delay time.Duration) *atomic.Int64 {
	t.Helper()
	release := make(chan struct{})
	var lookups atomic.Int64
	wait := func(path string) {
		if !onStalledMount(roots, path) {
			return
		}
		lookups.Add(1)
		if delay < 0 {
			<-release
			return
		}
		time.Sleep(delay)
	}
	restoreBound := fsprobe.SetProbeTimeoutForTest(bound)
	restoreStat := fsprobe.SetStatPathForTest(func(path string) (os.FileInfo, error) {
		wait(path)
		return os.Stat(path)
	})
	restoreEval := fsprobe.SetEvalSymlinksForTest(func(path string) (string, error) {
		wait(path)
		return filepath.EvalSymlinks(path)
	})
	restoreLstat := fsprobe.SetLstatForTest(func(path string) (os.FileInfo, error) {
		wait(path)
		return os.Lstat(path)
	})
	t.Cleanup(func() {
		close(release)
		time.Sleep(bound + max(delay, 0))
		restoreLstat()
		restoreEval()
		restoreStat()
		restoreBound()
	})
	return &lookups
}

// onLoopWithin runs step — Update work — on its own goroutine and returns how
// long it held the loop, failing the test once limit passes. loop tracks the
// goroutine so a cleanup registered before the mount can wait it out once the
// mount is released, and it never outlives the test's own state.
func onLoopWithin(t *testing.T, loop *sync.WaitGroup, limit time.Duration, what string, step func()) time.Duration {
	t.Helper()
	done := make(chan time.Duration, 1)
	loop.Add(1)
	go func() {
		defer loop.Done()
		start := time.Now()
		step()
		done <- time.Since(start)
	}()
	select {
	case elapsed := <-done:
		return elapsed
	case <-time.After(limit):
		t.Fatalf("%s did not return within %s", what, limit)
		return 0
	}
}

// registeredProjectRoot is a test's own read of where the registry binds id
// now, and the identity recorded for it.
func registeredProjectRoot(id string) (root, repoID, checkoutID string, ok bool) {
	projects, err := config.ListProjects()
	if err != nil {
		return "", "", "", false
	}
	for _, p := range projects {
		if p.ID == id {
			return p.Root, p.RepoID, p.CheckoutID, true
		}
	}
	return "", "", "", false
}

// registerUnionRoots registers n fresh checkouts and returns their records.
func registerUnionRoots(t *testing.T, n int) []config.Project {
	t.Helper()
	projects := make([]config.Project, n)
	for i := range projects {
		project, err := config.RegisterProject(initTestGitRepo(t))
		require.NoError(t, err)
		projects[i] = project
	}
	return projects
}

func projectRoots(projects []config.Project) []string {
	roots := make([]string, len(projects))
	for i, project := range projects {
		roots[i] = project.Root
	}
	return roots
}

// requireRegistryRowsKept asserts every registration keeps its own row, read
// as present unless it is in gone.
func requireRegistryRowsKept(t *testing.T, rows []overlay.Project, registered []config.Project, gone map[string]bool) {
	t.Helper()
	byRegistryID := make(map[string]overlay.Project, len(rows))
	for _, row := range rows {
		if row.RegistryID != "" {
			byRegistryID[row.RegistryID] = row
		}
	}
	for _, project := range registered {
		row, ok := byRegistryID[project.ID]
		if !assert.True(t, ok, "%s: a registration on a stalled mount keeps its row", project.Root) {
			continue
		}
		assert.Equal(t, gone[project.Root], row.MissingPath,
			"%s: read as gone only when the mount answered that it is gone", project.Root)
	}
}

// newUnionHome is a home scoped to a healthy active project, with a session
// snapshot that answers at once.
func newUnionHome(t *testing.T) *home {
	t.Helper()
	h := newTestHome(t)
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func(context.Context) ([]session.InstanceData, error) {
		return nil, nil
	}))
	h.appConfig.RootAgents = map[string]config.RootAgentConfig{}
	h.repoRoot = initTestGitRepo(t)
	h.repoID = config.RepoIDFromRoot(h.repoRoot)
	return h
}

// TestBuildProjectListFromCounted_HoldsOneBoundForManyStalledRoots is the R1
// regression for the poll: ten registered roots and two root_agents keys, all
// on a mount that answers no stat and no resolve. a9694c16c3 statted the roots
// one after another and then resolved them one after another — about twenty
// bounds before the probes even began. In one phase it is one bound.
func TestBuildProjectListFromCounted_HoldsOneBoundForManyStalledRoots(t *testing.T) {
	h := newUnionHome(t)
	registered := registerUnionRoots(t, 10)
	stalled := projectRoots(registered)
	// Two root_agents keys on the same mount that git answers are no
	// repository: the union canonicalizes exactly those against the registry.
	for _, name := range []string{"agents-a", "agents-b"} {
		key := filepath.Join(t.TempDir(), name)
		require.NoError(t, os.Mkdir(key, 0o755))
		h.appConfig.RootAgents[key] = config.RootAgentConfig{}
		stalled = append(stalled, key)
	}

	var loop sync.WaitGroup
	t.Cleanup(loop.Wait) // after the mount below is released
	mountForUnionTest(t, stalled, unionBound, -1)

	var rows []overlay.Project
	elapsed := onLoopWithin(t, &loop, 2*unionBound, "a poll over ten stalled roots", func() {
		rows, _, _ = h.buildProjectListFromCounted(nil)
	})
	t.Logf("the poll held Update for %s at a %s bound", elapsed, unionBound)
	requireRegistryRowsKept(t, rows, registered, nil)
}

// TestBuildProjectListFromCounted_ReadsSlowHealthyRootsAsTheyAnswer is the
// other half: every lookup on the mount answers, at 0.6 of its bound, and some
// roots are really gone. Each root keeps its own bound, so the poll reads the
// mount exactly as master does — the deleted roots missing, the rest present —
// and it still holds Update for about one bound rather than one per root.
func TestBuildProjectListFromCounted_ReadsSlowHealthyRootsAsTheyAnswer(t *testing.T) {
	h := newUnionHome(t)
	registered := registerUnionRoots(t, 10)
	gone := map[string]bool{}
	for i, project := range registered {
		if i%3 == 1 {
			require.NoError(t, os.RemoveAll(project.Root))
			gone[project.Root] = true
		}
	}

	var loop sync.WaitGroup
	t.Cleanup(loop.Wait)
	mountForUnionTest(t, projectRoots(registered), unionBound, unionBound*6/10)

	var rows []overlay.Project
	elapsed := onLoopWithin(t, &loop, 2*unionBound, "a poll over ten slow roots", func() {
		rows, _, _ = h.buildProjectListFromCounted(nil)
	})
	t.Logf("the poll held Update for %s at a %s bound", elapsed, unionBound)
	requireRegistryRowsKept(t, rows, registered, gone)
}

// TestProjectRebound_HoldsOneBoundForManyStalledRoots is the R1 regression for
// the rebind reply: the active project is rebound AWAY from a dead mount that
// ten other registered roots still sit on. a9694c16c3's reply re-read the
// registry three times, statting every root to find one id, and resolved the
// root it moved away from on its own after the refresh. Here the reply holds
// Update for one bound, and still follows the project to its new checkout.
func TestProjectRebound_HoldsOneBoundForManyStalledRoots(t *testing.T) {
	h, id := activeRebindHome(t)
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func(context.Context) ([]session.InstanceData, error) {
		return nil, nil
	}))
	h.appConfig.RootAgents = map[string]config.RootAgentConfig{}
	before, err := config.ListProjects()
	require.NoError(t, err)
	require.Len(t, before, 1)
	oldRoot := before[0].Root
	require.Equal(t, id, before[0].ID)

	others := registerUnionRoots(t, 10)
	stubRebindThrough(t, nil)
	newRoot := initTestGitRepo(t)
	// The daemon's rebind commits before the mount stops answering.
	reply := submitPickerRebind(t, h, newRoot)()

	var loop sync.WaitGroup
	t.Cleanup(loop.Wait)
	mountForUnionTest(t, append(projectRoots(others), oldRoot), unionBound, -1)

	elapsed := onLoopWithin(t, &loop, 2*unionBound, "a rebind reply over ten stalled roots", func() {
		h.Update(reply)
	})
	t.Logf("the rebind reply held Update for %s at a %s bound", elapsed, unionBound)
	settleProjectSwitch(t, h)
	assert.Equal(t, newRoot, h.repoRoot, "the reply still follows the active project to its new checkout")
	requireRegistryRowsKept(t, h.sidebarProjects, others, nil)
}

// probeCounts counts one kind of probe per path: started, and returned.
type probeCounts struct {
	mu       sync.Mutex
	started  map[string]int
	returned int
}

func (c *probeCounts) start(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started == nil {
		c.started = map[string]int{}
	}
	c.started[path]++
}

func (c *probeCounts) finish() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.returned++
}

// inFlight is how many started probes have not returned, and the most any one
// path has started.
func (c *probeCounts) inFlight() (running, mostForOnePath int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, n := range c.started {
		running += n
		mostForOnePath = max(mostForOnePath, n)
	}
	return running - c.returned, mostForOnePath
}

// TestProjectPoll_HoldsOneProbePerStalledPathAcrossPolls is the R3 regression:
// ten registered roots whose git never returns, the way git does when its FUSE
// request is already in userspace and the poll's kill cannot reach it. Every
// 750 ms poll on a9694c16c3 started a fresh path probe and a fresh proof for
// each — a git process and a pinned thread per root per poll. Ten polls here
// start one of each per root, and the number in flight stays where poll one
// left it.
func TestProjectPoll_HoldsOneProbePerStalledPathAcrossPolls(t *testing.T) {
	h := newUnionHome(t)
	registered := registerUnionRoots(t, 10)
	stalled := projectRoots(registered)

	release := make(chan struct{})
	var paths, proofs probeCounts
	pathProbe, proofProbe := projectPathRepoProbe, registeredProjectIdentityProbe
	projectPathRepoProbe = func(ctx context.Context, path string) (*config.RepoContext, error) {
		if !onStalledMount(stalled, path) {
			return pathProbe(ctx, path)
		}
		paths.start(path)
		defer paths.finish()
		<-release
		return nil, context.Canceled
	}
	registeredProjectIdentityProbe = func(ctx context.Context, project config.Project) (string, bool) {
		if !onStalledMount(stalled, project.Root) {
			return proofProbe(ctx, project)
		}
		proofs.start(project.Root)
		defer proofs.finish()
		<-release
		return "", false
	}
	t.Cleanup(func() {
		close(release)
		require.Eventually(t, func() bool {
			pathsRunning, _ := paths.inFlight()
			proofsRunning, _ := proofs.inFlight()
			return pathsRunning == 0 && proofsRunning == 0
		}, 10*time.Second, 10*time.Millisecond, "every released probe returns")
		projectPathRepoProbe, registeredProjectIdentityProbe = pathProbe, proofProbe
	})

	var laterPolls time.Duration
	for poll := 1; poll <= 10; poll++ {
		start := time.Now()
		rows, _, _ := h.buildProjectListFromCounted(nil)
		if poll > 1 {
			laterPolls += time.Since(start)
		}
		requireRegistryRowsKept(t, rows, registered, nil)
		pathsRunning, mostPaths := paths.inFlight()
		proofsRunning, mostProofs := proofs.inFlight()
		t.Logf("after poll %d: %d path probes and %d proofs in flight", poll, pathsRunning, proofsRunning)
		require.Equal(t, 1, mostPaths,
			"poll %d started a second path probe for a root whose first had not returned", poll)
		require.Equal(t, 1, mostProofs,
			"poll %d started a second proof for a root whose first had not returned", poll)
		require.Equal(t, len(stalled), pathsRunning, "poll %d: one path probe per stalled root, no more", poll)
		require.Equal(t, len(stalled), proofsRunning, "poll %d: one proof per stalled root, no more", poll)
	}
	// And a probe still running from an earlier poll is unanswered at once, not
	// waited on: the stalled roots cost poll one its gather windows (150 ms for
	// the paths, 250 ms for the proofs), and the nine polls after it fewer than
	// four polls' worth between them — waiting each time would cost nine.
	t.Logf("polls 2-10 held the loop for %s in total", laterPolls)
	assert.Less(t, laterPolls, 4*(projectPathScanTimeout+registeredProjectProveTimeout),
		"a poll waited on probes that earlier polls had already given up on")
}

// TestSnapshotPoll_ReadsTheRegistryOffTheLoop: the 750 ms poll reads the
// registry, and resolves every root and root_agents key, inside the fetch it
// already runs off the loop. Applying the poll then looks up no path on the
// stalled mount at all: the mount delays the poll, never Update.
func TestSnapshotPoll_ReadsTheRegistryOffTheLoop(t *testing.T) {
	h := newUnionHome(t)
	registered := registerUnionRoots(t, 10)
	stalled := projectRoots(registered)
	for _, name := range []string{"agents-a", "agents-b"} {
		key := filepath.Join(t.TempDir(), name)
		require.NoError(t, os.Mkdir(key, 0o755))
		h.appConfig.RootAgents[key] = config.RootAgentConfig{}
		stalled = append(stalled, key)
	}

	var loop sync.WaitGroup
	t.Cleanup(loop.Wait)
	lookups := mountForUnionTest(t, stalled, unionBound, -1)

	// bubbletea runs the poll's Cmd on a goroutine of its own; so does this.
	var msg tea.Msg
	offLoop := onLoopWithin(t, &loop, 2*unionBound, "the poll's off-loop fetch", func() {
		msg = h.fetchSnapshotCmd()()
	})
	before := lookups.Load()
	require.Positive(t, before, "premise: the poll's fetch looked the stalled paths up")
	applied := onLoopWithin(t, &loop, unionBound/2, "applying the poll in Update", func() {
		h.Update(msg)
	})
	t.Logf("the fetch took %s off the loop; applying it held Update for %s", offLoop, applied)
	assert.Equal(t, before, lookups.Load(), "applying a poll looks up no path on the stalled mount")
	requireRegistryRowsKept(t, h.sidebarProjects, registered, nil)
}

// TestSnapshotPoll_DoesNotApplyARegistryViewOlderThanTheLoops: a poll whose
// registry read began before a refresh on the loop — an add completing — must
// not replace that refresh's rows with its older read, which would hide the
// project the add just showed until the next poll.
func TestSnapshotPoll_DoesNotApplyARegistryViewOlderThanTheLoops(t *testing.T) {
	h := newUnionHome(t)
	first := registerUnionRoots(t, 1)
	stale := readProjectRegistryView(h.rootAgentKeys(), nil, h.spellingCache(), true)
	stale.gen = h.registryViewGen // the poll's read began here

	added := registerUnionRoots(t, 1)
	h.refreshSidebarProjects() // the add's completion refreshes on the loop
	both := append(append([]config.Project{}, first...), added...)
	requireRegistryRowsKept(t, h.sidebarProjects, both, nil)

	h.refreshSidebarProjectsFromPoll(nil, nil, nil, stale)
	requireRegistryRowsKept(t, h.sidebarProjects, both, nil)
	assert.NotSame(t, stale, h.registryView, "the older poll read is not installed over the loop's")

	current := readProjectRegistryView(h.rootAgentKeys(), nil, h.spellingCache(), true)
	current.gen = h.registryViewGen
	h.refreshSidebarProjectsFromPoll(nil, nil, nil, current)
	assert.Same(t, current, h.registryView, "a poll read that nothing superseded is the one installed")
	requireRegistryRowsKept(t, h.sidebarProjects, both, nil)
}

// TestProjectUnion_DoesNotReprobeASlowRootAgentKeyOnTheLoop: a root_agents key
// on a mount that answers, but slowly — inside the bound, past the probe
// window. The loop resolves it the first time it sees it; after that its reads
// take the cached spelling and never wait on that mount again.
func TestProjectUnion_DoesNotReprobeASlowRootAgentKeyOnTheLoop(t *testing.T) {
	h := newUnionHome(t)
	key := filepath.Join(t.TempDir(), "agents")
	require.NoError(t, os.Mkdir(key, 0o755))
	h.appConfig.RootAgents[key] = config.RootAgentConfig{}

	var loop sync.WaitGroup
	t.Cleanup(loop.Wait)
	lookups := mountForUnionTest(t, []string{key}, unionBound, unionBound*6/10)

	onLoopWithin(t, &loop, 2*unionBound, "the loop's first read", func() {
		h.buildProjectListFromCounted(nil)
	})
	learned := lookups.Load()
	require.Positive(t, learned, "premise: the first read resolved the key")
	for read := 2; read <= 4; read++ {
		elapsed := onLoopWithin(t, &loop, unionBound/2, "a later read on the loop", func() {
			h.buildProjectListFromCounted(nil)
		})
		t.Logf("read %d held Update for %s", read, elapsed)
	}
	assert.Equal(t, learned, lookups.Load(), "the slow key was looked up again on the loop")
}

// TestProjectUnion_PollRevalidatesACachedSpelling: the cache the loop reads
// from follows a changed symlink within a poll, because the poll resolves every
// path it covers again, off the loop.
func TestProjectUnion_PollRevalidatesACachedSpelling(t *testing.T) {
	h := newUnionHome(t)
	base := t.TempDir()
	dirA, dirB := filepath.Join(base, "a"), filepath.Join(base, "b")
	require.NoError(t, os.Mkdir(dirA, 0o755))
	require.NoError(t, os.Mkdir(dirB, 0o755))
	key := filepath.Join(base, "agents")
	require.NoError(t, os.Symlink(dirA, key))
	h.appConfig.RootAgents[key] = config.RootAgentConfig{}
	canonical := func(dir string) string {
		resolved, err := filepath.EvalSymlinks(dir)
		require.NoError(t, err)
		return resolved
	}

	h.buildProjectListFromCounted(nil)
	require.Equal(t, canonical(dirA), h.registryView.spellings[key], "premise: the loop learned the key's spelling")

	require.NoError(t, os.Remove(key))
	require.NoError(t, os.Symlink(dirB, key))
	h.buildProjectListFromCounted(nil)
	assert.Equal(t, canonical(dirA), h.registryView.spellings[key],
		"between polls, the loop reads the spelling it learned rather than resolving again")

	h.Update(h.fetchSnapshotCmd()())
	assert.Equal(t, canonical(dirB), h.registryView.spellings[key], "the poll resolved the key again")
	h.buildProjectListFromCounted(nil)
	assert.Equal(t, canonical(dirB), h.registryView.spellings[key], "and the loop's next read has the new spelling")
}

// TestRebindFollow_FollowsTheBindingTheSidebarShows: another client rebinds the
// active project again after this rebind's reply arrives and before the reply's
// refresh reads the registry. The Projects section shows that newer binding, so
// the follow must target it too — never a binding read before the refresh.
func TestRebindFollow_FollowsTheBindingTheSidebarShows(t *testing.T) {
	h, id := activeRebindHome(t)
	h.appConfig.RootAgents = map[string]config.RootAgentConfig{}
	stubRebindThrough(t, nil)
	echo, newer := initTestGitRepo(t), initTestGitRepo(t)
	reply := submitPickerRebind(t, h, echo)()

	// The reply's refresh fetches the session snapshot before it reads the
	// registry; the other client's rebind lands in between.
	var once sync.Once
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func(context.Context) ([]session.InstanceData, error) {
		once.Do(func() {
			_, err := config.RebindProject(id, newer)
			require.NoError(t, err)
		})
		return nil, nil
	}))

	h.Update(reply)
	settleProjectSwitch(t, h)
	shown := ""
	for _, row := range h.sidebarProjects {
		if row.RegistryID == id {
			shown = row.RegistryRoot
		}
	}
	newerRoot, _, _, ok := registeredProjectRoot(id)
	require.True(t, ok)
	require.Equal(t, newerRoot, shown, "premise: the Projects section shows the newer binding")
	assert.Equal(t, newer, h.repoRoot, "the follow targets the binding the Projects section shows")
}
