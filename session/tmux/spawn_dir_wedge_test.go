package tmux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/fsprobe"
	"github.com/stretchr/testify/require"
)

// #5341 + #5295 regression: the missing-worktree spawn gates in Start and
// restoreWithResult's respawn branch must route their workDir stat through
// statSocketDir (fsprobe.Stat), NOT a raw os.Stat. workDir is not confined to
// af's home — sibling worktrees live beside the user's repo, `--here`
// launches in the user's own checkout, and restore reuses persisted paths
// verbatim — so the path can sit on an existing-but-wedged bind/NFS/FUSE
// mount whose stat blocks in an uninterruptible D state. An unbounded stat on
// the daemon's synchronous create/restore path wedges the handler (and any
// lock held under it) forever instead of entering environmental backoff.
// The bounded fsprobe.Stat flight returns the typed
// ErrProbeTimedOut + context.DeadlineExceeded pair on non-answer, which the
// lost-restore environmental classifier reads as the bounded environmental
// class — the same discipline tmuxSocketFilesystemDir and the worktree-add
// probe already keep. These tests pin the routing by overriding the seam.

// wedgedStatErr is the exact shape fsprobe.Stat returns when its bounded
// flight misses its 2s deadline (see internal/fsprobe/fsprobe.go's
// waitForFlight): a "did not answer" wrapper carrying the
// ErrProbeTimedOut + context.DeadlineExceeded pair the lost-restore
// environmental classifier requires. Building it from the same sentinels (not
// a string) keeps errors.Is honest the way the production probe does.
func wedgedStatErr(dir string) error {
	return fmt.Errorf("stat probe of %s did not answer within %s — the filesystem did not answer the stat: %w: %w",
		dir, 2*time.Second, fsprobe.ErrProbeTimedOut, context.DeadlineExceeded)
}

// TestStartWorkDirGateRoutesThroughBoundedStat pins Site 1: Start's
// missing-worktree gate must refuse a wedged workDir through the bounded
// statSocketDir seam, surfacing the typed non-answer evidence in the
// ErrSpawnDirMissing chain so the lost-restore classifier routes the
// session to environmental backoff instead of hanging the create handler.
func TestStartWorkDirGateRoutesThroughBoundedStat(t *testing.T) {
	forceNewSessionEnvMarkers(t, true)
	name := toTmuxName("spawn-wedged-mount", "")
	wedged := filepath.Join(t.TempDir(), "wedged-worktree")

	var spawned bool
	session := newTmuxSession(name, "claude", NewMockPtyFactory(t),
		spawnAttemptExec(t, name, &spawned))

	calls := 0
	old := statSocketDir
	statSocketDir = func(p string) (os.FileInfo, error) {
		calls++
		require.Equal(t, wedged, p,
			"the gate must stat exactly the requested workDir, not a derived path")
		return nil, wedgedStatErr(p)
	}
	defer func() { statSocketDir = old }()

	err := session.Start(wedged)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSpawnDirMissing,
		"a bounded non-answer is still a missing-worktree refusal")
	require.ErrorIs(t, err, ErrSessionNotStarted,
		"the refusal happens before new-session, so no pane can exist")
	require.ErrorIs(t, err, fsprobe.ErrProbeTimedOut,
		"the bounded non-answer's sentinel must ride the chain for the classifier")
	require.ErrorIs(t, err, context.DeadlineExceeded,
		"the sentinel must keep its deadline pair so the classifier accepts it")
	require.Equal(t, 1, calls,
		"the gate must consult the seam exactly once — a raw os.Stat would leave calls at 0")
	require.Contains(t, err.Error(), name)
	require.Contains(t, err.Error(), wedged)
	require.False(t, spawned, "a refused spawn must never issue new-session")
}

