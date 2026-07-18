package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/daemon"
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

// stubTempHomeLockProbe forces the lock-probe outcome for a test, so the
// report/delete decisions can be driven off each of the three answers
// (held / takeable / unprovable) without a real daemon or a real NFS mount. The
// real probe mechanics are covered by the daemon package's ProbeHomeLock tests.
func stubTempHomeLockProbe(t *testing.T, answer func(dir string) daemon.ProbeAnswer) {
	t.Helper()
	prev := tempHomeLockProbe
	tempHomeLockProbe = answer
	t.Cleanup(func() { tempHomeLockProbe = prev })
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

// holdDaemonLock creates dir's daemon.lock and holds an exclusive flock on it
// for the whole test — exactly what a running daemon does. flock contends
// across open-file-descriptions the same in one process as across two, so this
// is a faithful stand-in for a live daemon owning the home.
func holdDaemonLock(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, "daemon.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	require.NoError(t, syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	// Creating the lock file refreshed dir's mtime; age it back so the home is
	// old enough to be a delete candidate. Otherwise the age gate would spare it
	// for looking fresh, masking whether the LOCK is what spares it.
	old := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(path, old, old))
	require.NoError(t, os.Chtimes(dir, old, old))
}

// TestTempHomeWithHeldDaemonLockIsSpared is the #1989 fail-first: a temp home
// whose daemon.lock is held by a LIVE daemon must never be reported stale or
// removed, even under --fix — and even when the process/pid surface the old
// scan relied on shows nothing (this run has no /proc and no tmux).
//
// Observed failing at 307f159: the old code has no lock probe, so its process
// scan finds no reference, reads the home as abandoned, and REPORTS it stale —
// the assertion that nothing is reported then fails. The lock probe is what
// turns "I saw no process" into the sound "a daemon holds the lock → in use".
func TestTempHomeWithHeldDaemonLockIsSpared(t *testing.T) {
	tempRoot := t.TempDir()
	dir := makeOldTempAFHome(t, tempRoot, "tmp.live-daemon-lock")
	holdDaemonLock(t, dir)

	report, err := Run(macLikeTempHomeOptions(t, tempRoot, true)) // Fix: true
	require.NoError(t, err)
	require.Empty(t, findByCheck(report, "stale-temp-home"),
		"a temp home whose daemon.lock is held by a live daemon must never be reported stale or removed")
	require.DirExists(t, dir)
	require.True(t, okContains(report, "an af daemon holds its lock"),
		"doctor should report the held-lock home as in use")
}

// TestTakeableLockTempHomeRemovedOnFix restores the teeth on a FACT: a temp home
// whose lock is takeable (no live daemon) and which no live tmux session names
// is provably unused, so --fix removes it. The lock outcome is forced to the
// proven-free answer so the test is deterministic on any filesystem; the real
// takeable-vs-untrusted mechanics live in the daemon package tests.
//
// Observed failing when the checkStaleTempHomes delete-arm is reverted to
// report-only (#1969's report-only state): the home is reported but not removed,
// so the require.True(Fixed) / require.NoDirExists assertions fail.
func TestTakeableLockTempHomeRemovedOnFix(t *testing.T) {
	tempRoot := t.TempDir()
	dir := makeOldTempAFHome(t, tempRoot, "tmp.abandoned")
	stubTempHomeLockProbe(t, func(string) daemon.ProbeAnswer { return daemon.AnswerNo() })

	report, err := Run(macLikeTempHomeOptions(t, tempRoot, true)) // Fix: true
	require.NoError(t, err)
	findings := findByCheck(report, "stale-temp-home")
	require.Len(t, findings, 1)
	require.True(t, findings[0].Fixed,
		"a home with a takeable lock and no live tmux session must be removed on --fix")
	require.NoDirExists(t, dir, "the provably-unused home must be gone")
}

