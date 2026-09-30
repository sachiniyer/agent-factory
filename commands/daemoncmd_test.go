package commands

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
)

const testUpgradeDaemonPath = "/tmp/af-upgraded"

func stubAutostartScope(t *testing.T, serves, installed bool, gateErr error) {
	t.Helper()
	prevServes := autostartUnitServesHomeFn
	prevConfigDir := configDirFn
	t.Cleanup(func() {
		autostartUnitServesHomeFn = prevServes
		configDirFn = prevConfigDir
	})
	configDirFn = func() (string, error) { return "/tmp/af-test-home", nil }
	autostartUnitServesHomeFn = func(string) (bool, bool, error) {
		return serves, installed, gateErr
	}
}

// Tests for the unit-aware upgrade respawn (#796) and the unconditional
// fallback (#813). All collaborators are stubbed so nothing here
// touches the real systemctl/launchctl or spawns a daemon process — a real
// supervised daemon may be running on the machine executing these tests.

// stubRespawnCollaborators replaces the autostart-detection, home-gate,
// unit-restart, ad-hoc-spawn, and shutdown-wait hooks used by
// respawnDaemonAfterUpgrade, restoring them on cleanup. The shutdown wait is
// stubbed to an immediate nil so no test here pings the host's control socket —
// a real supervised daemon answering it would stall the wait for its full
// grace. The home gate and unit-path reader are stubbed for the same reason:
// unstubbed they read the REAL host's autostart unit and config dir. It
// returns counters for the restart and ad-hoc paths.
//
// The stubbed unit serves THIS home and launches the very binary the upgrade
// wrote, so tests using this helper keep asserting what they always asserted:
// the unit-vs-ad-hoc branch. The cross-home gate (#1950) and the stale-binary
// check (#1947) are exercised by their own tests, which override these.
func stubRespawnCollaborators(t *testing.T, installed bool, restartErr error) (restartCalls, ensureCalls *int) {
	t.Helper()
	prevInstalled := autostartInstalledFn
	prevRestart := restartAutostartUnitFn
	prevEnsure := ensureDaemonFromPathFn
	prevWait := waitForShutdownCompletionFn
	prevServes := autostartUnitServesHomeFn
	prevUnitExec := autostartUnitExecPathFn
	prevConfigDir := configDirFn
	t.Cleanup(func() {
		autostartInstalledFn = prevInstalled
		restartAutostartUnitFn = prevRestart
		ensureDaemonFromPathFn = prevEnsure
		waitForShutdownCompletionFn = prevWait
		autostartUnitServesHomeFn = prevServes
		autostartUnitExecPathFn = prevUnitExec
		configDirFn = prevConfigDir
	})
	restartCalls = new(int)
	ensureCalls = new(int)
	autostartInstalledFn = func() bool { return installed }
	autostartUnitServesHomeFn = func(string) (serves bool, isInstalled bool, err error) {
		return installed, installed, nil
	}
	autostartUnitExecPathFn = func() (string, bool, error) { return testUpgradeDaemonPath, installed, nil }
	configDirFn = func() (string, error) { return "/tmp/af-test-home", nil }
	restartAutostartUnitFn = func() error {
		*restartCalls++
		return restartErr
	}
	ensureDaemonFromPathFn = func(string) error {
		*ensureCalls++
		return nil
	}
	waitForShutdownCompletionFn = func(int) error { return nil }
	return restartCalls, ensureCalls
}

// TestRespawnAfterUpgradeRestartsInstalledUnit pins the #796 fix: when the
// autostart unit is installed, the post-upgrade respawn must go through the
// service manager so the daemon stays supervised, and must NOT spawn an
// ad-hoc child.
func TestRespawnAfterUpgradeRestartsInstalledUnit(t *testing.T) {
	restartCalls, ensureCalls := stubRespawnCollaborators(t, true, nil)

	if _, err := respawnDaemonAfterUpgrade(testUpgradeDaemonPath, 0); err != nil {
		t.Fatalf("respawnDaemonAfterUpgrade: %v", err)
	}

	if *restartCalls != 1 {
		t.Fatalf("unit restarts = %d, want 1", *restartCalls)
	}
	if *ensureCalls != 0 {
		t.Fatalf("ad-hoc spawns = %d, want 0 (unit restart must not be demoted to an ad-hoc daemon)", *ensureCalls)
	}
}