// TestStartWorkDirGateConsultsSeamNotRawStat proves the gate routes through
// the seam rather than a raw os.Stat: a path the seam CLAIMS is a directory is
// admitted past the gate even when the real filesystem says it does not
// exist. The inverse of the wedge test — the seam IS the verdict, and a fix
// that falls back to os.Stat would refuse here.
func TestStartWorkDirGateConsultsSeamNotRawStat(t *testing.T) {
	forceNewSessionEnvMarkers(t, true)
	name := toTmuxName("spawn-seam-verdict", "")
	missing := filepath.Join(t.TempDir(), "does-not-exist-on-disk")

	// Precondition: the path genuinely does not exist on the real filesystem,
	// so a raw os.Stat gate would refuse and never reach new-session.
	_, statErr := os.Stat(missing)
	require.Error(t, statErr, "precondition: the path must not exist on disk")

	var probed bool
	exec_ := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			s := c.String()
			switch {
			case strings.Contains(s, "has-session"):
				if !probed {
					probed = true
					return determinateAbsence(t, name)
				}
				return nil
			}
			return nil
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("no server running")
		},
	}
	session := newTmuxSession(name, "claude", NewMockPtyFactory(t), exec_)

	seamCalls := 0
	old := statSocketDir
	statSocketDir = func(p string) (os.FileInfo, error) {
		seamCalls++
		require.Equal(t, missing, p)
		// The seam says "directory exists" even though the disk says ENOENT.
		// Return a real FileInfo for an unrelated directory so IsDir() is
		// true without constructing a mockFileInfo.
		return os.Stat(t.TempDir())
	}
	defer func() { statSocketDir = old }()

	// The spawn proceeds past the gate; whatever fails downstream is not
	// ErrSpawnDirMissing.
	err := session.Start(missing)
	require.Equal(t, 1, seamCalls,
		"the gate must consult the seam exactly once and accept its verdict")
	if err != nil {
		require.NotErrorIs(t, err, ErrSpawnDirMissing,
			"the seam's directory verdict must admit the spawn — a raw os.Stat gate would refuse here: %v", err)
	}
}

// TestStartWorkDirGateBoundedNonAnswerDoesNotHang is the hang regression for
// Site 1: the BUG was an unbounded os.Stat that blocked the daemon's create
// handler forever on a wedged mount. fsprobe.Stat bounds the lookup. To
// prove the gate uses the bounded seam, not a raw os.Stat, this test:
//   - restores statSocketDir to the real fsprobe.Stat (so the production
//     bounded flight is what runs), and
//   - injects a never-answered stat through fsprobe's own statPath seam,
//     standing in for a wedged NFS hard mount / hung FUSE daemon — the
//     D-state syscall the bound exists to catch.
//
// A fix that falls back to os.Stat would hang past the test timeout.
func TestStartWorkDirGateBoundedNonAnswerDoesNotHang(t *testing.T) {
	oldSeam := statSocketDir
	statSocketDir = fsprobe.Stat
	defer func() { statSocketDir = oldSeam }()

	wedged := filepath.Join(t.TempDir(), "wedged-forever")
	require.NoError(t, os.Mkdir(wedged, 0o755))

	// The wedged stat blocks on release so the test can unblock and drain the
	// in-flight fsprobe worker BEFORE restoring the statPath/probeTimeout
	// globals: the worker reads statPath (fsprobe.go's Stat closure) inside
	// its own goroutine, so restoring statPath while that goroutine is still
	// alive is a data race under -race. closing release lets the worker
	// return, wg.Wait() establishes the happens-before edge, and only then
	// do the SetStatPathForTest/SetProbeTimeoutForTest restores run.
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	restoreStatPath := fsprobe.SetStatPathForTest(func(string) (os.FileInfo, error) {
		defer wg.Done()
		<-release
		return nil, nil
	})
	defer restoreStatPath()
	// Shrink the fsprobe bound so the test stays fast; the regression
	// property is "bounded at all", not the specific 2s value.
	restoreTimeout := fsprobe.SetProbeTimeoutForTest(200 * time.Millisecond)
	defer restoreTimeout()
	defer func() {
		close(release)
		wg.Wait()
	}()

	forceNewSessionEnvMarkers(t, true)
	name := toTmuxName("spawn-stat-hang", "")
	var spawned bool
	session := newTmuxSession(name, "claude", NewMockPtyFactory(t),
		spawnAttemptExec(t, name, &spawned))

	done := make(chan error, 1)
	go func() { done <- session.Start(wedged) }()

	select {
	case err := <-done:
		require.Error(t, err)
		require.ErrorIs(t, err, ErrSpawnDirMissing,
			"a wedged-worktree stat must still refuse as a missing-worktree gate")
		require.ErrorIs(t, err, fsprobe.ErrProbeTimedOut,
			"the bounded non-answer sentinel must ride the chain")
		require.ErrorIs(t, err, context.DeadlineExceeded,
			"the sentinel must keep its deadline pair")
		require.False(t, spawned, "a wedged-worktree spawn must never issue new-session")
	case <-time.After(10 * time.Second):
		t.Fatal("Start hung on a wedged worktree stat — the gate must use the bounded fsprobe.Stat seam, not a raw unbounded os.Stat (#5295)")
	}
}

