package commands

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
)

// Tests for the post-upgrade daemon restart actually LANDING, and for never
// reporting success when it did not (#1947), plus the cross-home unit gate
// (#1950).
//
// `af upgrade` has restarted the daemon since #498/#1386 — that is not what
// was broken. What was broken is that the restart could run, fail to land, and
// still print "Upgraded successfully!": the unit could serve a different home,
// relaunch a different binary, or fail and silently demote to an ad-hoc
// daemon, and every one of those printed success.
//
// Everything the restart path touches is stubbed. These run on a machine with
// a REAL supervised daemon: an unstubbed health probe reads its pid file and
// pings its control socket, and an unstubbed home gate reads its autostart
// unit.

// stubDaemonHealth pins the liveness probe runUpgrade consults before printing
// an all-clear, so no test here reads the host's real pid file or pings its
// control socket.
func stubDaemonHealth(t *testing.T, h daemon.HealthStatus) {
	t.Helper()
	prev := daemonHealthFn
	t.Cleanup(func() { daemonHealthFn = prev })
	daemonHealthFn = func() daemon.HealthStatus { return h }
}

// upgradeHarness stands up a release server and a throwaway "installed"
// binary, and returns the path runUpgrade will overwrite. The binary is real
// on disk because runUpgrade resolves it through EvalSymlinks.
func upgradeHarness(t *testing.T) (binPath string, url string) {
	t.Helper()
	binPath = tempBinPath(t)
	if err := os.WriteFile(binPath, []byte("old-binary"), 0755); err != nil {
		t.Fatalf("seed binary: %v", err)
	}
	archive := makeTarGz(t, map[string][]byte{"agent-factory": []byte("new-binary")})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(archive)
	}))
	t.Cleanup(srv.Close)

	prevExe := osExecutableFn
	t.Cleanup(func() { osExecutableFn = prevExe })
	osExecutableFn = func() (string, error) { return binPath, nil }
	return binPath, srv.URL
}

// stubShutdown pins what RequestShutdown reports and counts the calls.
func stubShutdown(t *testing.T, result daemon.ShutdownResult, err error) *int {
	t.Helper()
	prev := requestDaemonShutdownFn
	t.Cleanup(func() { requestDaemonShutdownFn = prev })
	calls := new(int)
	requestDaemonShutdownFn = func() (daemon.ShutdownResult, error) {
		*calls++
		return result, err
	}
	return calls
}

// TestRespawnAfterUpgrade_LeavesOtherHomesUnitAlone is the #1950 repro, from
// that issue's ready-made failing test (its home gate is stubbed here so it
// cannot read the host's real unit).
//
// `AGENT_FACTORY_HOME=/tmp/sandbox af upgrade` stops the sandbox's daemon,
// then restarts whatever autostart unit EXISTS — the developer's real one,
// serving a different home — and returns on success, skipping the ad-hoc
// fallback. The sandbox it was actually upgrading is left with no daemon at
// all, and someone else's daemon gets a bounce it never asked for.
func TestRespawnAfterUpgrade_LeavesOtherHomesUnitAlone(t *testing.T) {
	restartCalls, ensureCalls := stubRespawnCollaborators(t, true, nil)
	// The unit file exists, and the unit restart would succeed — but it serves
	// a DIFFERENT home than the one being upgraded.
	autostartUnitServesHomeFn = func(string) (serves bool, installed bool, err error) {
		return false, true, nil
	}

	if _, err := respawnDaemonAfterUpgrade(testUpgradeDaemonPath); err != nil {
		t.Fatalf("respawnDaemonAfterUpgrade: %v", err)
	}

	if *restartCalls != 0 {
		t.Fatalf("unit restarts = %d, want 0 (a unit serving another home must never be restarted)", *restartCalls)
	}
	if *ensureCalls != 1 {
		t.Fatalf("ad-hoc spawns = %d, want 1 (the current home must still get a daemon)", *ensureCalls)
	}
}

