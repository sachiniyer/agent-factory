package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/proctree"
)

func makeOldTempAFHome(t *testing.T, tempRoot, name string) string {
	t.Helper()
	dir := filepath.Join(tempRoot, name)
	require.NoError(t, os.MkdirAll(dir, 0755))
	markers := []string{
		filepath.Join(dir, "config.json"),
		filepath.Join(dir, "state.json"),
	}
	for _, marker := range markers {
		require.NoError(t, os.WriteFile(marker, []byte("{}"), 0644))
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, p := range append(markers, dir) {
		require.NoError(t, os.Chtimes(p, old, old))
	}
	return dir
}

func writeOldDaemonPID(t *testing.T, dir string, pid int) {
	t.Helper()
	path := filepath.Join(dir, "daemon.pid")
	require.NoError(t, os.WriteFile(path, []byte(strconv.Itoa(pid)), 0600))
	old := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(path, old, old))
}

func stubDaemonProcessProbe(t *testing.T, alive func(int) bool, argv func(int) []string) {
	t.Helper()
	origAlive := daemonPIDLooksAlive
	origArgv := daemonProcessArgv
	daemonPIDLooksAlive = alive
	daemonProcessArgv = argv
	t.Cleanup(func() {
		daemonPIDLooksAlive = origAlive
		daemonProcessArgv = origArgv
	})
}

func macLikeTempHomeOptions(t *testing.T, tempRoot string, fix bool) Options {
	t.Helper()
	opts := testOptions(t, fix)
	opts.TempDir = tempRoot
	opts.snapshot = func() (map[int]proctree.Process, error) {
		return nil, fmt.Errorf("no /proc")
	}
	opts.Exec = cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("no tmux")
		},
	}
	return opts
}

func TestTempHomeDaemonLivenessUsesDaemonProcessArgv(t *testing.T) {
	tempRoot := t.TempDir()
	dir := makeOldTempAFHome(t, tempRoot, "tmp.live-daemon")
	writeOldDaemonPID(t, dir, 4242)
	stubDaemonProcessProbe(t,
		func(pid int) bool { return pid == 4242 },
		func(pid int) []string {
			require.Equal(t, 4242, pid)
			return []string{"/usr/local/bin/af", "--daemon"}
		},
	)

	report, err := Run(macLikeTempHomeOptions(t, tempRoot, true))
	require.NoError(t, err)
	require.Empty(t, findByCheck(report, "stale-temp-home"))
	require.DirExists(t, dir, "a temp home with a verified live daemon must never be removed")
	require.True(t, okContains(report, "daemon pid is live"))
}

func TestTempHomeDaemonUncertainLivenessSparesHome(t *testing.T) {
	tempRoot := t.TempDir()
	dir := makeOldTempAFHome(t, tempRoot, "tmp.unknown-daemon")
	writeOldDaemonPID(t, dir, 4243)
	stubDaemonProcessProbe(t,
		func(pid int) bool { return pid == 4243 },
		func(pid int) []string { return nil },
	)

	report, err := Run(macLikeTempHomeOptions(t, tempRoot, true))
	require.NoError(t, err)
	require.Empty(t, findByCheck(report, "stale-temp-home"))
	require.DirExists(t, dir, "uncertain daemon liveness must fail closed")
	require.True(t, okContains(report, "daemon.pid liveness is uncertain"))
}

// TestScanContextDefaultsSnapshot pins the invariant broken by #1785: the
// context the checks read must carry a snapshot func even when the caller
// supplied none. Defaulting after the scanContext copies the Options left
// ctx.opts.snapshot nil in production, quietly turning the fix-time process
// recheck in staleTempHomeRemoveFix into dead code that only tests reached.
func TestScanContextDefaultsSnapshot(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	// Production shape: no injected snapshot, exactly as `af doctor` calls it.
	ctx, err := newScanContext(Options{TempDir: t.TempDir()})
	require.NoError(t, err)
	require.NotNil(t, ctx.opts.snapshot,
		"scan context must carry a snapshot func, else the fix-time process recheck is unreachable")
}

// TestStaleTempHomeRefusesWithoutSnapshotFunc pins the fail-closed branch: a
// context with no snapshot func cannot recheck for live processes, so it must
// refuse rather than delete on the stale detection snapshot.
func TestStaleTempHomeRefusesWithoutSnapshotFunc(t *testing.T) {
	tempRoot := t.TempDir()
	dir := makeOldTempAFHome(t, tempRoot, "tmp.no-snapshot-func")
	ctx := &scanContext{opts: Options{TempDir: tempRoot, ConfigDir: t.TempDir()}}

	err := staleTempHomeRemoveFix(ctx, dir)()
	require.ErrorContains(t, err, "no process snapshot available")
	require.DirExists(t, dir, "an un-recheckable home must fail closed, not be removed")
}

