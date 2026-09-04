package doctor

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// #3845 section 2. Two classes of debris that no doctor check could see.
//
// `af --daemon` processes running from a temp-dir binary: checkOrphanedProcesses
// counts descendants of dead tmux sessions, which these are not, and
// checkForeignDaemons only escalates a daemon whose HOME is missing — and these
// kept their homes alive by re-creating them, which is what #3845's first half
// fixes. Three of them ran from /tmp/Test*/001/af for eleven days.
//
// And directories under the temp dir holding nothing but a daemon-http.sock:
// isAFHome needs two markers before it will look at a directory at all, and that
// socket is not one of them, so all 9,892 of them were invisible.

// stubDaemonExe fixes what doctor reads from /proc/<pid>/exe. Staged rather than
// observed: a test cannot exec a binary and then have the kernel report it as
// deleted or as living somewhere it does not.
func stubDaemonExe(t *testing.T, bins map[int]daemonBinary) {
	t.Helper()
	prevExe, prevReadable := daemonProcessExe, exePathsReadable
	t.Cleanup(func() { daemonProcessExe, exePathsReadable = prevExe, prevReadable })
	daemonProcessExe = func(pid int) daemonBinary { return bins[pid] }
	// Pinned so these tests assert doctor's CLASSIFICATION on every runner,
	// including the macOS one where /proc/<pid>/exe does not exist. The platform
	// branch has its own test below.
	exePathsReadable = func() bool { return true }
}

// A platform with no readable exe path earns ONE row, not one per daemon — and
// none at all when there is no daemon to have said anything about.
func TestAPlatformWithNoReadableBinaryPathSaysSoOnce(t *testing.T) {
	first := spawnWithEnv(t, "daemon-one", nil, nil)
	second := spawnWithEnv(t, "daemon-two", nil, nil)
	opts := testOptions(t, false, first.PID, second.PID)
	stubDaemonProcessArgv(t, func(int) []string { return []string{"/usr/local/bin/af", "--daemon"} })
	prev := exePathsReadable
	t.Cleanup(func() { exePathsReadable = prev })
	exePathsReadable = func() bool { return false }

	report, err := Run(opts)
	require.NoError(t, err)

	require.Empty(t, findByCheck(report, checkLeakedDaemon),
		"a platform fact must not become a per-process finding")
	rows := 0
	for _, c := range report.Checks {
		if c.Name == "leaked-daemons" {
			rows++
			require.Equal(t, StatusWarn, c.Status)
			require.False(t, c.Problem, "the user cannot fix the platform")
		}
	}
	require.Equal(t, 1, rows, "exactly one row for the platform, whatever the daemon count")
}

// stubDeadSocketProbe forces the socket dial's outcome so a test can stage each
// of the three answers without depending on the machine's networking.
func stubDeadSocketProbe(t *testing.T, answer func(path string) daemon.ProbeAnswer) {
	t.Helper()
	prev := deadSocketProbe
	t.Cleanup(func() { deadSocketProbe = prev })
	deadSocketProbe = answer
}

// stubUnreadableEnviron makes one pid's environ unreadable while leaving every
// other pid's real facts alone — the state a process we do not own presents, and
// the one daemonProcessHome turns into "which home it serves is unknown".
func stubUnreadableEnviron(t *testing.T, pid int) {
	t.Helper()
	prev := daemonProcessEnvLookup
	t.Cleanup(func() { daemonProcessEnvLookup = prev })
	daemonProcessEnvLookup = func(p int, key string) (string, bool, error) {
		if p == pid {
			return "", false, os.ErrPermission
		}
		return prev(p, key)
	}
}

// leakedDaemonOptions stages one spawned process as a daemon doctor can classify:
// daemon-shaped argv, a readable home, and an age past the kill window.
func leakedDaemonOptions(t *testing.T, fix bool, p proctree.Process, home string) Options {
	t.Helper()
	opts := testOptions(t, fix, p.PID)
	stubDaemonProcessArgv(t, func(int) []string { return []string{"/tmp/whatever/af", "--daemon"} })
	stubProcessHomes(t, map[int]string{p.PID: home})
	// A fixture spawned a millisecond ago stands for a daemon that has been up
	// for eleven days; the real window is exercised by
	// TestAYoungTempBinaryDaemonIsReportedButNotOfferedForAKill.
	opts.minLeakedDaemonAge = time.Nanosecond
	return opts
}

