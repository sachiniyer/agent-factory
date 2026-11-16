package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/fsprobe"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/overlay"
)

// #7272 (extended to the project-union derivation): buildProjectListFromCounted
// and the rebind reply's samePath run on the bubbletea event loop, so every
// path-identity lookup they make must route through the bounded fsprobe flight,
// not pathutil.ResolveForCompare's unbounded ancestor walk. On a wedged mount
// (NFS, FUSE, a dead sshfs) an unbounded filepath.EvalSymlinks never returns, and
// a single registered project on such a mount parks Update — key input, repaint
// and the Ctrl+C hard exit with it — until the process is SIGKILLed. The sidebar
// poll re-enters the derivation every 750 ms, so the freeze needs no user action.

// wedgeResolutionForTest wedges fsprobe.ResolveForCompare's symlink walk for
// paths at or under root: the seam blocks until the test ends (the way a project
// root on a wedged mount never answers), bounds the flight at 50 ms so a
// regression is fast, and lets every other path resolve normally. The cleanup
// releases the blocked goroutines and lets every still-wedged flight finish
// before the seam is restored, so no later test joins a flight still parked on
// the seam this test installed — the same pattern session/launch_dir_bound_test
// uses for the launch-dir resolve.
func wedgeResolutionForTest(t *testing.T, root string) {
	t.Helper()
	release := make(chan struct{})
	restoreTimeout := fsprobe.SetProbeTimeoutForTest(50 * time.Millisecond)
	t.Cleanup(func() {
		close(release)
		// Let every flight still blocked in the seam finish before the bound
		// comes back, so the flight registry is clean for the next test.
		time.Sleep(50 * time.Millisecond)
		restoreTimeout()
	})
	t.Cleanup(fsprobe.SetEvalSymlinksForTest(func(path string) (string, error) {
		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			<-release
		}
		return filepath.EvalSymlinks(path)
	}))
}

// resolveForCompareOrClean is resolveForCompare's spelling alone: the cleaned
// path on a non-answer.
func resolveForCompareOrClean(path string) string {
	spelling, _ := resolveForCompare(path)
	return spelling
}

// TestResolveForCompareOrClean_BoundsAWedgedWalk is the regression: the on-loop
// identity-lookup helper must route the canonicalizing walk through the bounded
// fsprobe flight so a stalled mount degrades instead of hanging the event loop.
//
// The wedge drives ONLY fsprobe's eval seam. pathutil.ResolveForCompare — the
// unbounded predecessor this replaces — resolves through filepath.EvalSymlinks
// directly, untouched by the seam, so a regression that re-introduces
// pathutil.ResolveForCompare would still canonicalize (link -> real) instead of
// degrading to the cleaned spelling. Asserting the cleaned spelling is the
// assertion that the resolve is bounded by fsprobe, and asserting it returns
// well under the walk's production bound is the assertion that the loop is not
// parked.
func TestResolveForCompareOrClean_BoundsAWedgedWalk(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	require.NoError(t, os.Mkdir(real, 0o755))
	link := filepath.Join(base, "link")
	require.NoError(t, os.Symlink(real, link))

	// Premise: without a wedge, a resolving symlink canonicalizes — proving
	// the helper does the walk when the filesystem answers, so the post-wedge
	// fallback is a degradation, not a no-op.
	require.NotEqual(t, filepath.Clean(link), resolveForCompareOrClean(link),
		"premise: a resolving symlink canonicalizes when the walk answers")

	wedgeResolutionForTest(t, link)

	start := time.Now()
	got := resolveForCompareOrClean(link)
	require.Less(t, time.Since(start), 2*time.Second,
		"a wedged walk must not hang the event loop the project-union derivation runs on")
	assert.Equal(t, filepath.Clean(link), got,
		"a wedged resolve degrades to the cleaned spelling, not the canonical one an unbounded pathutil walk would still return")
}