// TestStaleTempHomeClaimedAfterDetectionIsSpared is the behavioral contract the
// fix-time recheck exists for: a home that looked abandoned during detection but
// was claimed by a process before --fix ran must survive. The snapshot func hides
// the claimer from the detection pass and reveals it at fix time, standing in for
// a process that started in between.
func TestStaleTempHomeClaimedAfterDetectionIsSpared(t *testing.T) {
	tempRoot := t.TempDir()
	dir := makeOldTempAFHome(t, tempRoot, "tmp.claimed-late")

	claimer := spawnWithEnv(t, "sh", nil, map[string]string{"AGENT_FACTORY_HOME": dir})

	opts := macLikeTempHomeOptions(t, tempRoot, true)
	var calls int
	opts.snapshot = func() (map[int]proctree.Process, error) {
		calls++
		if calls == 1 {
			// Detection: the claimer has not started yet.
			return map[int]proctree.Process{}, nil
		}
		// Fix time: the claimer now holds the home.
		return map[int]proctree.Process{claimer.PID: claimer}, nil
	}

	report, err := Run(opts)
	require.NoError(t, err)
	require.Greater(t, calls, 1, "fix must re-take the process snapshot, not reuse the detection one")

	findings := findByCheck(report, "stale-temp-home")
	require.Len(t, findings, 1, "detection saw an abandoned home and proposed removing it")
	require.False(t, findings[0].Fixed)
	require.ErrorContains(t, findings[0].FixErr, "live process references it")
	require.DirExists(t, dir, "a home claimed between detection and fix must not be removed")
}

func TestTempHomeWithLiveTmuxSessionMarkerSparesHomeWithoutProc(t *testing.T) {
	tempRoot := t.TempDir()
	dir := makeOldTempAFHome(t, tempRoot, "tmp.live-session")
	opts := macLikeTempHomeOptions(t, tempRoot, true)
	opts.Exec = cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			if len(cmd.Args) > 1 && cmd.Args[1] == "ls" {
				return []byte("af_live-session\n"), nil
			}
			if len(cmd.Args) > 1 && cmd.Args[1] == "show-environment" &&
				strings.Contains(strings.Join(cmd.Args, " "), "af_live-session") {
				return []byte("AF_HOME=" + dir + "\n"), nil
			}
			return nil, fmt.Errorf("unexpected tmux command: %s", cmd.String())
		},
	}

	report, err := Run(opts)
	require.NoError(t, err)
	require.Empty(t, findByCheck(report, "stale-temp-home"))
	require.DirExists(t, dir, "a temp home with a live tmux session marker must never be removed")
	require.True(t, okContains(report, "live tmux session references it"))
}

func TestStaleTempHomeFixRechecksDaemonBeforeRemove(t *testing.T) {
	tempRoot := t.TempDir()
	dir := makeOldTempAFHome(t, tempRoot, "tmp.race-daemon")
	alive := false
	stubDaemonProcessProbe(t,
		func(pid int) bool { return alive && pid == 4244 },
		func(pid int) []string {
			if alive && pid == 4244 {
				return []string{"af", "--daemon"}
			}
			return nil
		},
	)

	report, err := Run(macLikeTempHomeOptions(t, tempRoot, false))
	require.NoError(t, err)
	findings := findByCheck(report, "stale-temp-home")
	require.Len(t, findings, 1)
	require.NotNil(t, findings[0].fix)

	alive = true
	writeOldDaemonPID(t, dir, 4244)
	err = findings[0].fix()
	require.Error(t, err)
	require.Contains(t, err.Error(), "daemon pid is live")
	require.DirExists(t, dir, "fix must re-check and refuse to remove a newly active temp home")
}

// TestStaleTempHomeFixFailsClosedWhenSnapshotFailsAtFixTime is the #1728
// regression: detection got a working process snapshot (so the home was
// flagged stale on genuinely-empty data), but the fix-time recheck fails
// (transient /proc error). The detection snapshot is now stale — a one-off
// command with no daemon.pid and no tmux marker could have claimed the home
// in between — so the fix must fail closed rather than delete on stale data.
// Before the fix, the snapshot error was swallowed and the home deleted.
func TestStaleTempHomeFixFailsClosedWhenSnapshotFailsAtFixTime(t *testing.T) {
	tempRoot := t.TempDir()
	dir := makeOldTempAFHome(t, tempRoot, "tmp.snapshot-race")

	opts := testOptions(t, true)
	opts.TempDir = tempRoot
	// No live tmux session references the home.
	opts.Exec = cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error { return nil },
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			return nil, fmt.Errorf("no tmux")
		},
	}
	// Detection (call 1) sees a clean, non-nil snapshot so the home is flagged
	// stale; the fix-time recheck (call 2+) fails, mirroring a transient /proc
	// error between detection and remediation.
	var calls int
	opts.snapshot = func() (map[int]proctree.Process, error) {
		calls++
		if calls == 1 {
			return map[int]proctree.Process{}, nil
		}
		return nil, fmt.Errorf("snapshot unavailable at fix time")
	}

	report, err := Run(opts)
	require.NoError(t, err)

	findings := findByCheck(report, "stale-temp-home")
	require.Len(t, findings, 1, "home must be detected as stale")
	require.GreaterOrEqual(t, calls, 2, "fix must re-take the snapshot at fix time")
	require.False(t, findings[0].Fixed, "fix must fail closed when the fix-time snapshot fails")
	require.Error(t, findings[0].FixErr)
	require.Contains(t, findings[0].FixErr.Error(), "process snapshot failed")
	require.DirExists(t, dir, "an in-use home must never be deleted when liveness cannot be verified")
}