// TestRespawnAfterUpgradeWithoutUnitSpawnsAdHoc: installs without an
// autostart unit fall back to an ad-hoc daemon spawn.
func TestRespawnAfterUpgradeWithoutUnitSpawnsAdHoc(t *testing.T) {
	restartCalls, ensureCalls := stubRespawnCollaborators(t, false, nil)

	if _, err := respawnDaemonAfterUpgrade(testUpgradeDaemonPath, 0); err != nil {
		t.Fatalf("respawnDaemonAfterUpgrade: %v", err)
	}

	if *restartCalls != 0 {
		t.Fatalf("unit restarts = %d, want 0 when no unit is installed", *restartCalls)
	}
	if *ensureCalls != 1 {
		t.Fatalf("ad-hoc spawns = %d, want 1", *ensureCalls)
	}
}

// TestRespawnAfterUpgradeFallsBackWhenRestartFails: a failing
// systemctl/launchctl invocation must not leave task schedules dark — the
// respawn falls back to the ad-hoc spawn.
func TestRespawnAfterUpgradeFallsBackWhenRestartFails(t *testing.T) {
	restartCalls, ensureCalls := stubRespawnCollaborators(t, true, errors.New("systemctl exited 1"))

	if _, err := respawnDaemonAfterUpgrade(testUpgradeDaemonPath, 0); err != nil {
		t.Fatalf("respawnDaemonAfterUpgrade: %v", err)
	}

	if *restartCalls != 1 {
		t.Fatalf("unit restarts = %d, want 1", *restartCalls)
	}
	if *ensureCalls != 1 {
		t.Fatalf("ad-hoc spawns = %d, want 1 (fallback after a failed unit restart)", *ensureCalls)
	}
}

// TestRespawnAfterUpgradeSpawnsWithZeroEnabledTasks pins the #813 fix: the
// post-upgrade fallback must respawn unconditionally, not only when an
// enabled task exists. Callers only invoke the respawn after stopping a
// running daemon, and that daemon may have been serving the web UI alone.
// AGENT_FACTORY_HOME points at an empty temp dir so the task store is
// guaranteed empty — if the enabled-task gate ever creeps back into the
// respawn path, this test fails.
func TestRespawnAfterUpgradeSpawnsWithZeroEnabledTasks(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_, ensureCalls := stubRespawnCollaborators(t, false, nil)

	if _, err := respawnDaemonAfterUpgrade(testUpgradeDaemonPath, 0); err != nil {
		t.Fatalf("respawnDaemonAfterUpgrade: %v", err)
	}

	if *ensureCalls != 1 {
		t.Fatalf("ad-hoc spawns = %d, want 1 even with zero enabled tasks (the stopped daemon must be restored, #813)", *ensureCalls)
	}
}

func TestRespawnAfterUpgradeSpawnsAdHocFromProvidedPath(t *testing.T) {
	stubRespawnCollaborators(t, false, nil)
	var gotPath string
	ensureDaemonFromPathFn = func(path string) error {
		gotPath = path
		return nil
	}

	if _, err := respawnDaemonAfterUpgrade("/opt/af/new", 0); err != nil {
		t.Fatalf("respawnDaemonAfterUpgrade: %v", err)
	}

	if gotPath != "/opt/af/new" {
		t.Fatalf("ad-hoc respawn path = %q, want /opt/af/new", gotPath)
	}
}

// TestRespawnAfterUpgradeWaitsForShutdownFirst pins the #854 fix: the Shutdown
// RPC acks before the old daemon tears down, so the respawn must wait for the
// control socket to die before EITHER respawn branch runs — otherwise the new
// daemon (ad-hoc EnsureDaemon ping or the unit-restarted daemon's startup ping
// guard) sees the dying daemon as alive, skips the spawn, and nothing is left
// running once it exits. Both branches are exercised.
func TestRespawnAfterUpgradeWaitsForShutdownFirst(t *testing.T) {
	for _, tc := range []struct {
		name      string
		installed bool
		wantStep  string
	}{
		{name: "ad-hoc branch", installed: false, wantStep: "ensure"},
		{name: "unit branch", installed: true, wantStep: "restart"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubRespawnCollaborators(t, tc.installed, nil)
			var seq []string
			gotPID := -1
			waitForShutdownCompletionFn = func(pid int) error {
				seq = append(seq, "wait")
				gotPID = pid
				return nil
			}
			prevRestart, prevEnsure := restartAutostartUnitFn, ensureDaemonFromPathFn
			restartAutostartUnitFn = func() error {
				seq = append(seq, "restart")
				return prevRestart()
			}
			ensureDaemonFromPathFn = func(path string) error {
				seq = append(seq, "ensure")
				return prevEnsure(path)
			}

			if _, err := respawnDaemonAfterUpgrade(testUpgradeDaemonPath, 4242); err != nil {
				t.Fatalf("respawnDaemonAfterUpgrade: %v", err)
			}

			if len(seq) != 2 || seq[0] != "wait" || seq[1] != tc.wantStep {
				t.Fatalf("call sequence = %v, want [wait %s]", seq, tc.wantStep)
			}
			// The wait must watch the stopped daemon's process, not merely its
			// socket (#5007): the pid handed to the respawn reaches the wait.
			if gotPID != 4242 {
				t.Fatalf("shutdown wait got pid %d, want 4242 (the stopped daemon's pid)", gotPID)
			}
		})
	}
}

