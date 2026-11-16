package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/throwawaysandbox"
	"github.com/sachiniyer/agent-factory/session"
)

// #7447: tearing down an expired throwaway removes the session containers its
// home created, each proven by its own af.home label: nothing else reaps them
// once that home's daemon never starts again. The expiring daemon reaps its
// own on the way out; a marked start reaps a swept sibling's after readiness,
// and keeps the sibling registered until none is left.

// captureContainerReap replaces the container reaper and returns the proof
// each call was given; the reaper itself is session.ReapHomeContainers, which
// session's own tests pin. result is what each call reports.
func captureContainerReap(t *testing.T) *[]func(string) bool {
	t.Helper()
	return captureContainerReapReporting(t, session.OrphanSweepResult{})
}

func captureContainerReapReporting(t *testing.T, result session.OrphanSweepResult) *[]func(string) bool {
	t.Helper()
	var calls []func(string) bool
	previous := throwawayReapContainersFn
	throwawayReapContainersFn = func(isHome func(string) bool) session.OrphanSweepResult {
		calls = append(calls, isHome)
		return result
	}
	t.Cleanup(func() { throwawayReapContainersFn = previous })
	return &calls
}

// notTheDefaultHome points the default-home check at a directory of the
// test's own, so the homes under test are not refused as the real install.
func notTheDefaultHome(t *testing.T) {
	t.Helper()
	previous := throwawayDefaultHomeFn
	notDefault := t.TempDir()
	throwawayDefaultHomeFn = func() (string, error) { return notDefault, nil }
	t.Cleanup(func() { throwawayDefaultHomeFn = previous })
}

// TestThrowawayContainerReapProvesOnlyThisHome: the container reap asks the
// reaper for the containers of this home, reached by its own path or by a
// symlink to it, and of no other directory.
func TestThrowawayContainerReapProvesOnlyThisHome(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	other := filepath.Join(root, "other")
	require.NoError(t, os.Mkdir(home, 0o700))
	require.NoError(t, os.Mkdir(other, 0o700))
	alias := filepath.Join(root, "alias")
	require.NoError(t, os.Symlink(home, alias))

	notTheDefaultHome(t)
	calls := captureContainerReap(t)

	require.True(t, reapThrowawayHomeContainers(home), "a reap that removed everything it proved is complete")

	require.Len(t, *calls, 1, "the reap must ask for the home's containers")
	isHome := (*calls)[0]
	require.True(t, isHome(home), "a container labelled with the home's own path is this home's")
	require.True(t, isHome(alias), "a container labelled through a symlink to the home is this home's")
	require.False(t, isHome(other), "a container another home created is never this home's")
	require.False(t, isHome(""), "an unlabelled container is never this home's")
}

// TestThrowawayLabelProofRejectsARelativeLabel: a relative af.home label is
// unproven, even when it names this very home from the reaper's cwd. It was
// relative to the creating daemon's cwd, which the reaper cannot know, so two
// throwaways launched from different directories with the same relative home
// would otherwise reap each other's containers.
func TestThrowawayLabelProofRejectsARelativeLabel(t *testing.T) {
	parent := t.TempDir()
	home := filepath.Join(parent, ".af")
	require.NoError(t, os.Mkdir(home, 0o700))
	calls := captureContainerReap(t)

	// From here ".af" names the home.
	t.Chdir(parent)
	reapThrowawayContainers(home)

	require.Len(t, *calls, 1)
	isHome := (*calls)[0]
	require.True(t, isHome(home), "the absolute label still proves the home")
	require.False(t, isHome(".af"), "a relative label must be unproven, whatever it names from the reaper's cwd")
	require.False(t, isHome("./.af"), "a relative label must be unproven, whatever it names from the reaper's cwd")
}

// TestThrowawayLabelProofSkipsAVanishedHome: a label whose directory no longer
// exists — an already-deleted throwaway home — is skipped, never matched, even
// when it spells this home's own path, and so is a label naming something
// other than a directory.
func TestThrowawayLabelProofSkipsAVanishedHome(t *testing.T) {
	t.Run("this home is already gone", func(t *testing.T) {
		home := filepath.Join(t.TempDir(), "gone")
		require.NoError(t, os.Mkdir(home, 0o700))
		// The teardown hands the reap the canonical home, resolved while it
		// still existed.
		home, err := canonicalDir(home)
		require.NoError(t, err)
		require.NoError(t, os.Remove(home))
		calls := captureContainerReap(t)

		reapThrowawayContainers(home)

		for _, isHome := range *calls {
			require.False(t, isHome(home), "a label naming a deleted home must never match it")
		}
	})

	t.Run("the label names a deleted directory or a file", func(t *testing.T) {
		root := t.TempDir()
		home := filepath.Join(root, "home")
		require.NoError(t, os.Mkdir(home, 0o700))
		gone := filepath.Join(root, "gone")
		require.NoError(t, os.Mkdir(gone, 0o700))
		require.NoError(t, os.Remove(gone))
		file := filepath.Join(root, "file")
		require.NoError(t, os.WriteFile(file, nil, 0o600))
		calls := captureContainerReap(t)

		reapThrowawayContainers(home)

		require.Len(t, *calls, 1)
		isHome := (*calls)[0]
		require.False(t, isHome(gone), "a label naming a deleted directory is skipped")
		require.False(t, isHome(file), "a label naming a file is skipped")
	})
}