// TestRespawnAfterUpgrade_RestartsUnitServingThisHome locks the other side of
// the gate: the home check must not cost the #796 supervised restart when the
// unit is ours. Without this, "never restart the unit" would pass #1950.
func TestRespawnAfterUpgrade_RestartsUnitServingThisHome(t *testing.T) {
	restartCalls, ensureCalls := stubRespawnCollaborators(t, true, nil)
	autostartUnitServesHomeFn = func(string) (serves bool, installed bool, err error) {
		return true, true, nil
	}

	if _, err := respawnDaemonAfterUpgrade(testUpgradeDaemonPath); err != nil {
		t.Fatalf("respawnDaemonAfterUpgrade: %v", err)
	}

	if *restartCalls != 1 {
		t.Fatalf("unit restarts = %d, want 1 (a unit serving THIS home stays supervised)", *restartCalls)
	}
	if *ensureCalls != 0 {
		t.Fatalf("ad-hoc spawns = %d, want 0 (a good unit restart must not be demoted)", *ensureCalls)
	}
}

// TestRespawnAfterUpgrade_UnprovableHomeLeavesUnitAlone: the gate is proof, not
// permission. When the unit's home cannot be determined, restarting it is a
// coin flip on someone else's daemon — spawn ad-hoc for the home in front of us
// instead.
func TestRespawnAfterUpgrade_UnprovableHomeLeavesUnitAlone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		serves func(string) (bool, bool, error)
		config func() (string, error)
	}{
		{
			name:   "unit unreadable",
			serves: func(string) (bool, bool, error) { return false, true, errors.New("permission denied") },
			config: func() (string, error) { return "/tmp/af-test-home", nil },
		},
		{
			name:   "config dir unresolvable",
			serves: func(string) (bool, bool, error) { return true, true, nil },
			config: func() (string, error) { return "", errors.New("no home dir") },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restartCalls, ensureCalls := stubRespawnCollaborators(t, true, nil)
			autostartUnitServesHomeFn = tc.serves
			configDirFn = tc.config

			if _, err := respawnDaemonAfterUpgrade(testUpgradeDaemonPath); err != nil {
				t.Fatalf("respawnDaemonAfterUpgrade: %v", err)
			}

			if *restartCalls != 0 {
				t.Fatalf("unit restarts = %d, want 0 (an unproven unit must not be touched)", *restartCalls)
			}
			if *ensureCalls != 1 {
				t.Fatalf("ad-hoc spawns = %d, want 1 (this home still needs a daemon)", *ensureCalls)
			}
		})
	}
}

// TestUpgrade_NoRestartSkipsRestart pins the --no-restart opt-out: it must not
// stop the daemon at all. The binary swap still happens.
func TestUpgrade_NoRestartSkipsRestart(t *testing.T) {
	binPath, url := upgradeHarness(t)
	shutdownCalls := stubShutdown(t, daemon.ShutdownViaRPC, nil)
	stubDaemonHealth(t, daemon.HealthStatus{})

	var out, errOut bytes.Buffer
	if err := runUpgrade(&out, &errOut, url, true); err != nil {
		t.Fatalf("runUpgrade --no-restart: %v", err)
	}

	if *shutdownCalls != 0 {
		t.Fatalf("shutdown calls = %d, want 0 (--no-restart must leave the daemon alone)", *shutdownCalls)
	}
	got, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read upgraded binary: %v", err)
	}
	if string(got) != "new-binary" {
		t.Fatalf("binary contents = %q, want new-binary (--no-restart must still install)", got)
	}
	// The user opted out, so this is not a warning — but it must not read as
	// "you are running the new version" either.
	if !strings.Contains(out.String(), "--no-restart") {
		t.Fatalf("stdout must say the daemon was deliberately left on the old binary.\ngot=%q", out.String())
	}
}

// TestUpgrade_DefaultRestarts locks the default the whole issue turns on: the
// restart is not opt-in. If this ever flips, a shipped fix stops reaching the
// daemon and #1947 is back.
func TestUpgrade_DefaultRestarts(t *testing.T) {
	_, url := upgradeHarness(t)
	shutdownCalls := stubShutdown(t, daemon.ShutdownViaRPC, nil)
	stubDaemonHealth(t, daemon.HealthStatus{})
	stubRespawnCollaborators(t, false, nil)

	var out, errOut bytes.Buffer
	if err := runUpgrade(&out, &errOut, url, false); err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}

	if *shutdownCalls != 1 {
		t.Fatalf("shutdown calls = %d, want 1 (af upgrade restarts the daemon by default)", *shutdownCalls)
	}
	if !strings.Contains(out.String(), "Restarted the running daemon") {
		t.Fatalf("stdout should report the restart.\ngot=%q", out.String())
	}
}