// tempBinaryDaemon is the #3842 shape: an af daemon whose own binary lives under
// a `go test` temp directory.
func tempBinaryDaemon(tempDir string) daemonBinary {
	return daemonBinary{path: filepath.Join(tempDir, "TestSomething123", "001", "af"), known: true}
}

func TestADaemonRunningFromATempBinaryIsReportedAndOfferedForAKill(t *testing.T) {
	proc := spawnWithEnv(t, "leaky-daemon", nil, nil)
	otherHome := t.TempDir()
	opts := leakedDaemonOptions(t, false, proc, otherHome)
	stubDaemonExe(t, map[int]daemonBinary{proc.PID: tempBinaryDaemon(opts.TempDir)})

	report, err := Run(opts)
	require.NoError(t, err)

	findings := findByCheck(report, checkLeakedDaemon)
	require.Len(t, findings, 1)
	require.True(t, findings[0].Actionable,
		"a daemon running af from the temp dir is not an install; it is debris")
	require.NotEmpty(t, findings[0].FixAction, "it must be offered for a kill")
	require.Contains(t, findings[0].Detail, "which is not an install")
	require.Contains(t, findings[0].Detail, otherHome, "the row must name the home it serves")
}

// Paired with a leaked daemon in the SAME run, deliberately. "No finding" is
// what a check that never ran also produces, so the negative is only worth
// anything next to a positive the same run had to find.
func TestAnInstalledDaemonBinaryIsNotReportedAtAll(t *testing.T) {
	healthy := spawnWithEnv(t, "healthy-daemon", nil, nil)
	leaked := spawnWithEnv(t, "leaky-daemon", nil, nil)
	opts := testOptions(t, false, healthy.PID, leaked.PID)
	stubDaemonProcessArgv(t, func(int) []string { return []string{"/tmp/whatever/af", "--daemon"} })
	stubProcessHomes(t, map[int]string{healthy.PID: t.TempDir(), leaked.PID: t.TempDir()})
	opts.minLeakedDaemonAge = time.Nanosecond
	stubDaemonExe(t, map[int]daemonBinary{
		healthy.PID: {path: "/usr/local/bin/af", known: true},
		leaked.PID:  tempBinaryDaemon(opts.TempDir),
	})

	report, err := Run(opts)
	require.NoError(t, err)

	findings := findByCheck(report, checkLeakedDaemon)
	require.Len(t, findings, 1, "exactly the temp-binary daemon, and not the installed one")
	require.Contains(t, findings[0].Detail, fmt.Sprintf("pid %d", leaked.PID))
	require.NotContains(t, findings[0].Detail, fmt.Sprintf("pid %d", healthy.PID))
}

// The departure from the issue's "a real install's daemon never [has a deleted
// binary]". It does: `af upgrade` replaces the binary in place, so every healthy
// daemon reads as deleted until it restarts — the reason watchDaemonHome refuses
// to check the binary path at all (#1093). Treating that as debris would offer
// to kill the working daemon of everyone who has just upgraded.
func TestADeletedInstallBinaryIsAnUpgradeNotALeak(t *testing.T) {
	proc := spawnWithEnv(t, "upgraded-daemon", nil, nil)
	opts := leakedDaemonOptions(t, true, proc, t.TempDir()) // Fix: true
	stubDaemonExe(t, map[int]daemonBinary{proc.PID: {path: "/usr/local/bin/af", deleted: true, known: true}})

	report, err := Run(opts)
	require.NoError(t, err)

	findings := findByCheck(report, checkLeakedDaemon)
	require.Len(t, findings, 1)
	require.False(t, findings[0].Actionable, "a replaced binary is not an unhealthy machine")
	require.Empty(t, findings[0].FixAction, "an upgraded daemon must never be offered for a kill")
	require.Contains(t, findings[0].Detail, "af upgrade")
	require.True(t, proctree.AliveSame(proc), "--fix must not have killed the upgraded daemon")
}

func TestADeletedTempBinaryIsStillRecognisedAsATempBinary(t *testing.T) {
	proc := spawnWithEnv(t, "leaky-deleted-daemon", nil, nil)
	opts := leakedDaemonOptions(t, false, proc, t.TempDir())
	bin := tempBinaryDaemon(opts.TempDir)
	bin.deleted = true
	stubDaemonExe(t, map[int]daemonBinary{proc.PID: bin})

	report, err := Run(opts)
	require.NoError(t, err)

	findings := findByCheck(report, checkLeakedDaemon)
	require.Len(t, findings, 1)
	require.True(t, findings[0].Actionable,
		"the deleted suffix must be stripped before the temp-dir test, or a deleted test binary escapes")
	require.Contains(t, findings[0].Detail, "since deleted")
}