// TestRestoreWorkDirGateRoutesThroughBoundedStat pins Site 2:
// restoreWithResult's respawn-branch gate must refuse a wedged workDir
// through the bounded statSocketDir seam. The refusal returns
// RestoreReattached (not RestoreRespawned) so callers do not retire
// lost-state facts on a refused respawn, and the typed evidence rides the
// ErrSpawnDirMissing chain for the lost-restore environmental classifier.
func TestRestoreWorkDirGateRoutesThroughBoundedStat(t *testing.T) {
	forceNewSessionEnvMarkers(t, true)
	name := toTmuxName("restore-wedged-mount", "")
	wedged := filepath.Join(t.TempDir(), "wedged-worktree")

	var spawned bool
	session := newTmuxSession(name, "claude", NewMockPtyFactory(t),
		spawnAttemptExec(t, name, &spawned))

	calls := 0
	old := statSocketDir
	statSocketDir = func(p string) (os.FileInfo, error) {
		calls++
		require.Equal(t, wedged, p)
		return nil, wedgedStatErr(p)
	}
	defer func() { statSocketDir = old }()

	res, err := session.RestoreWithResult(wedged)
	require.Error(t, err)
	require.Equal(t, RestoreReattached, res,
		"a refused respawn is not a respawn — callers must not retire lost-state facts on it")
	require.ErrorIs(t, err, ErrSpawnDirMissing,
		"a bounded non-answer is still a missing-worktree refusal")
	require.ErrorIs(t, err, fsprobe.ErrProbeTimedOut,
		"the bounded non-answer sentinel must ride the chain for the classifier")
	require.ErrorIs(t, err, context.DeadlineExceeded,
		"the sentinel must keep its deadline pair")
	require.Equal(t, 1, calls,
		"the gate must consult the seam exactly once — a raw os.Stat would leave calls at 0")
	require.Contains(t, err.Error(), name)
	require.Contains(t, err.Error(), wedged)
	require.Contains(t, err.Error(), "unavailable")
	require.False(t, spawned, "the respawn must not reach new-session")
}

// TestRestoreWorkDirGateBoundedNonAnswerDoesNotHang is the hang regression
// for Site 2: the restore poll runs on the daemon's synchronous startup
// restore lane (which holds the manager mutex and the per-session op-lock),
// so an unbounded os.Stat wedges every RPC that needs those locks until a
// daemon restart. The gate must use the bounded fsprobe.Stat seam.
func TestRestoreWorkDirGateBoundedNonAnswerDoesNotHang(t *testing.T) {
	oldSeam := statSocketDir
	statSocketDir = fsprobe.Stat
	defer func() { statSocketDir = oldSeam }()

	wedged := filepath.Join(t.TempDir(), "restore-wedged-forever")
	require.NoError(t, os.Mkdir(wedged, 0o755))

	// See TestStartWorkDirGateBoundedNonAnswerDoesNotHang: the wedged stat
	// blocks on release so the in-flight fsprobe worker drains (wg.Wait)
	// before the statPath/probeTimeout globals are restored, avoiding a
	// -race data race on statPath between the worker's read and the
	// SetStatPathForTest restore.
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	restoreStatPath := fsprobe.SetStatPathForTest(func(string) (os.FileInfo, error) {
		defer wg.Done()
		<-release
		return nil, nil
	})
	defer restoreStatPath()
	restoreTimeout := fsprobe.SetProbeTimeoutForTest(200 * time.Millisecond)
	defer restoreTimeout()
	defer func() {
		close(release)
		wg.Wait()
	}()

	forceNewSessionEnvMarkers(t, true)
	name := toTmuxName("restore-stat-hang", "")
	var spawned bool
	session := newTmuxSession(name, "claude", NewMockPtyFactory(t),
		spawnAttemptExec(t, name, &spawned))

	type result struct {
		res RestoreResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := session.RestoreWithResult(wedged)
		done <- result{res, err}
	}()

	select {
	case got := <-done:
		require.Error(t, got.err)
		require.Equal(t, RestoreReattached, got.res,
			"a refused respawn is not a respawn")
		require.ErrorIs(t, got.err, ErrSpawnDirMissing)
		require.ErrorIs(t, got.err, fsprobe.ErrProbeTimedOut)
		require.ErrorIs(t, got.err, context.DeadlineExceeded)
		require.False(t, spawned, "a wedged-worktree respawn must never issue new-session")
	case <-time.After(10 * time.Second):
		t.Fatal("Restore hung on a wedged worktree stat — the respawn gate must use the bounded fsprobe.Stat seam, not a raw unbounded os.Stat (#5295)")
	}
}