// TestUpgrade_NoRestartFlagRegistered pins the flag onto the command with the
// default the restart behavior depends on. runUpgrade's noRestart parameter is
// covered by the two tests above; this covers the wiring that reaches it.
func TestUpgrade_NoRestartFlagRegistered(t *testing.T) {
	f := upgradeCmd.Flags().Lookup("no-restart")
	if f == nil {
		t.Fatal("af upgrade has no --no-restart flag")
	}
	if f.DefValue != "false" {
		t.Fatalf("--no-restart default = %q, want false (restarting is the default)", f.DefValue)
	}
	if !strings.Contains(f.Usage, "restarts it by default") {
		t.Fatalf("--no-restart help must say the restart already happens by default.\ngot=%q", f.Usage)
	}
}

// TestUpgrade_FailedUnitRestartIsLoud is a repro for the silent demotion. When
// the unit restart fails, the respawn falls back to an ad-hoc daemon and only
// WARNs to the log — which the user never sees — while stdout says "Restarted
// the running daemon from the new binary". The daemon is now unsupervised and
// will not come back at next login, and nothing told the user.
func TestUpgrade_FailedUnitRestartIsLoud(t *testing.T) {
	binPath, url := upgradeHarness(t)
	stubShutdown(t, daemon.ShutdownViaRPC, nil)
	stubDaemonHealth(t, daemon.HealthStatus{})
	// A unit that serves this home and launches this very binary, so the ONLY
	// thing wrong is the failing restart.
	stubRespawnCollaborators(t, true, errors.New("systemctl exited 1"))
	autostartUnitExecPathFn = func() (string, bool, error) { return binPath, true, nil }

	var out, errOut bytes.Buffer
	if err := runUpgrade(&out, &errOut, url, false); err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}

	if errOut.Len() == 0 {
		t.Fatalf("a failed unit restart must reach the user, not just the log; stderr was empty.\nstdout=%q", out.String())
	}
	for _, want := range []string{"systemctl exited 1", "af daemon install"} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatalf("stderr missing %q.\ngot=%q", want, errOut.String())
		}
	}
}

// TestUpgrade_StaleUnitBinaryIsLoud is the repro for the most likely macOS
// cause: the unit bakes its program path at install time, so with two installs
// on one box (Homebrew /opt/homebrew/bin/af vs ~/.local/bin/af) `af upgrade`
// overwrites the one it is running, the unit restart faithfully brings the
// OTHER, older binary back up, and we print "Upgraded successfully! Restarted
// the running daemon from the new binary." Every word of that is wrong.
func TestUpgrade_StaleUnitBinaryIsLoud(t *testing.T) {
	const otherInstall = "/opt/homebrew/bin/af"
	_, url := upgradeHarness(t)
	stubShutdown(t, daemon.ShutdownViaRPC, nil)
	stubDaemonHealth(t, daemon.HealthStatus{})
	stubRespawnCollaborators(t, true, nil)
	autostartUnitExecPathFn = func() (string, bool, error) { return otherInstall, true, nil }

	var out, errOut bytes.Buffer
	if err := runUpgrade(&out, &errOut, url, false); err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}

	if !strings.Contains(errOut.String(), otherInstall) {
		t.Fatalf("a unit that relaunches a DIFFERENT binary must be reported; stderr did not name %s.\nstdout=%q\nstderr=%q",
			otherInstall, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "af daemon install") {
		t.Fatalf("stderr must name the fix.\ngot=%q", errOut.String())
	}
}