// The daemon serving THIS home is the one answering this very run. However odd
// its binary path, offering to kill it would take the machine down mid-diagnosis.
func TestATempBinaryDaemonServingTheActiveHomeIsNeverOfferedForAKill(t *testing.T) {
	proc := spawnWithEnv(t, "dev-build-daemon", nil, nil)
	opts := testOptions(t, true, proc.PID) // Fix: true
	stubDaemonProcessArgv(t, func(int) []string { return []string{"/tmp/build/af", "--daemon"} })
	stubProcessHomes(t, map[int]string{proc.PID: opts.ConfigDir})
	opts.minLeakedDaemonAge = time.Nanosecond
	stubDaemonExe(t, map[int]daemonBinary{proc.PID: tempBinaryDaemon(opts.TempDir)})

	report, err := Run(opts)
	require.NoError(t, err)

	findings := findByCheck(report, checkLeakedDaemon)
	require.Len(t, findings, 1)
	require.False(t, findings[0].Actionable)
	require.Empty(t, findings[0].FixAction)
	require.Contains(t, findings[0].Detail, "serves THIS home")
	require.True(t, proctree.AliveSame(proc), "--fix must never kill the daemon serving the active home")
}

// "I could not read its home" is not "its home is not mine", and this is a kill.
func TestATempBinaryDaemonWithAnUnreadableHomeIsReportedNotKilled(t *testing.T) {
	proc := spawnWithEnv(t, "unreadable-home-daemon", nil, nil)
	opts := testOptions(t, true, proc.PID) // Fix: true
	stubDaemonProcessArgv(t, func(int) []string { return []string{"/tmp/build/af", "--daemon"} })
	opts.minLeakedDaemonAge = time.Nanosecond
	stubDaemonExe(t, map[int]daemonBinary{proc.PID: tempBinaryDaemon(opts.TempDir)})
	stubUnreadableEnviron(t, proc.PID)

	report, err := Run(opts)
	require.NoError(t, err)

	findings := findByCheck(report, checkLeakedDaemon)
	require.Len(t, findings, 1)
	require.False(t, findings[0].Actionable)
	require.Empty(t, findings[0].FixAction)
	require.Contains(t, findings[0].Detail, "unreadable")
	require.True(t, proctree.AliveSame(proc))
}

// The window exists for one process: a `go test` run that is still going. Its
// daemons are indistinguishable from the leaked ones by path and argv, and
// killing one mid-suite makes this check a flaky-test generator.
func TestAYoungTempBinaryDaemonIsReportedButNotOfferedForAKill(t *testing.T) {
	proc := spawnWithEnv(t, "test-run-daemon", nil, nil)
	opts := testOptions(t, true, proc.PID) // Fix: true
	stubDaemonProcessArgv(t, func(int) []string { return []string{"/tmp/build/af", "--daemon"} })
	stubProcessHomes(t, map[int]string{proc.PID: t.TempDir()})
	opts.minLeakedDaemonAge = time.Hour // the production window; the fixture is seconds old
	stubDaemonExe(t, map[int]daemonBinary{proc.PID: tempBinaryDaemon(opts.TempDir)})

	report, err := Run(opts)
	require.NoError(t, err)

	findings := findByCheck(report, checkLeakedDaemon)
	require.Len(t, findings, 1)
	require.False(t, findings[0].Actionable)
	require.Empty(t, findings[0].FixAction)
	require.Contains(t, findings[0].Detail, "test run that is still going")
	require.True(t, proctree.AliveSame(proc), "a daemon a live test run may own must survive --fix")
}

func TestAnUnreadableDaemonBinaryIsReportedRatherThanAssumedInstalled(t *testing.T) {
	proc := spawnWithEnv(t, "opaque-daemon", nil, nil)
	opts := leakedDaemonOptions(t, false, proc, t.TempDir())
	stubDaemonExe(t, map[int]daemonBinary{}) // every pid unknown

	report, err := Run(opts)
	require.NoError(t, err)

	findings := findByCheck(report, checkLeakedDaemon)
	require.Len(t, findings, 1)
	require.False(t, findings[0].Actionable, "an unreadable binary path is an unknown, not an unhealthy machine")
	require.Contains(t, findings[0].Detail, "cannot be read")
}

