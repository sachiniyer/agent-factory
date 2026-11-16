package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/throwawaysandbox"
	"github.com/sachiniyer/agent-factory/session"
)

// #7447: tearing down an expired throwaway removes the session containers its
// home created, each proven by its own af.home label: nothing else reaps them
// once that home's daemon never starts again.

// captureContainerReap replaces the container reaper and returns the proof
// each call was given; the reaper itself is session.ReapHomeContainers, which
// session's own tests pin.
func captureContainerReap(t *testing.T) *[]func(string) bool {
	t.Helper()
	var calls []func(string) bool
	previous := throwawayReapContainersFn
	throwawayReapContainersFn = func(isHome func(string) bool) session.OrphanSweepResult {
		calls = append(calls, isHome)
		return session.OrphanSweepResult{}
	}
	t.Cleanup(func() { throwawayReapContainersFn = previous })
	return &calls
}

// TestThrowawayTeardownReapsTheHomesContainers: the teardown asks the reaper
// for the containers of this home, reached by its own path or by a symlink to
// it, and of no other directory.
func TestThrowawayTeardownReapsTheHomesContainers(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	other := filepath.Join(root, "other")
	require.NoError(t, os.Mkdir(home, 0o700))
	require.NoError(t, os.Mkdir(other, 0o700))
	alias := filepath.Join(root, "alias")
	require.NoError(t, os.Symlink(home, alias))

	previousDefault := throwawayDefaultHomeFn
	notDefault := t.TempDir()
	throwawayDefaultHomeFn = func() (string, error) { return notDefault, nil }
	t.Cleanup(func() { throwawayDefaultHomeFn = previousDefault })
	calls := captureContainerReap(t)

	// No tmux dir and no recorded pids: the containers are all there is to reap.
	teardownThrowawaySandbox(home, &throwawaysandbox.Marker{Version: 1, CreatedUnix: 1, MaxLifetimeSeconds: 1})

	require.Len(t, *calls, 1, "the teardown must reap the home's containers")
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