// TestRespawnAfterUpgradeDoesNotRespawnBesideUnfinishedShutdown pins the
// reversal of the old warn-and-respawn contract (#5007): a wait that reports
// the old daemon still running at its bound means it is usually still joining
// durable work with its control socket already closed. A respawn then would
// lose the per-home lock to it and exit, leaving nothing once the old daemon
// finishes — so the respawn must return the error and start nothing, on
// either branch.
func TestRespawnAfterUpgradeDoesNotRespawnBesideUnfinishedShutdown(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(fmt.Sprintf("unit installed=%v", installed), func(t *testing.T) {
			restartCalls, ensureCalls := stubRespawnCollaborators(t, installed, nil)
			waitErr := errors.New("daemon pid 4242 still running 60s after shutdown was acknowledged")
			waitForShutdownCompletionFn = func(int) error { return waitErr }

			_, err := respawnDaemonAfterUpgrade(testUpgradeDaemonPath, 4242)
			if !errors.Is(err, waitErr) {
				t.Fatalf("respawnDaemonAfterUpgrade error = %v, want it to wrap the wait error", err)
			}
			if *restartCalls != 0 || *ensureCalls != 0 {
				t.Fatalf("unit restarts = %d, ad-hoc spawns = %d; want 0 and 0 while the old daemon is still shutting down", *restartCalls, *ensureCalls)
			}
		})
	}
}

// TestRestartDaemonFromPathThreadsStoppedDaemonPID pins #5007's handle: the
// pid RequestShutdown reports for the daemon it stopped must reach both the
// respawn and, through it, the shutdown wait — so the respawn waits on that
// process exiting rather than on a control socket that outlives the ack.
func TestRestartDaemonFromPathThreadsStoppedDaemonPID(t *testing.T) {
	stubRespawnCollaborators(t, false, nil)
	prevShutdown := requestDaemonShutdownFn
	prevRespawn := respawnDaemonFn
	t.Cleanup(func() {
		requestDaemonShutdownFn = prevShutdown
		respawnDaemonFn = prevRespawn
	})
	requestDaemonShutdownFn = func() (daemon.ShutdownResult, int, error) {
		return daemon.ShutdownViaRPC, 4242, nil
	}
	var seq []string
	waitPID, respawnPID, respawnPath := -1, -1, ""
	waitForShutdownCompletionFn = func(pid int) error {
		seq = append(seq, "wait")
		waitPID = pid
		return nil
	}
	prevEnsure := ensureDaemonFromPathFn
	ensureDaemonFromPathFn = func(path string) error {
		seq = append(seq, "ensure")
		return prevEnsure(path)
	}
	respawnDaemonFn = func(path string, pid int) (respawnResult, error) {
		respawnPath, respawnPID = path, pid
		return respawnDaemonAfterUpgrade(path, pid)
	}

	outcome, err := restartDaemonFromPathDetailed("/opt/af/current")
	if err != nil {
		t.Fatalf("restartDaemonFromPathDetailed: %v", err)
	}
	if !outcome.Respawned {
		t.Fatalf("outcome.Respawned = false, want true")
	}
	if respawnPath != "/opt/af/current" || respawnPID != 4242 {
		t.Fatalf("respawn got (%q, %d), want (%q, 4242)", respawnPath, respawnPID, "/opt/af/current")
	}
	if waitPID != 4242 {
		t.Fatalf("shutdown wait got pid %d, want 4242", waitPID)
	}
	if len(seq) != 2 || seq[0] != "wait" || seq[1] != "ensure" {
		t.Fatalf("call sequence = %v, want [wait ensure]", seq)
	}
}

