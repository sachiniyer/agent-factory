package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
)

// #7272 (extended to the registry read on the project-union derivation):
// ListProjects is the registry read buildProjectListFromCounted performs
// synchronously on the bubbletea event loop, and projectFromRecord calls
// projectPathExists on each record's root. The stat ran unbounded via os.Stat,
// so a single registered project on a stalled NFS/FUSE/sshfs mount parked
// Update — key input, repaint and the Ctrl+C hard exit with it — before the
// bounded resolve flight was even reached. The stat now runs inside
// fsprobe.Stat's bounded flight; a wedged stat surfaces the bounded non-answer,
// which is not determinate absence, so path_exists fails closed to "present"
// exactly as an unreadable-but-there root already does.

// wedgeStatForTest wedges fsprobe.Stat's underlying stat for paths at or under
// root: the seam blocks until the test ends, the way a project root on a
// wedged mount never answers, bounds the flight at 50 ms so a regression is
// fast, and lets every other path answer normally. The cleanup releases the
// blocked goroutines and lets every still-wedged flight finish before the seam
// is restored, so no later test joins a flight still parked on this seam.
func wedgeStatForTest(t *testing.T, root string) {
	t.Helper()
	release := make(chan struct{})
	restoreTimeout := fsprobe.SetProbeTimeoutForTest(50 * time.Millisecond)
	t.Cleanup(func() {
		close(release)
		time.Sleep(50 * time.Millisecond)
		restoreTimeout()
	})
	t.Cleanup(fsprobe.SetStatPathForTest(func(path string) (os.FileInfo, error) {
		if path == root || (root != "" && len(path) > len(root) && path[len(root)] == filepath.Separator && path[:len(root)] == root) {
			<-release
			return nil, os.ErrNotExist
		}
		return os.Stat(path)
	}))
}

// TestProjectPathExists_BoundsAWedgedStat pins the regression: a wedged stat
// must surface as the bounded non-answer rather than parking the event loop.
// The non-answer is not determinate absence, so path_exists fails closed to
// "present" — the same verdict an unreadable-but-there root already gets, and
// the one a stalled mount must get so the registered project keeps its row
// instead of vanishing behind a stat nobody could perform.
func TestProjectPathExists_BoundsAWedgedStat(t *testing.T) {
	root := t.TempDir()

	// Premise: a readable directory is present, and a determinately-absent
	// path is the one honest "no".
	require.True(t, projectPathExists(root), "premise: a readable directory is present")
	require.False(t, projectPathExists(filepath.Join(root, "gone")),
		"premise: a determinately-absent path is the one honest false")

	wedgeStatForTest(t, root)

	start := time.Now()
	got := projectPathExists(root)
	require.Less(t, time.Since(start), 2*time.Second,
		"a wedged stat must not park the event loop the registry read runs on")
	assert.True(t, got,
		"a wedged stat is evidence of nothing: the root stays present rather than vanishing behind a stat nobody could perform")
}

// TestListProjects_BoundsAWedgedRootStat is the end-to-end smoke for the
// registry read on the event-loop derivation: a single registered project whose
// root is on a stalled mount must not park ListProjects. The wedged stat
// degrades to "present" and the project keeps its row, so the switcher union the
// sidebar poll rebuilds every 750 ms still renders it instead of freezing the
// TUI.
func TestListProjects_BoundsAWedgedRootStat(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))

	root := initProjectRegistryRepo(t, filepath.Join(base, "checkout"))
	registered, err := RegisterProject(root)
	require.NoError(t, err)

	// Premise: the registered project reads back with PathExists=true on a
	// healthy mount.
	projects, err := ListProjects()
	require.NoError(t, err)
	require.Len(t, projects, 1)
	require.True(t, projects[0].PathExists, "premise: the registered root is present on a healthy mount")
	require.Equal(t, registered.ID, projects[0].ID)

	wedgeStatForTest(t, registered.Root)

	type result struct {
		projects []Project
		err      error
	}
	done := make(chan result, 1)
	go func() {
		ps, err := ListProjects()
		done <- result{projects: ps, err: err}
	}()

	select {
	case r := <-done:
		require.NoError(t, r.err, "the registry enumerates fine; only one root's stat is wedged")
		require.Len(t, r.projects, 1, "the wedged-root project keeps its row")
		assert.True(t, r.projects[0].PathExists,
			"a wedged stat fails closed to present, so the project does not vanish from the switcher union")
		assert.Equal(t, registered.ID, r.projects[0].ID)
	case <-time.After(5 * time.Second):
		t.Fatal("ListProjects did not return: a registered project on a wedged mount parked the registry read on the event loop")
	}
}

// TestListProjects_BoundsAcrossManyWedgedRoots pins the registry-read
// concurrency bound: when several DISTINCT registered roots are each on a
// stalled mount, the per-root stats run concurrently so the whole read is
// bounded by one fsprobe flight deadline rather than N× it. A sequential
// implementation would consume the full bound per root — six wedged roots at
// an 80 ms bound would take ~480 ms — so completing well inside that proves
// the stats overlapped. Each wedged root fails closed to "present" exactly as
// a single wedged root does.
func TestListProjects_BoundsAcrossManyWedgedRoots(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))

	const wedged = 6
	roots := make([]string, wedged)
	for i := 0; i < wedged; i++ {
		roots[i] = initProjectRegistryRepo(t, filepath.Join(base, "checkout", fmt.Sprintf("r%d", i)))
		_, err := RegisterProject(roots[i])
		require.NoError(t, err)
	}

	// Wedge the stat for every registered root at once: a single seam blocks
	// any of them, the way several roots on the same stalled mount never
	// answer. Per-flight bound is 80 ms, so a sequential read would take
	// ~480 ms while a concurrent read takes ~80 ms.
	release := make(chan struct{})
	restoreTimeout := fsprobe.SetProbeTimeoutForTest(80 * time.Millisecond)
	t.Cleanup(func() {
		close(release)
		time.Sleep(80 * time.Millisecond)
		restoreTimeout()
	})
	t.Cleanup(fsprobe.SetStatPathForTest(func(path string) (os.FileInfo, error) {
		for _, root := range roots {
			if path == root || (len(path) > len(root) && path[len(root)] == filepath.Separator && path[:len(root)] == root) {
				<-release
				return nil, os.ErrNotExist
			}
		}
		return os.Stat(path)
	}))

	type result struct {
		projects []Project
		err      error
		elapsed  time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		ps, err := ListProjects()
		done <- result{projects: ps, err: err, elapsed: time.Since(start)}
	}()

	select {
	case r := <-done:
		require.NoError(t, r.err, "the registry enumerates fine; only the per-root stats are wedged")
		require.Len(t, r.projects, wedged, "every wedged-root project keeps its row")
		for _, p := range r.projects {
			assert.True(t, p.PathExists,
				"a wedged stat fails closed to present, so the project does not vanish from the switcher union")
		}
		require.Less(t, r.elapsed, 250*time.Millisecond,
			"distinct wedged roots were stat sequentially and parked the registry read on the event loop (took %s)", r.elapsed)
	case <-time.After(2 * time.Second):
		t.Fatal("ListProjects did not return: distinct wedged roots parked the registry read on the event loop")
	}
}