func TestFixStopsALeakedTempBinaryDaemon(t *testing.T) {
	proc := spawnWithEnv(t, "leaky-daemon-to-kill", nil, nil)
	opts := leakedDaemonOptions(t, true, proc, t.TempDir()) // Fix: true
	stubDaemonExe(t, map[int]daemonBinary{proc.PID: tempBinaryDaemon(opts.TempDir)})

	report, err := Run(opts)
	require.NoError(t, err)

	findings := findByCheck(report, checkLeakedDaemon)
	require.Len(t, findings, 1)
	require.True(t, findings[0].Fixed, "fix error: %v", findings[0].FixErr)
	require.Eventually(t, func() bool { return !proctree.AliveSame(proc) },
		5*time.Second, 20*time.Millisecond, "the leaked daemon should be gone")
}

// ---- dead-socket homes ----

// deadSocketDir builds the residue shape exactly: a directory whose ENTIRE
// content is one abandoned daemon-http.sock, aged past the sweep's window.
func deadSocketDir(t *testing.T, tempRoot, name string) string {
	t.Helper()
	dir := filepath.Join(tempRoot, name)
	require.NoError(t, os.MkdirAll(dir, 0755))
	socket := filepath.Join(dir, daemon.HTTPSocketName())
	l, err := net.Listen("unix", socket)
	require.NoError(t, err)
	l.(*net.UnixListener).SetUnlinkOnClose(false) // leave the debris behind
	require.NoError(t, l.Close())
	old := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(socket, old, old))
	require.NoError(t, os.Chtimes(dir, old, old))
	return dir
}

func TestADirectoryHoldingOnlyADeadDaemonSocketIsReported(t *testing.T) {
	tempRoot := socketTempHome(t)
	dir := deadSocketDir(t, tempRoot, "af-1450838862")

	report, err := Run(macLikeTempHomeOptions(t, tempRoot, false))
	require.NoError(t, err)

	findings := findByCheck(report, checkDeadSocketHome)
	require.Len(t, findings, 1)
	require.True(t, findings[0].Actionable)
	require.NotEmpty(t, findings[0].FixAction)
	require.Contains(t, findings[0].Detail, dir)
	require.DirExists(t, dir, "a report-only run removes nothing")
}

func TestFixRemovesADeadSocketDirectory(t *testing.T) {
	tempRoot := socketTempHome(t)
	dir := deadSocketDir(t, tempRoot, "af-1450838862")

	report, err := Run(macLikeTempHomeOptions(t, tempRoot, true)) // Fix: true
	require.NoError(t, err)

	findings := findByCheck(report, checkDeadSocketHome)
	require.Len(t, findings, 1)
	require.True(t, findings[0].Fixed, "fix error: %v", findings[0].FixErr)
	require.NoDirExists(t, dir)
}

// A directory with anything else in it is a temp HOME, and removing one of those
// can destroy a session's state — checkStaleTempHomes owns it, on a proof from
// the kernel. This check must not reach for it on the much weaker evidence it
// has.
func TestADirectoryWithMoreThanTheSocketIsLeftToTheTempHomeSweep(t *testing.T) {
	tempRoot := socketTempHome(t)
	dir := deadSocketDir(t, tempRoot, "af-with-state")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "instances.json"), []byte("[]"), 0644))
	// The witness: a directory in the same sweep that IS this shape, so "no
	// finding for the one holding state" cannot be a check that never ran.
	debris := deadSocketDir(t, tempRoot, "af-debris")

	report, err := Run(macLikeTempHomeOptions(t, tempRoot, true)) // Fix: true
	require.NoError(t, err)

	findings := findByCheck(report, checkDeadSocketHome)
	require.Len(t, findings, 1)
	require.Contains(t, findings[0].Detail, debris)
	require.NoDirExists(t, debris)
	require.DirExists(t, dir)
	require.FileExists(t, filepath.Join(dir, "instances.json"),
		"the dead-socket path must never touch a directory holding state")
}