// TestWorkDirGateMissingDirStillRefuses is the no-regression half: routing
// through the bounded seam must not change the existing #5341 missing /
// not-a-directory semantics. A genuinely missing path and a path that
// exists but is a file both still refuse through ErrSpawnDirMissing, and the
// refusal still happens before new-session. (spawn_dir_refuse_test.go covers
// the same behavior on the real os.Stat path; this test pins it against the
// seam routing, so a regression that swaps ENOENT handling for a
// timeout-shaped verdict is caught.)
func TestWorkDirGateMissingDirStillRefuses(t *testing.T) {
	forceNewSessionEnvMarkers(t, true)
	// Use the real fsprobe.Stat seam so a missing path surfaces a real ENOENT,
	// not a wedge override. The fix must preserve ENOENT pass-through.
	oldSeam := statSocketDir
	statSocketDir = fsprobe.Stat
	defer func() { statSocketDir = oldSeam }()

	// Start gate — missing dir.
	name := toTmuxName("seam-missing-start", "")
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	var spawned bool
	session := newTmuxSession(name, "claude", NewMockPtyFactory(t),
		spawnAttemptExec(t, name, &spawned))
	err := session.Start(missing)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrSpawnDirMissing)
	require.ErrorIs(t, err, ErrSessionNotStarted)
	// The missing-path refusal must NOT carry the bounded non-answer pair —
	// fsprobe.Stat surfaces a real ENOENT, a definitive verdict, not a wedged
	// non-answer.
	require.NotErrorIs(t, err, fsprobe.ErrProbeTimedOut,
		"a missing path is a real ENOENT, not a bounded non-answer: %v", err)
	require.NotErrorIs(t, err, context.DeadlineExceeded,
		"a missing path is a real ENOENT, not a bounded non-answer: %v", err)
	require.False(t, spawned, "a refused spawn must never issue new-session")

	// Start gate — file where a dir is expected.
	fileName := toTmuxName("seam-file-start", "")
	file := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))
	var fileSpawned bool
	fileSession := newTmuxSession(fileName, "claude", NewMockPtyFactory(t),
		spawnAttemptExec(t, fileName, &fileSpawned))
	fileErr := fileSession.Start(file)
	require.ErrorIs(t, fileErr, ErrSpawnDirMissing)
	require.Contains(t, fileErr.Error(), "not a directory")
	require.False(t, fileSpawned)

	// Restore gate — missing dir still refuses with RestoreReattached.
	restoreName := toTmuxName("seam-missing-restore", "")
	missingRestore := filepath.Join(t.TempDir(), "no-such-restore")
	var restoreSpawned bool
	restoreSession := newTmuxSession(restoreName, "claude", NewMockPtyFactory(t),
		spawnAttemptExec(t, restoreName, &restoreSpawned))
	res, restoreErr := restoreSession.RestoreWithResult(missingRestore)
	require.Error(t, restoreErr)
	require.Equal(t, RestoreReattached, res)
	require.ErrorIs(t, restoreErr, ErrSpawnDirMissing)
	require.NotErrorIs(t, restoreErr, fsprobe.ErrProbeTimedOut)
	require.NotErrorIs(t, restoreErr, context.DeadlineExceeded)
	require.False(t, restoreSpawned)
}