func TestRestartDaemonFromPathNoDaemonIsNoOp(t *testing.T) {
	prevShutdown := requestDaemonShutdownFn
	prevRespawn := respawnDaemonFn
	t.Cleanup(func() {
		requestDaemonShutdownFn = prevShutdown
		respawnDaemonFn = prevRespawn
	})
	requestDaemonShutdownFn = func() (daemon.ShutdownResult, int, error) {
		return daemon.ShutdownNoDaemon, 0, nil
	}
	respawnDaemonFn = func(string, int) (respawnResult, error) {
		t.Fatalf("respawn must not run when no daemon is present")
		return respawnResult{}, nil
	}

	result, err := restartDaemonFromPath(testUpgradeDaemonPath)
	if err != nil {
		t.Fatalf("restartDaemonFromPath: %v", err)
	}
	if result != daemon.ShutdownNoDaemon {
		t.Fatalf("restart result = %v, want ShutdownNoDaemon", result)
	}
}

func TestRestartDaemonFromPathRespawnsStoppedDaemon(t *testing.T) {
	prevShutdown := requestDaemonShutdownFn
	prevRespawn := respawnDaemonFn
	t.Cleanup(func() {
		requestDaemonShutdownFn = prevShutdown
		respawnDaemonFn = prevRespawn
	})
	requestDaemonShutdownFn = func() (daemon.ShutdownResult, int, error) {
		return daemon.ShutdownViaRPC, 0, nil
	}
	var gotPath string
	respawnDaemonFn = func(path string, _ int) (respawnResult, error) {
		gotPath = path
		return respawnResult{}, nil
	}

	result, err := restartDaemonFromPath("/opt/af/current")
	if err != nil {
		t.Fatalf("restartDaemonFromPath: %v", err)
	}
	if result != daemon.ShutdownViaRPC {
		t.Fatalf("restart result = %v, want ShutdownViaRPC", result)
	}
	if gotPath != "/opt/af/current" {
		t.Fatalf("respawn path = %q, want /opt/af/current", gotPath)
	}
}

// The command promises an idempotent no-op when no daemon is running. That
// answer must be established before reading, rewriting, or reloading an
// installed unit: a broken stale unit is irrelevant when nothing will stop.
func TestRunDaemonRestartNoDaemonSkipsUnsafeUnitRefresh(t *testing.T) {
	prevPresence := daemonRestartPresenceFn
	prevRefresh := refreshAutostartUnitFn
	prevExecutable := osExecutableFn
	prevConfigDir := configDirFn
	prevQuiet := daemonRestartQuiet
	t.Cleanup(func() {
		daemonRestartPresenceFn = prevPresence
		refreshAutostartUnitFn = prevRefresh
		osExecutableFn = prevExecutable
		configDirFn = prevConfigDir
		daemonRestartQuiet = prevQuiet
	})

	daemonRestartPresenceFn = func() daemon.ProbeAnswer { return daemon.AnswerNo() }
	refreshAutostartUnitFn = func() error {
		t.Fatal("no-daemon restart touched the stale autostart unit")
		return errors.New("daemon-reload failed")
	}
	osExecutableFn = func() (string, error) {
		t.Fatal("no-daemon restart resolved an executable despite being a no-op")
		return "", nil
	}
	configDirFn = func() (string, error) {
		t.Fatal("no-daemon restart tried to scope an irrelevant autostart unit")
		return "", nil
	}
	daemonRestartQuiet = false

	var out, errOut bytes.Buffer
	if err := runDaemonRestart(&out, &errOut); err != nil {
		t.Fatalf("runDaemonRestart: %v", err)
	}
	if got := out.String(); got != "no running daemon to restart\n" {
		t.Fatalf("restart output = %q, want documented no-op", got)
	}
	// The no-daemon branch is a clean no-op; nothing was demoted, so stderr
	// must stay quiet just like the upgrade path's "no daemon" case does.
	if errOut.Len() != 0 {
		t.Fatalf("no-daemon restart must not warn on stderr.\ngot stderr=%q", errOut.String())
	}
}