func TestALiveSocketIsNeverCalledDebris(t *testing.T) {
	tempRoot := socketTempHome(t)
	dir := filepath.Join(tempRoot, "af-live")
	require.NoError(t, os.MkdirAll(dir, 0755))
	socket := filepath.Join(dir, daemon.HTTPSocketName())
	l, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	old := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(socket, old, old))
	debris := deadSocketDir(t, tempRoot, "af-debris") // the witness; see above

	report, err := Run(macLikeTempHomeOptions(t, tempRoot, true)) // Fix: true
	require.NoError(t, err)

	findings := findByCheck(report, checkDeadSocketHome)
	require.Len(t, findings, 1, "only the dead one; something is accepting on the live socket")
	require.Contains(t, findings[0].Detail, debris)
	require.DirExists(t, dir, "a live daemon's directory must survive --fix")
	require.NoDirExists(t, debris)
}

// Undetermined is not No. A dial that did not complete says nothing about
// whether anything is serving the socket, and this package never lets an unknown
// authorise a removal (#1989, daemon.ProbeAnswer).
func TestAnUndeterminedSocketProbeReportsWithoutRemoving(t *testing.T) {
	tempRoot := socketTempHome(t)
	dir := deadSocketDir(t, tempRoot, "af-unknown")
	stubDeadSocketProbe(t, func(string) daemon.ProbeAnswer {
		return daemon.Undetermined(os.ErrDeadlineExceeded)
	})

	report, err := Run(macLikeTempHomeOptions(t, tempRoot, true)) // Fix: true
	require.NoError(t, err)

	findings := findByCheck(report, checkDeadSocketHome)
	require.Len(t, findings, 1)
	require.False(t, findings[0].Actionable)
	require.Empty(t, findings[0].FixAction, "an unprovable socket must never carry a removal")
	require.DirExists(t, dir)
}

func TestADeadSocketDirectoryALiveProcessCallsHomeIsSpared(t *testing.T) {
	tempRoot := socketTempHome(t)
	dir := deadSocketDir(t, tempRoot, "af-claimed")
	proc := spawnWithEnv(t, "claimant", nil, map[string]string{"AGENT_FACTORY_HOME": dir})

	opts := macLikeTempHomeOptions(t, tempRoot, true) // Fix: true
	opts.snapshot = snapshotOf(t, proc.PID)

	report, err := Run(opts)
	require.NoError(t, err)

	require.Empty(t, findByCheck(report, checkDeadSocketHome))
	require.DirExists(t, dir, "a directory a live process names as its home must never be removed")
	require.True(t, okContains(report, "is in use"))
}

func TestADeadSocketDirectoryALiveProcessWorksInsideIsSpared(t *testing.T) {
	tempRoot := socketTempHome(t)
	dir := deadSocketDir(t, tempRoot, "af-occupied")
	proc := spawnWithEnv(t, "occupant", nil, nil)
	stubProcessCwd(t, map[int]string{proc.PID: dir})

	opts := macLikeTempHomeOptions(t, tempRoot, true) // Fix: true
	opts.snapshot = snapshotOf(t, proc.PID)

	report, err := Run(opts)
	require.NoError(t, err)

	require.Empty(t, findByCheck(report, checkDeadSocketHome))
	require.DirExists(t, dir)
	require.True(t, okContains(report, "is working inside it"),
		"the check must have looked and SAID it spared this one, not merely produced no row")
}

// The teeth of the removal, and the reason this path needs no lock proof: it
// re-checks the shape and then calls os.Remove, so a directory that gained a
// file between detection and the fix FAILS instead of being swept away with it.
func TestTheRemovalRefusesADirectoryThatGainedAFile(t *testing.T) {
	tempRoot := socketTempHome(t)
	dir := deadSocketDir(t, tempRoot, "af-raced")
	ctx, err := newScanContext(macLikeTempHomeOptions(t, tempRoot, true))
	require.NoError(t, err)
	fix := deadSocketHomeRemoveFix(ctx, dir, tempRoot, ctx.opts.ConfigDir)

	precious := filepath.Join(dir, "someone-elses-work")
	require.NoError(t, os.WriteFile(precious, []byte("do not delete me"), 0644))

	require.Error(t, fix(), "a directory that is no longer only a dead socket must not be removed")
	require.FileExists(t, precious)
	require.DirExists(t, dir)
}