// TestUpgrade_UnitLaunchingTheUpgradedBinaryIsQuiet locks the staleness check
// against false alarms: the normal single-install case must stay quiet, or the
// warning becomes noise users learn to ignore.
func TestUpgrade_UnitLaunchingTheUpgradedBinaryIsQuiet(t *testing.T) {
	binPath, url := upgradeHarness(t)
	stubShutdown(t, daemon.ShutdownViaRPC, nil)
	stubDaemonHealth(t, daemon.HealthStatus{})
	stubRespawnCollaborators(t, true, nil)
	autostartUnitExecPathFn = func() (string, bool, error) { return binPath, true, nil }

	var out, errOut bytes.Buffer
	if err := runUpgrade(&out, &errOut, url, false); err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}

	if errOut.Len() != 0 {
		t.Fatalf("a unit launching the upgraded binary must not warn.\ngot stderr=%q", errOut.String())
	}
	if !strings.Contains(out.String(), "Restarted the running daemon from the new binary") {
		t.Fatalf("stdout should report the clean restart.\ngot=%q", out.String())
	}
}

// TestUpgrade_NoDaemonButDaemonRunningIsNotUnqualifiedSuccess is the repro for
// the false "no daemon". RequestShutdown reports ShutdownNoDaemon for a
// missing OR unreachable socket, so "nothing was running" and "a daemon is
// running that we cannot reach" arrive identically — and the default branch
// printed a bare "Upgraded successfully!" over both. The daemon kept serving
// the old binary with nothing said.
func TestUpgrade_NoDaemonButDaemonRunningIsNotUnqualifiedSuccess(t *testing.T) {
	_, url := upgradeHarness(t)
	stubShutdown(t, daemon.ShutdownNoDaemon, nil)
	// The socket said "nobody home"; the process table disagrees.
	stubDaemonHealth(t, daemon.HealthStatus{PIDFilePID: 4242, PIDVerified: true})

	var out, errOut bytes.Buffer
	if err := runUpgrade(&out, &errOut, url, false); err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}

	if errOut.Len() == 0 {
		t.Fatalf("a running-but-unreachable daemon must be reported, not papered over with success.\nstdout=%q", out.String())
	}
	for _, want := range []string{"4242", "old binary", "af daemon restart"} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatalf("stderr missing %q.\ngot=%q", want, errOut.String())
		}
	}
}

// TestUpgrade_NoDaemonIsQuietWhenTrulyNoDaemon locks the common case — fresh
// installs, CI, and any `af upgrade` with nothing running — against the check
// above turning into a false alarm on every run.
func TestUpgrade_NoDaemonIsQuietWhenTrulyNoDaemon(t *testing.T) {
	_, url := upgradeHarness(t)
	stubShutdown(t, daemon.ShutdownNoDaemon, nil)
	stubDaemonHealth(t, daemon.HealthStatus{})

	var out, errOut bytes.Buffer
	if err := runUpgrade(&out, &errOut, url, false); err != nil {
		t.Fatalf("runUpgrade: %v", err)
	}

	if errOut.Len() != 0 {
		t.Fatalf("no daemon running is not a problem and must stay quiet.\ngot stderr=%q", errOut.String())
	}
	if strings.TrimSpace(out.String()) != "Upgraded successfully!" {
		t.Fatalf("stdout = %q, want a plain success line", out.String())
	}
}

// TestUpgrade_UnreachableDaemonIsLoud: a daemon proven to be listening that we
// could not stop (ShutdownFailed/#553, ShutdownError/#978) always arrives with
// an error. That error used to print to STDOUT prefixed with "Upgraded
// successfully!" — the user is left on the old daemon either way, so it belongs
// on stderr with the recovery command.
func TestUpgrade_UnreachableDaemonIsLoud(t *testing.T) {
	_, url := upgradeHarness(t)
	stubShutdown(t, daemon.ShutdownError, errors.New("dial timeout"))
	stubDaemonHealth(t, daemon.HealthStatus{})

	var out, errOut bytes.Buffer
	if err := runUpgrade(&out, &errOut, url, false); err != nil {
		t.Fatalf("runUpgrade must not fail the install when the daemon cannot be stopped: %v", err)
	}

	for _, want := range []string{"dial timeout", "old binary", "af daemon restart"} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatalf("stderr missing %q.\ngot=%q", want, errOut.String())
		}
	}
}