// daemonRestartPresentHarness stands up the seams runDaemonRestart touches once
// it has decided a daemon is present: the presence probe answers yes, the
// executable resolves to a real temp file (runDaemonRestart EvalSymlinks it), no
// autostart unit serves this home so refreshAutostartUnitForCurrentHome no-ops,
// and restartDaemonFromPathDetailed reports the given shutdown/respawn. The
// respawn is stubbed directly so a test injects a UnitErr demotion without going
// through respawnDaemonAfterUpgrade (whose own demotion path has its own tests).
// It returns buffers the caller drives runDaemonRestart against.
func daemonRestartPresentHarness(t *testing.T, shutdown daemon.ShutdownResult, respawn respawnResult) (*bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	binPath := tempBinPath(t)
	if err := os.WriteFile(binPath, []byte("binary"), 0o755); err != nil {
		t.Fatalf("seed binary: %v", err)
	}

	prevPresence := daemonRestartPresenceFn
	prevExecutable := osExecutableFn
	prevShutdown := requestDaemonShutdownFn
	prevRespawn := respawnDaemonFn
	prevQuiet := daemonRestartQuiet
	t.Cleanup(func() {
		daemonRestartPresenceFn = prevPresence
		osExecutableFn = prevExecutable
		requestDaemonShutdownFn = prevShutdown
		respawnDaemonFn = prevRespawn
		daemonRestartQuiet = prevQuiet
	})
	daemonRestartPresenceFn = func() daemon.ProbeAnswer { return daemon.AnswerYes() }
	osExecutableFn = func() (string, error) { return binPath, nil }
	requestDaemonShutdownFn = func() (daemon.ShutdownResult, int, error) { return shutdown, 0, nil }
	respawnDaemonFn = func(string, int) (respawnResult, error) { return respawn, nil }
	stubAutostartScope(t, false, false, nil) // no unit serves this home -> refresh no-ops
	daemonRestartQuiet = false

	return new(bytes.Buffer), new(bytes.Buffer)
}

// TestRunDaemonRestart_FailedUnitRestartIsLoud is the repro for the silent
// demotion under `af daemon restart`. When the respawn falls back to an ad-hoc
// daemon after the autostart unit's restart fails, the daemon is up but
// unsupervised: it dies with the session and will not return at next login.
// runDaemonRestart used to call the discarding restartDaemonFromPath wrapper and
// print a bare "daemon restarted" over the demotion — the exact anti-pattern
// respawnDaemonAfterUpgrade's contract names as "half of #1947", and the one
// TestUpgrade_FailedUnitRestartIsLoud already fixed for `af upgrade`. The fix
// routes it to stderr with the repair, mirroring reportUpgradeRestart.
func TestRunDaemonRestart_FailedUnitRestartIsLoud(t *testing.T) {
	unitErr := errors.New("systemctl --user restart failed: exit 1")
	out, errOut := daemonRestartPresentHarness(t, daemon.ShutdownViaRPC, respawnResult{UnitErr: unitErr})

	if err := runDaemonRestart(out, errOut); err != nil {
		t.Fatalf("runDaemonRestart: %v", err)
	}

	// The daemon IS running (just unsupervised), so the success line stands and
	// is accurate; the warning qualifies it rather than replacing it.
	if got := out.String(); got != "daemon restarted\n" {
		t.Fatalf("stdout = %q, want the success line over the demotion", got)
	}
	if errOut.Len() == 0 {
		t.Fatalf("a failed unit restart must reach the user, not just the log; stderr was empty.\nstdout=%q", out.String())
	}
	for _, want := range []string{
		"systemctl --user restart failed: exit 1",        // names the failed unit restart
		"ad-hoc process instead",                         // names the unsupervised fallback
		"unsupervised and will not return at next login", // names the supervision loss
		"af daemon install",                              // names the repair
	} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatalf("stderr missing %q.\ngot=%q", want, errOut.String())
		}
	}
	// af daemon restart wrote no new binary, so the wording must not import the
	// upgrade-specific "new binary" claim that reportUpgradeRestart is allowed to
	// make.
	if strings.Contains(errOut.String(), "new binary") {
		t.Fatalf("daemon restart never wrote a new binary; stderr must not claim one.\ngot=%q", errOut.String())
	}
}

