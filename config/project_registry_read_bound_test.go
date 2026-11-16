package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
)

// #7397 review R1: the registry read the TUI makes on its event loop must hold
// its caller for ONE bound in total, whatever the number of registered roots
// and however many of them sit on a stalled mount — while each root still gets
// a bound of its own, so a slow mount that answers inside it is read exactly as
// it answered (#7389 review R1's class: one deadline shared across roots turns a
// slow answer into a refusal).

// registerProjectRoots registers n fresh checkouts under base and returns their
// registered (canonical) roots.
func registerProjectRoots(t *testing.T, base string, n int) []string {
	t.Helper()
	roots := make([]string, n)
	for i := range roots {
		root := initProjectRegistryRepo(t, filepath.Join(base, "mount", fmt.Sprintf("r%02d", i)))
		registered, err := RegisterProject(root)
		require.NoError(t, err)
		roots[i] = registered.Root
	}
	return roots
}

// onMount reports whether path is one of roots or lies under one.
func onMount(roots []string, path string) bool {
	for _, root := range roots {
		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// mountForTest stands in for the mount every root in roots sits on, through
// fsprobe's stat, symlink-resolution and lstat seams. With delay < 0 the mount
// is dark: every lookup on it blocks until the test ends. Otherwise every lookup
// on it answers truthfully after delay. Paths off the mount answer at once. It
// returns the count of stats the mount was asked for. The cleanup releases the
// mount and lets every released flight unpublish before the seams come back, so
// no later test joins a flight parked on this test's seam.
func mountForTest(t *testing.T, roots []string, bound, delay time.Duration) *atomic.Int64 {
	t.Helper()
	release := make(chan struct{})
	var stats atomic.Int64
	wait := func(path string) {
		if !onMount(roots, path) {
			return
		}
		if delay < 0 {
			<-release
			return
		}
		time.Sleep(delay)
	}
	restoreBound := fsprobe.SetProbeTimeoutForTest(bound)
	restoreStat := fsprobe.SetStatPathForTest(func(path string) (os.FileInfo, error) {
		if onMount(roots, path) {
			stats.Add(1)
		}
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
	return &stats
}

// readWithin runs read on its own goroutine and returns how long it held its
// caller, failing the test once limit passes. reads tracks the goroutine so the
// caller's cleanup can wait it out after the mount is released.
func readWithin(t *testing.T, reads *sync.WaitGroup, limit time.Duration, what string, read func()) time.Duration {
	t.Helper()
	done := make(chan time.Duration, 1)
	reads.Add(1)
	go func() {
		defer reads.Done()
		start := time.Now()
		read()
		done <- time.Since(start)
	}()
	select {
	case elapsed := <-done:
		return elapsed
	case <-time.After(limit):
		t.Fatalf("%s held its caller past %s: the roots were read one after another, not in one phase", what, limit)
		return 0
	}
}

// resolveOrEmpty is the bounded canonicalization the TUI runs alongside the
// registry read, with an empty answer for "could not look".
func resolveOrEmpty(root string) string {
	resolved, err := fsprobe.ResolveForCompare(root)
	if err != nil {
		return ""
	}
	return resolved
}

// TestListProjectsAlongside_HoldsOneBoundForManyStalledRoots is the R1
// regression: twelve registered roots, every one on a mount that never answers
// a stat or a resolve. Statted and then resolved one after another — the shape
// a9694c16c3 shipped — the read waits two bounds per root (24 here). Statted in
// one phase and resolved in a second, it still waits two. In one phase it waits
// one, so anything under two bounds is the property.
func TestListProjectsAlongside_HoldsOneBoundForManyStalledRoots(t *testing.T) {
	const bound = 300 * time.Millisecond
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	roots := registerProjectRoots(t, base, 12)

	var reads sync.WaitGroup
	t.Cleanup(reads.Wait) // runs after the mount below is released
	mountForTest(t, roots, bound, -1)

	var (
		projects  []Project
		spellings []string
		err       error
	)
	elapsed := readWithin(t, &reads, 2*bound, "a registry read over twelve stalled roots", func() {
		projects, spellings, err = ListProjectsAlongside(resolveOrEmpty)
	})
	t.Logf("held for %s at a %s bound", elapsed, bound)
	require.NoError(t, err, "the registry enumerates fine; only the roots are stalled")
	require.Len(t, projects, len(roots), "no stalled entry is dropped")
	require.Len(t, spellings, len(projects), "one probe answer per project")
	for i, project := range projects {
		assert.True(t, project.PathExists,
			"%s: a stat that never answered is not absence, so the project is not read as gone", project.Root)
		assert.Empty(t, spellings[i],
			"%s: a resolve that never answered is a non-answer, not a spelling", project.Root)
	}
}

// TestListProjectsAlongside_ReadsSlowHealthyRootsAsTheyAnswer is the other half
// of the property: every lookup on the mount answers, at 0.6 of its bound. Each
// root keeps a bound of its own, so the read reports exactly what the mount
// said — the deleted roots gone, the others present under their canonical
// spelling — which one deadline shared across the roots would not. And it still
// holds its caller for about one bound, not one per root.
func TestListProjectsAlongside_ReadsSlowHealthyRootsAsTheyAnswer(t *testing.T) {
	const bound = 500 * time.Millisecond
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	roots := registerProjectRoots(t, base, 10)
	deleted := map[string]bool{}
	for i, root := range roots {
		if i%3 == 1 {
			require.NoError(t, os.RemoveAll(root))
			deleted[root] = true
		}
	}

	var reads sync.WaitGroup
	t.Cleanup(reads.Wait)
	mountForTest(t, roots, bound, bound*6/10)

	var (
		projects  []Project
		spellings []string
		err       error
	)
	elapsed := readWithin(t, &reads, 2*bound, "a registry read over ten slow roots", func() {
		projects, spellings, err = ListProjectsAlongside(resolveOrEmpty)
	})
	t.Logf("held for %s at a %s bound", elapsed, bound)
	require.NoError(t, err)
	require.Len(t, projects, len(roots))
	for i, project := range projects {
		if deleted[project.Root] {
			assert.False(t, project.PathExists,
				"%s: the mount answered that the root is gone, inside the root's own bound", project.Root)
			continue
		}
		assert.True(t, project.PathExists, "%s: the mount answered that the root is there", project.Root)
		assert.Equal(t, project.Root, spellings[i],
			"%s: the resolve answered inside its own bound, so its spelling is the canonical one", project.Root)
	}
}

// TestProjectBindingByID_ReadsTheRecordWithoutStattingAnyRoot pins the lookup
// the rebind reply makes on the event loop, twice: finding one id must read
// that record, not stat every registered root. Through ListProjects it statted
// all ten stalled roots, a bound each; here it stats none.
func TestProjectBindingByID_ReadsTheRecordWithoutStattingAnyRoot(t *testing.T) {
	const bound = 300 * time.Millisecond
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	roots := registerProjectRoots(t, base, 10)
	projects, err := ListProjects()
	require.NoError(t, err)
	want := projects[len(projects)/2]
	require.NotEmpty(t, want.RepoID, "fixture: registration records the repository identity")

	var reads sync.WaitGroup
	t.Cleanup(reads.Wait)
	stats := mountForTest(t, roots, bound, -1)

	var (
		binding ProjectBinding
		ok      bool
	)
	elapsed := readWithin(t, &reads, bound/2, "a lookup of one id over ten stalled roots", func() {
		binding, ok, err = ProjectBindingByID(want.ID)
	})
	t.Logf("held for %s at a %s bound", elapsed, bound)
	require.NoError(t, err)
	require.True(t, ok, "the record is there")
	assert.Equal(t, ProjectBinding{ID: want.ID, CheckoutID: want.CheckoutID, Root: want.Root, RepoID: want.RepoID}, binding,
		"the binding is the record's own fields")
	assert.Zero(t, stats.Load(), "a lookup by id stats no root, its own included")

	_, ok, err = ProjectBindingByID("prj_" + strings.Repeat("0", 32))
	require.NoError(t, err)
	assert.False(t, ok, "an id with no record is not bound")
}
