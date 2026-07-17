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
// ctx.opts.snapshot nil in production, so the checks silently saw no processes
// at all — the blindness class this package now reports rather than hides.
func TestScanContextDefaultsSnapshot(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	// Production shape: no injected snapshot, exactly as `af doctor` calls it.
	ctx, err := newScanContext(Options{TempDir: t.TempDir()})
	require.NoError(t, err)
	require.NotNil(t, ctx.opts.snapshot,
		"scan context must carry a snapshot func, else the fix-time process recheck is unreachable")
}

// TestStaleTempHomeRefusesWithoutSnapshotFunc and
// TestStaleTempHomeClaimedAfterDetectionIsSpared are DELETED with the closure
// they drove (staleTempHomeRemoveFix).
//
// They were good tests of a bad idea. One pinned that an un-recheckable home
// fails closed; the other that a home claimed between detection and --fix
// survives. Both hardened the INPUTS to "did I see a process referencing this
// home?" — and that question is unanswerable by process inspection, so no
// amount of input-hardening made the rm -rf it authorised safe. The delete is
// gone; there is nothing left to fail closed.
//
// What replaced them: TestStaleTempHomeReportedButNeverRemoved (doctor_test.go)
// asserts a --fix run REPORTS the home and leaves it on disk.

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

// TestStaleTempHomeFixRechecksDaemonBeforeRemove is DELETED with the closure it
// drove — the fourth and last test whose subject was the fix-time recheck. It
// asserted the fix re-reads daemon.pid and refuses a home that became active in
// between. Sound reasoning, wrong layer: it made the rm -rf's inputs fresher,
// not its question answerable. There is no fix to recheck now.

// TestStaleTempHomeFixFailsClosedWhenSnapshotFailsAtFixTime is the #1728
// regression: detection got a working process snapshot (so the home was
// flagged stale on genuinely-empty data), but the fix-time recheck fails
// (transient /proc error). The detection snapshot is now stale — a one-off
// command with no daemon.pid and no tmux marker could have claimed the home
// in between — so the fix must fail closed rather than delete on stale data.
/// TestStaleTempHomeFixFailsClosedWhenSnapshotFailsAtFixTime is DELETED with the
// closure it drove, for the same reason as its two siblings above: it hardened
// an INPUT ("re-take the snapshot at fix time, fail closed if it fails") to a
// question process inspection cannot answer. Its final assertion — that an
// unverifiable home is never deleted — is now structurally true, because
// nothing deletes. TestStaleTempHomeReportedButNeverRemoved asserts it directly.