// TestRunDaemonRestart_GatedUnitRestartIsLoud covers the OTHER demotion the
// UnitErr test does not reach: when the post-shutdown ownership check cannot
// determine whether the installed unit serves this home (for example, if the
// unit becomes unreadable between the earlier refresh check and
// unitRestartTarget), respawn conservatively starts an ad-hoc daemon and returns
// UnitGateErr with no UnitErr. That branch loses login/reboot supervision just
// the same, yet this command used to print only the success line over it.
// runDaemonRestart now mirrors reportUpgradeRestart and routes the cause and
// reinstall remedy to stderr.
func TestRunDaemonRestart_GatedUnitRestartIsLoud(t *testing.T) {
	gateErr := errors.New("autostart unit unreadable: permission denied")
	out, errOut := daemonRestartPresentHarness(t, daemon.ShutdownViaRPC, respawnResult{UnitGateErr: gateErr})

	if err := runDaemonRestart(out, errOut); err != nil {
		t.Fatalf("runDaemonRestart: %v", err)
	}

	// The daemon IS running (just unsupervised), so the success line stands and
	// is accurate; the warning qualifies it rather than replacing it.
	if got := out.String(); got != "daemon restarted\n" {
		t.Fatalf("stdout = %q, want the success line over the demotion", got)
	}
	if errOut.Len() == 0 {
		t.Fatalf("a gated unit restart must reach the user, not just the log; stderr was empty.\nstdout=%q", out.String())
	}
	for _, want := range []string{
		"autostart unit unreadable: permission denied", // names the gate failure
		"was left alone",                // names that the unit was not touched
		"unsupervised ad-hoc process",   // names the unsupervised fallback
		"will not return at next login", // names the supervision loss
		"af daemon install",             // names the repair
	} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatalf("stderr missing %q.\ngot=%q", want, errOut.String())
		}
	}
	// A gated restart attempted no unit restart, so the wording must not claim
	// one failed (that is the UnitErr branch's message), and af daemon restart
	// wrote no new binary, so it must not import the upgrade-specific "new
	// binary" claim either.
	if strings.Contains(errOut.String(), "could not be restarted") {
		t.Fatalf("gated restart did not attempt a unit restart; stderr must not claim one failed.\ngot=%q", errOut.String())
	}
	if strings.Contains(errOut.String(), "new binary") {
		t.Fatalf("daemon restart never wrote a new binary; stderr must not claim one.\ngot=%q", errOut.String())
	}
}

// TestRunDaemonRestart_CleanRestartIsQuiet locks the happy path against the
// warning above becoming noise: a supervised (or genuinely ad-hoc) respawn with
// no demotion must stay quiet on stderr and print the plain success line. The
// zero respawnResult is the good outcome the vast majority of real restarts
// produce, which is why this bug went undetected for so long.
func TestRunDaemonRestart_CleanRestartIsQuiet(t *testing.T) {
	out, errOut := daemonRestartPresentHarness(t, daemon.ShutdownViaRPC, respawnResult{})

	if err := runDaemonRestart(out, errOut); err != nil {
		t.Fatalf("runDaemonRestart: %v", err)
	}
	if got := out.String(); got != "daemon restarted\n" {
		t.Fatalf("stdout = %q, want the plain success line", got)
	}
	if errOut.Len() != 0 {
		t.Fatalf("a clean restart must stay quiet on stderr.\ngot stderr=%q", errOut.String())
	}
}

// TestRunDaemonRestart_FailedUnitRestartIsLoudWithSIGTERM covers the SIGTERM
// arm of the success switch together with the demotion: a pre-fix daemon (no
// Shutdown RPC) stopped via the SIGTERM fallback that then demotes to ad-hoc
// must still surface the lost supervision, not paper it with the SIGTERM
// success line.
func TestRunDaemonRestart_FailedUnitRestartIsLoudWithSIGTERM(t *testing.T) {
	unitErr := errors.New("launchctl bootstrap failed: exit 1")
	out, errOut := daemonRestartPresentHarness(t, daemon.ShutdownViaSIGTERM, respawnResult{UnitErr: unitErr})

	if err := runDaemonRestart(out, errOut); err != nil {
		t.Fatalf("runDaemonRestart: %v", err)
	}
	if !strings.Contains(out.String(), "SIGTERM fallback") {
		t.Fatalf("stdout = %q, want the SIGTERM-specific success line", out.String())
	}
	for _, want := range []string{"launchctl bootstrap failed: exit 1", "unsupervised", "af daemon install"} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatalf("stderr missing %q (SIGTERM stop must not swallow the demotion).\ngot=%q", want, errOut.String())
		}
	}
}