// TestThrowawayContainerReapNeverTouchesTheDefaultHome: the container reap
// refuses the default install's home before anything else, as the teardown
// does, so the real install's containers are never put to the proof at all.
func TestThrowawayContainerReapNeverTouchesTheDefaultHome(t *testing.T) {
	home := t.TempDir()
	previousDefault := throwawayDefaultHomeFn
	throwawayDefaultHomeFn = func() (string, error) { return home, nil }
	t.Cleanup(func() { throwawayDefaultHomeFn = previousDefault })
	calls := captureContainerReap(t)

	reapThrowawayHomeContainers(home)

	require.Empty(t, *calls, "the default install's home must be refused before its containers are reaped")
}

// TestThrowawayContainerReapIsCompleteOnlyWhenNothingIsLeft: a reap that could
// not list the engine, or left a container unremoved or in an unknown state,
// is incomplete, so the swept home stays registered for a retry.
func TestThrowawayContainerReapIsCompleteOnlyWhenNothingIsLeft(t *testing.T) {
	for name, result := range map[string]session.OrphanSweepResult{
		"the engine could not be listed": {Unlisted: true},
		"a container's state is unknown": {Listed: 1, Unknown: 1},
		"a container was not removed":    {Listed: 1, Errors: 1},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			notTheDefaultHome(t)
			captureContainerReapReporting(t, result)
			require.False(t, reapThrowawayHomeContainers(home))
		})
	}
	t.Run("every proven container went", func(t *testing.T) {
		home := t.TempDir()
		notTheDefaultHome(t)
		captureContainerReapReporting(t, session.OrphanSweepResult{Listed: 2, Reaped: 2})
		require.True(t, reapThrowawayHomeContainers(home))
	})
}

// registerHomes writes a registry holding each home, and returns its dir and
// each home's entry as the registry lists it.
func registerHomes(t *testing.T, homes ...string) (string, map[string]string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "registry")
	require.NoError(t, os.Mkdir(dir, 0o700))
	entries := map[string]string{}
	for _, home := range homes {
		canon, err := canonicalDir(home)
		require.NoError(t, err)
		require.NoError(t, throwawaysandbox.WriteRegistryEntry(dir, canon))
		entries[home] = canon
	}
	return dir, entries
}

func registered(t *testing.T, dir string) []string {
	t.Helper()
	homes, _, err := throwawaysandbox.ReadRegistryHomes(dir)
	require.NoError(t, err)
	return homes
}

// TestThrowawaySweptHomeStaysRegisteredUntilItsContainersAreGone: after
// readiness the worker reaps each swept home's containers and deregisters the
// home only when none is left, so a later marked start can still find the
// rest — the only reap a throwaway nobody restarts will get (Codex on #7448).
func TestThrowawaySweptHomeStaysRegisteredUntilItsContainersAreGone(t *testing.T) {
	done, unfinished := t.TempDir(), t.TempDir()
	dir, entries := registerHomes(t, done, unfinished)
	pending := []throwawayContainerReap{
		{home: entries[done], registered: entries[done]},
		{home: entries[unfinished], registered: entries[unfinished]},
	}

	reapSweptThrowawayContainers(pending, dir, func(home string) bool { return home == entries[done] }, nil)

	require.Equal(t, []string{entries[unfinished]}, registered(t, dir),
		"only the home whose containers are all gone is deregistered")
}

// TestThrowawaySweepLeavesContainerReapsForAfterReadiness: the startup sweep
// tears down an expired sibling's processes but no containers. Docker work,
// with its per-call timeouts, would hold the marked daemon's readiness
// (#5815, Codex on #7448). It hands the home on, still registered, for the
// worker that runs after readiness.
func TestThrowawaySweepLeavesContainerReapsForAfterReadiness(t *testing.T) {
	self := markedHome(t, nil)
	expired := markedHome(t, func(m *throwawaysandbox.Marker) {
		m.CreatedUnix = 1
		m.MaxLifetimeSeconds = 60
	})
	dir, entries := registerHomes(t, expired)
	previousReg, previousNow, previousTeardown, previousReap := throwawayRegistryDirFn, throwawayNowFn, throwawayTeardownFn, throwawayHomeContainerReapFn
	throwawayRegistryDirFn = func() string { return dir }
	throwawayNowFn = func() time.Time { return time.Unix(100000, 0) }
	var tornDown []string
	throwawayTeardownFn = func(home string, _ *throwawaysandbox.Marker) { tornDown = append(tornDown, home) }
	reapedDuringSweep := 0
	throwawayHomeContainerReapFn = func(string) bool { reapedDuringSweep++; return true }
	t.Cleanup(func() {
		throwawayRegistryDirFn, throwawayNowFn, throwawayTeardownFn, throwawayHomeContainerReapFn = previousReg, previousNow, previousTeardown, previousReap
	})

	pending := sweepThrowawaySandboxes(self)

	require.Equal(t, []string{entries[expired]}, tornDown, "the sweep still tears down the expired sibling's processes")
	require.Zero(t, reapedDuringSweep, "the sweep must not reap containers on the readiness path")
	require.Equal(t, []throwawayContainerReap{{home: entries[expired], registered: entries[expired]}}, pending,
		"the sibling is handed on for its container reap")
	require.Equal(t, []string{entries[expired]}, registered(t, dir), "and stays registered until that reap finishes")
}