// TestResolveForCompareOrClean_AnswersACanonicalResolve is the non-regression:
// a healthy mount still canonicalizes — both spellings of one path compare equal
// under the #2110 rule, so a registered project and a root_agents key that spell
// the same directory through different symlinks still collapse to one row.
func TestResolveForCompareOrClean_AnswersACanonicalResolve(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	require.NoError(t, os.Mkdir(real, 0o755))
	link := filepath.Join(base, "link")
	require.NoError(t, os.Symlink(real, link))

	// A missing leaf behind a symlinked ancestor resolves the ancestor and
	// keeps the leaf, so both spellings of the missing-leaf path compare equal.
	viaLink := resolveForCompareOrClean(filepath.Join(link, "gone"))
	viaReal := resolveForCompareOrClean(filepath.Join(real, "gone"))
	assert.Equal(t, viaReal, viaLink,
		"both spellings of a missing leaf behind a symlink compare equal under a healthy mount")
	assert.Equal(t, "gone", filepath.Base(viaLink), "the missing leaf is kept")
}

// TestSamePath_BoundsAWedgedComparison pins the rebind reply handler's
// comparison: the follow compares roots in the projectReboundMsg Update handler,
// against spellings startResolvingSpellings resolved, so a wedged lookup on
// either side must degrade within the bound instead of parking the loop. Two
// spellings of the SAME wedged path still compare equal: each side falls back to
// its cleaned spelling, and Clean compares to Clean.
func TestSamePath_BoundsAWedgedComparison(t *testing.T) {
	samePath := func(a, b string) bool {
		return startResolvingSpellings([]string{a, b}, &projectSpellingCache{}, true)().same(a, b)
	}
	base := t.TempDir()
	real := filepath.Join(base, "real")
	require.NoError(t, os.Mkdir(real, 0o755))
	link := filepath.Join(base, "link")
	require.NoError(t, os.Symlink(real, link))

	// Premise: a symlink and its target compare equal when the walk answers.
	require.True(t, samePath(link, real),
		"premise: a symlink and its target compare equal when the walk answers")

	wedgeResolutionForTest(t, link)

	start := time.Now()
	got := samePath(link, link)
	require.Less(t, time.Since(start), 2*time.Second,
		"a wedged comparison must not hang the rebind reply handler")
	assert.True(t, got,
		"an identity comparison degrades to Clean==Clean and still recognizes the same path")
}

// TestBuildProjectListFromCounted_BoundsAWedgedRegisteredRoot is the end-to-end
// smoke for the event-loop derivation: a single registered project whose root is
// on a stalled mount must not park buildProjectListFromCounted. The derivation
// runs synchronously inside every Update handler that lists projects (Ctrl+P,
// the sidebar poll, the switch/rebind replies), so a wedge anywhere in its
// path-identity lookups parks the whole TUI. The wedged fsprobe flight degrades
// to the cleaned spelling and the registered project keeps its recorded row.
func TestBuildProjectListFromCounted_BoundsAWedgedRegisteredRoot(t *testing.T) {
	h := newTestHome(t)
	t.Cleanup(SetAllReposSnapshotFetcherForTest(func(context.Context) ([]session.InstanceData, error) {
		return nil, nil
	}))
	h.appConfig.RootAgents = map[string]config.RootAgentConfig{}

	// A real registered project, the way the daemon writes one.
	registeredRoot := initTestGitRepo(t)
	project, err := config.RegisterProject(registeredRoot)
	require.NoError(t, err)

	// The active project is a DIFFERENT repo, so the registered row can only
	// have come from the registry union.
	h.repoRoot = initTestGitRepo(t)
	h.repoID = config.RepoIDFromRoot(h.repoRoot)

	// Wedge the registered project's root the way a stalled NFS/FUSE/sshfs mount
	// does: the bound is 50 ms, so the derivation must finish in well under the
	// production per-project worst case of the bounded resolve flight.
	wedgeResolutionForTest(t, project.Root)

	type result struct {
		projects []overlay.Project
	}
	done := make(chan result, 1)
	go func() {
		projects, _, _ := h.buildProjectListFromCounted(nil)
		done <- result{projects: projects}
	}()

	select {
	case r := <-done:
		// The registered project keeps its row: the derivation degrades rather
		// than vanishing the project whose root it could not canonicalize.
		seen := map[string]bool{}
		for _, p := range r.projects {
			seen[p.RepoID] = true
		}
		assert.True(t, seen[project.RepoID],
			"a registered project on a stalled mount keeps its row rather than freezing the TUI")
	case <-time.After(5 * time.Second):
		t.Fatal("buildProjectListFromCounted did not return: a registered project on a wedged mount parked the event loop")
	}
}