// The fix pass takes ONE tmux recheck, not one per removal. On the maintainer's
// box this check finds 626 removable directories, and the sibling temp-home fix's
// once-per-removal shape would be ~16 shell-outs each — ~10,000 calls at the tmux
// server every live agent session depends on.
//
// The count is asserted as "does not grow with the number of removals", which is
// the property, rather than as a magic number that would break whenever the tmux
// listing gains a call.
func TestTheFixPassCountsTmuxOnceHoweverManyDirectoriesItRemoves(t *testing.T) {
	run := func(dirs int) (removals, tmuxCalls int) {
		tempRoot := socketTempHome(t)
		var made []string
		for i := 0; i < dirs; i++ {
			made = append(made, deadSocketDir(t, tempRoot, fmt.Sprintf("af-debris-%d", i)))
		}
		opts := macLikeTempHomeOptions(t, tempRoot, true) // Fix: true
		calls := 0
		opts.Exec = cmd_test.MockCmdExec{
			RunFunc: func(*exec.Cmd) error { return nil },
			OutputFunc: func(c *exec.Cmd) ([]byte, error) {
				calls++
				return []byte(""), nil
			},
		}
		report, err := Run(opts)
		require.NoError(t, err)
		for _, f := range findByCheck(report, checkDeadSocketHome) {
			require.True(t, f.Fixed, "fix error: %v", f.FixErr)
			removals++
		}
		for _, d := range made {
			require.NoDirExists(t, d)
		}
		return removals, calls
	}

	oneRemoval, oneCalls := run(1)
	require.Equal(t, 1, oneRemoval)
	manyRemovals, manyCalls := run(8)
	require.Equal(t, 8, manyRemovals)
	require.Equal(t, oneCalls, manyCalls,
		"the tmux recheck must be taken once per fix PASS; eight times the removals took %d calls against %d",
		manyCalls, oneCalls)
}

func TestTheRemovalRefusesADirectoryOutsideTheTempDir(t *testing.T) {
	tempRoot := socketTempHome(t)
	outside := socketTempHome(t)
	dir := deadSocketDir(t, outside, "af-elsewhere")
	ctx, err := newScanContext(macLikeTempHomeOptions(t, tempRoot, true))
	require.NoError(t, err)

	require.Error(t, deadSocketHomeRemoveFix(ctx, dir, tempRoot, ctx.opts.ConfigDir)())
	require.DirExists(t, dir)
}

// ---- the probes themselves ----

func TestProbeDeadSocketAnswersNoOnlyForARefusedDial(t *testing.T) {
	// socketTempHome, not t.TempDir: a real bind here, and t.TempDir embeds the
	// test's name, which overruns sun_path.
	dir := socketTempHome(t)

	live := filepath.Join(dir, "live.sock")
	l, err := net.Listen("unix", live)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	require.Equal(t, "yes", probeDeadSocket(live).String(), "a socket with a listener is in use")

	abandoned := filepath.Join(dir, "abandoned.sock")
	dead, err := net.Listen("unix", abandoned)
	require.NoError(t, err)
	dead.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, dead.Close())
	require.Equal(t, "no", probeDeadSocket(abandoned).String(), "a refused dial is a positive no")

	require.Equal(t, "no", probeDeadSocket(filepath.Join(dir, "gone.sock")).String(),
		"a socket that is not there answers no as definitely as a refused one")
}

func TestReadDaemonProcessExeStripsTheDeletedSuffix(t *testing.T) {
	if !procExePathsReadable() {
		t.Skip("/proc/<pid>/exe is Linux's; the platform branch is covered by TestAPlatformWithNoReadableBinaryPathSaysSoOnce")
	}
	bin := readDaemonProcessExe(os.Getpid())
	require.True(t, bin.known, "this test binary's own exe link must be readable")
	require.NotContains(t, bin.path, deletedExeSuffix)

	require.False(t, readDaemonProcessExe(-1).known,
		"an unreadable exe link must report unknown, never an empty installed path")
}

// holdsOnlyADaemonSocket is what makes the removal safe, so a name is never
// enough — a regular file that borrowed the socket's name must not qualify.
func TestOnlyARealSocketCountsAsADeadSocketDirectory(t *testing.T) {
	dir := t.TempDir()
	require.False(t, holdsOnlyADaemonSocket(dir), "an empty directory is not this shape")

	require.NoError(t, os.WriteFile(filepath.Join(dir, daemon.HTTPSocketName()), nil, 0644))
	require.False(t, holdsOnlyADaemonSocket(dir),
		"a regular file wearing the socket's name is not a socket")

	require.False(t, holdsOnlyADaemonSocket(filepath.Join(dir, "does-not-exist")),
		"a directory that cannot be listed is never this shape")
}