// TestTakeableLockTempHomeReportedButNotRemovedWithoutFix: without --fix the
// same provably-unused home is REPORTED with a fix action and left on disk.
func TestTakeableLockTempHomeReportedButNotRemovedWithoutFix(t *testing.T) {
	tempRoot := t.TempDir()
	dir := makeOldTempAFHome(t, tempRoot, "tmp.abandoned")
	stubTempHomeLockProbe(t, func(string) daemon.ProbeAnswer { return daemon.AnswerNo() })

	report, err := Run(macLikeTempHomeOptions(t, tempRoot, false)) // report-only
	require.NoError(t, err)
	findings := findByCheck(report, "stale-temp-home")
	require.Len(t, findings, 1)
	require.NotEmpty(t, findings[0].FixAction, "a provably-unused home must be offered for removal")
	require.False(t, findings[0].Fixed)
	require.DirExists(t, dir, "a report-only run must not remove anything")
}

// TestUnprovableLockTempHomeReportedNotRemoved: when the lock answer is
// Undetermined — no lock file, or a filesystem whose flock cannot be trusted —
// the home is REPORTED but carries NO fix action and is never removed, even
// under --fix. Unknown is not a licence to os.RemoveAll (#1989).
func TestUnprovableLockTempHomeReportedNotRemoved(t *testing.T) {
	tempRoot := t.TempDir()
	dir := makeOldTempAFHome(t, tempRoot, "tmp.unprovable")
	stubTempHomeLockProbe(t, func(string) daemon.ProbeAnswer {
		return daemon.Undetermined(fmt.Errorf("simulated untrusted filesystem"))
	})

	report, err := Run(macLikeTempHomeOptions(t, tempRoot, true)) // Fix: true
	require.NoError(t, err)
	findings := findByCheck(report, "stale-temp-home")
	require.Len(t, findings, 1)
	require.Empty(t, findings[0].FixAction,
		"an unprovable home must not authorise a delete: unknown is not proof of non-use")
	require.False(t, findings[0].Fixed)
	require.DirExists(t, dir, "a home we cannot prove unused must never be removed")
	require.Contains(t, findings[0].Detail, "untrusted filesystem",
		"the report must name WHY it could not decide, or the operator cannot act on it")
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

// TestTempHomeWithLiveTmuxSessionMarkerSparesHomeWithoutProc: a live tmux
// session naming the home spares it even with no process table and no daemon
// lock — the second, sound signal (tmux show-environment) the lock cannot see.
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
	require.True(t, okContains(report, "a live tmux session references it"))
}

// TestStaleTempHomeFixRefusesWhenDaemonAppearsAfterDetection: findings are
// applied AFTER detection, so a daemon can start in the window between. The fix
// re-probes the lock and must REFUSE rather than rm -rf a home that is now
// owned. Detection sees a takeable lock; by fix time a daemon holds it.
func TestStaleTempHomeFixRefusesWhenDaemonAppearsAfterDetection(t *testing.T) {
	tempRoot := t.TempDir()
	dir := makeOldTempAFHome(t, tempRoot, "tmp.claimed")

	probes := 0
	stubTempHomeLockProbe(t, func(string) daemon.ProbeAnswer {
		probes++
		if probes == 1 {
			return daemon.AnswerNo() // detection: provably free
		}
		return daemon.AnswerYes() // fix time: a daemon has claimed it
	})

	report, err := Run(macLikeTempHomeOptions(t, tempRoot, true)) // Fix: true
	require.NoError(t, err)
	findings := findByCheck(report, "stale-temp-home")
	require.Len(t, findings, 1)
	require.False(t, findings[0].Fixed, "a home claimed between detection and fix must not be removed")
	require.Error(t, findings[0].FixErr, "the refusal must surface as a fix error, not a silent skip")
	require.Contains(t, findings[0].FixErr.Error(), "cannot prove no daemon owns it")
	require.DirExists(t, dir, "the newly-claimed home must survive the --fix run")
	require.GreaterOrEqual(t, probes, 2, "the fix must re-probe the lock rather than trust detection")
}
