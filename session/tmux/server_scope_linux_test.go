//go:build linux

package tmux

import (
	"bytes"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/systemdunit"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/log/logtest"
)

func TestNewTmuxServerCommandScopesDaemonUnitSpawn(t *testing.T) {
	t.Setenv(systemdunit.DaemonMarkerEnv, systemdunit.DaemonUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))

	cmd, scoped := newTmuxServerCommandAfterEnsure(EnsureDaemonServer(), "new-session", "-d", "-s", "af_worker")
	if !scoped {
		t.Fatal("daemon-owned tmux server command was not marked systemd-scoped")
	}
	want := "systemd-run --user --scope --quiet --collect -- tmux new-session -d -s af_worker"
	if got := strings.Join(cmd.Args, " "); got != want {
		t.Fatalf("daemon-owned server command = %q, want %q", got, want)
	}
}

func TestNewTmuxServerCommandDoesNotScopeClientWhenServerAlreadyExists(t *testing.T) {
	testguard.IsolateTmux(t)
	t.Setenv(systemdunit.DaemonMarkerEnv, systemdunit.DaemonUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))

	// Start a foreign/legacy private server with the ordinary exit-empty policy.
	// The daemon may attach clients, but must not make that server immortal: once
	// its last session ends it should exit and be replaced by the dedicated scope.
	out, err := exec.Command("tmux", "new-session", "-d", "-s", "preexisting", "sleep", "60").CombinedOutput()
	if err != nil {
		t.Fatalf("start private tmux server: %v: %s", err, out)
	}
	if out, err := exec.Command("tmux", "set-option", "-g", "exit-empty", "on").CombinedOutput(); err != nil {
		t.Fatalf("set private server exit-empty on: %v: %s", err, out)
	}
	restore := ConfigureDaemonServer(t.TempDir())
	t.Cleanup(restore)

	cmd, scoped := newTmuxServerCommandAfterEnsure(EnsureDaemonServer(), "new-session", "-d", "-s", "af_worker")
	if scoped {
		t.Fatal("session client was put in a transient scope even though the shared server already exists")
	}
	want := "tmux -N new-session -d -s af_worker"
	if got := strings.Join(cmd.Args, " "); got != want {
		t.Fatalf("existing-server client command = %q, want %q", got, want)
	}
	out, err = exec.Command("tmux", "show-options", "-gv", "exit-empty").CombinedOutput()
	if err != nil {
		t.Fatalf("read existing server exit-empty: %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "on" {
		t.Fatalf("daemon changed existing server exit-empty to %q, want on", got)
	}
}

func TestEnsuredSessionClientCannotRecreateVanishedServer(t *testing.T) {
	testguard.IsolateTmux(t)
	t.Setenv(systemdunit.DaemonMarkerEnv, systemdunit.DaemonUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))
	restore := ConfigureDaemonServer(t.TempDir())
	t.Cleanup(restore)

	// nil records an earlier successful ensure, but the private server is now
	// absent as if it exited before this client connected. The final client must
	// fail instead of making the daemon service the replacement server's owner.
	cmd, scoped := newTmuxServerCommandAfterEnsure(
		nil, "new-session", "-d", "-s", "af_raced_replacement", "sleep", "60",
	)
	if scoped {
		t.Fatal("successfully ensured client unexpectedly used the fail-open session scope")
	}
	if err := cmd.Run(); err == nil {
		t.Fatal("client recreated a vanished shared server from the daemon cgroup")
	}
	if tmuxServerRunning() {
		t.Fatal("no-autostart client left a replacement tmux server running")
	}
}

func TestOldTmuxUsesSessionScopeCompatibilityFallback(t *testing.T) {
	dir := t.TempDir()
	tmuxShim := filepath.Join(dir, "tmux")
	if err := os.WriteFile(tmuxShim, []byte("#!/bin/sh\nif [ \"$1\" = -V ]; then echo 'tmux 3.2a'; exit 0; fi\nexit 64\n"), 0o700); err != nil {
		t.Fatalf("write old tmux shim: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(systemdunit.DaemonMarkerEnv, systemdunit.DaemonUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))
	restore := ConfigureDaemonServer(t.TempDir())
	t.Cleanup(restore)

	cmd, scoped := newTmuxServerCommandAfterEnsure(nil, "new-session", "-d", "-s", "af_old_tmux")
	if !scoped {
		t.Fatalf("tmux 3.2a client command %q bypassed the compatibility scope", strings.Join(cmd.Args, " "))
	}
	want := "systemd-run --user --scope --quiet --collect -- tmux new-session -d -s af_old_tmux"
	if got := strings.Join(cmd.Args, " "); got != want {
		t.Fatalf("old-tmux compatibility command = %q, want %q", got, want)
	}
}

func TestNoStartProbeFailureDoesNotDowngradeAndIsRetried(t *testing.T) {
	dir := t.TempDir()
	attemptsPath := filepath.Join(dir, "attempts")
	tmuxShim := filepath.Join(dir, "tmux")
	script := `#!/bin/sh
set -eu
attempt=0
if [ -f "$AF_TEST_TMUX_PROBE_ATTEMPTS" ]; then
    attempt=$(cat "$AF_TEST_TMUX_PROBE_ATTEMPTS")
fi
attempt=$((attempt + 1))
printf '%s\n' "$attempt" >"$AF_TEST_TMUX_PROBE_ATTEMPTS"
if [ "$attempt" -eq 1 ]; then
    exit 75
fi
printf '%s\n' 'tmux 3.4'
`
	if err := os.WriteFile(tmuxShim, []byte(script), 0o700); err != nil {
		t.Fatalf("write tmux probe shim: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AF_TEST_TMUX_PROBE_ATTEMPTS", attemptsPath)
	t.Setenv(systemdunit.DaemonMarkerEnv, systemdunit.DaemonUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))
	restore := ConfigureDaemonServer(t.TempDir())
	t.Cleanup(restore)

	var warnings logtest.Buffer
	oldWarningOut, oldWarningFlags := log.WarningLog.Writer(), log.WarningLog.Flags()
	log.WarningLog.SetOutput(&warnings)
	log.WarningLog.SetFlags(0)
	t.Cleanup(func() {
		log.WarningLog.SetOutput(oldWarningOut)
		log.WarningLog.SetFlags(oldWarningFlags)
	})

	cmd, scoped := newTmuxServerCommandAfterEnsure(nil, "new-session", "-d", "-s", "af_probe_failed")
	if scoped {
		t.Errorf("failed tmux -V probe downgraded client to compatibility scope: %q", strings.Join(cmd.Args, " "))
	}
	if got := strings.Join(cmd.Args, " "); got != "tmux -N new-session -d -s af_probe_failed" {
		t.Errorf("failed-probe client command = %q, want isolation-preserving -N client", got)
	}
	for _, want := range []string{"tmux -V probe failed:", "exit status 75"} {
		if !strings.Contains(warnings.String(), want) {
			t.Errorf("failed-probe warning %q does not contain %q", warnings.String(), want)
		}
	}
	if strings.Contains(warnings.String(), "lacks a verified -N") {
		t.Errorf("failed probe was logged as a stable unsupported verdict: %q", warnings.String())
	}

	cmd, scoped = newTmuxServerCommandAfterEnsure(nil, "new-session", "-d", "-s", "af_probe_retry")
	if scoped {
		t.Errorf("successful retry still used compatibility scope: %q", strings.Join(cmd.Args, " "))
	}
	if got := strings.Join(cmd.Args, " "); got != "tmux -N new-session -d -s af_probe_retry" {
		t.Errorf("retried-probe client command = %q, want cached supported client", got)
	}
	attempts, err := os.ReadFile(attemptsPath)
	if err != nil {
		t.Fatalf("read tmux probe attempts: %v", err)
	}
	if got := strings.TrimSpace(string(attempts)); got != "2" {
		t.Fatalf("tmux -V probe attempts = %q, want failure plus one retry", got)
	}
}

func TestNoStartProbeCachesSupportedVerdict(t *testing.T) {
	dir := t.TempDir()
	attemptsPath := filepath.Join(dir, "attempts")
	tmuxShim := filepath.Join(dir, "tmux")
	script := `#!/bin/sh
set -eu
attempt=0
if [ -f "$AF_TEST_TMUX_PROBE_ATTEMPTS" ]; then
    attempt=$(cat "$AF_TEST_TMUX_PROBE_ATTEMPTS")
fi
printf '%s\n' "$((attempt + 1))" >"$AF_TEST_TMUX_PROBE_ATTEMPTS"
printf '%s\n' 'tmux 3.4'
`
	if err := os.WriteFile(tmuxShim, []byte(script), 0o700); err != nil {
		t.Fatalf("write tmux probe shim: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AF_TEST_TMUX_PROBE_ATTEMPTS", attemptsPath)
	t.Setenv(systemdunit.DaemonMarkerEnv, systemdunit.DaemonUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))
	restore := ConfigureDaemonServer(t.TempDir())
	t.Cleanup(restore)

	for i := 0; i < 2; i++ {
		cmd, scoped := newTmuxServerCommandAfterEnsure(nil, "new-session", "-d", "-s", "af_supported")
		if scoped || strings.Join(cmd.Args, " ") != "tmux -N new-session -d -s af_supported" {
			t.Fatalf("supported tmux client = %q, scoped=%v", strings.Join(cmd.Args, " "), scoped)
		}
	}
	attempts, err := os.ReadFile(attemptsPath)
	if err != nil {
		t.Fatalf("read tmux probe attempts: %v", err)
	}
	if got := strings.TrimSpace(string(attempts)); got != "1" {
		t.Fatalf("tmux -V probe attempts = %q, want one cached supported verdict", got)
	}
}

func TestNoStartProbeCachesUnsupportedVerdict(t *testing.T) {
	dir := t.TempDir()
	attemptsPath := filepath.Join(dir, "attempts")
	tmuxShim := filepath.Join(dir, "tmux")
	script := `#!/bin/sh
set -eu
attempt=0
if [ -f "$AF_TEST_TMUX_PROBE_ATTEMPTS" ]; then
    attempt=$(cat "$AF_TEST_TMUX_PROBE_ATTEMPTS")
fi
attempt=$((attempt + 1))
printf '%s\n' "$attempt" >"$AF_TEST_TMUX_PROBE_ATTEMPTS"
if [ "$attempt" -eq 1 ]; then
    printf '%s\n' 'tmux 3.2a'
else
    printf '%s\n' 'tmux 3.4'
fi
`
	if err := os.WriteFile(tmuxShim, []byte(script), 0o700); err != nil {
		t.Fatalf("write tmux probe shim: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AF_TEST_TMUX_PROBE_ATTEMPTS", attemptsPath)
	t.Setenv(systemdunit.DaemonMarkerEnv, systemdunit.DaemonUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))
	restore := ConfigureDaemonServer(t.TempDir())
	t.Cleanup(restore)

	for i := 0; i < 2; i++ {
		cmd, scoped := newTmuxServerCommandAfterEnsure(nil, "new-session", "-d", "-s", "af_unsupported")
		if !scoped {
			t.Fatalf("old tmux client bypassed compatibility scope on call %d: %q", i+1, strings.Join(cmd.Args, " "))
		}
	}
	attempts, err := os.ReadFile(attemptsPath)
	if err != nil {
		t.Fatalf("read tmux probe attempts: %v", err)
	}
	if got := strings.TrimSpace(string(attempts)); got != "1" {
		t.Fatalf("tmux -V probe attempts = %q, want one cached unsupported verdict", got)
	}
}

func TestTmuxVersionProbeAllowsMoreTimeThanSocketProbe(t *testing.T) {
	if tmuxVersionProbeTimeout <= serverProbeTimeout {
		t.Fatalf("tmux -V timeout = %s, want longer than socket probe timeout %s", tmuxVersionProbeTimeout, serverProbeTimeout)
	}
}

func TestTmuxVersionSupportsNoStart(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    bool
	}{
		{version: "tmux 3.2a", want: false},
		{version: "tmux 3.3", want: true},
		{version: "tmux 3.3a", want: true},
		{version: "tmux 3.4", want: true},
		{version: "tmux next-3.6", want: true},
		{version: "tmux master", want: true},
		{version: "tmux openbsd-7.4", want: false},
		{version: "garbage", want: false},
	} {
		if got := tmuxVersionAtLeast(tc.version, 3, 3); got != tc.want {
			t.Errorf("tmuxVersionAtLeast(%q, 3, 3) = %v, want %v", tc.version, got, tc.want)
		}
	}
}

func TestNewTmuxServerCommandDoesNotTrustInheritedSystemdMarker(t *testing.T) {
	t.Setenv(systemdunit.DaemonMarkerEnv, systemdunit.DaemonUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()+1))

	cmd, scoped := newTmuxServerCommandAfterEnsure(EnsureDaemonServer(), "new-session")
	if scoped {
		t.Fatal("descendant with inherited marker was marked systemd-scoped")
	}
	if got := strings.Join(cmd.Args, " "); got != "tmux new-session" {
		t.Fatalf("descendant with inherited marker launched %q, want direct tmux client", got)
	}
}

func TestDedicatedServerScopeIsNamedAndIndependent(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), dedicatedServerLogName)
	t.Setenv("AF_TEST_UNRELATED_SECRET", "must-not-enter-tmux-server")
	cmd := dedicatedServerScopeCommand(logPath, "agent-factory-tmux-server-unit-under-test")
	got := strings.Join(cmd.Args, " ")
	for _, want := range []string{
		"systemd-run --user --scope --quiet --collect",
		"--unit=agent-factory-tmux-server-unit-under-test",
		"--property=KillMode=control-group",
		" /proc/" + strconv.Itoa(os.Getpid()) + "/exe " + dedicatedServerExecMarker,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("dedicated server command %q does not contain %q", got, want)
		}
	}
	for _, forbidden := range []string{"BindsTo=", "After=" + systemdunit.DaemonUnitName, "new-session"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("dedicated server command %q unexpectedly contains %q", got, forbidden)
		}
	}
	wantEnv := dedicatedServerLogEnv + "=" + logPath
	if !containsExact(cmd.Env, wantEnv) {
		t.Fatalf("dedicated server environment does not contain %q", wantEnv)
	}
	for _, entry := range cmd.Env {
		if strings.HasPrefix(entry, "AF_TEST_UNRELATED_SECRET=") {
			t.Fatal("dedicated tmux server inherited an unrelated daemon secret")
		}
	}
}

func TestDedicatedServerWrapperCapturesStderr(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	tmuxShim := filepath.Join(dir, "tmux")
	// Presence reads go to the probe path, not the captured launch args: the
	// post-exit verdict read (#7557) is a show-options call, and only the
	// foreground -D launch is what this fixture means to capture.
	script := "#!/bin/sh\nif [ \"$1\" != \"show-options\" ]; then printf '%s\\n' \"$*\" >\"$AF_TEST_TMUX_ARGS\"; fi\nprintf '%s\\n' 'fatal-sentinel-3307' >&2\nexit 42\n"
	if err := os.WriteFile(tmuxShim, []byte(script), 0o700); err != nil {
		t.Fatalf("write tmux shim: %v", err)
	}
	logPath := filepath.Join(dir, dedicatedServerLogName)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AF_TEST_TMUX_ARGS", argsPath)
	t.Setenv(dedicatedServerLogEnv, logPath)

	if err := runDedicatedServer(); err == nil {
		t.Fatal("foreground tmux failure was reported as success")
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read tmux shim args: %v", err)
	}
	if got := strings.TrimSpace(string(args)); got != "-D" {
		t.Fatalf("tmux wrapper args = %q, want foreground -D", got)
	}
	contents, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read tmux server log: %v", err)
	}
	for _, want := range []string{"fatal-sentinel-3307", "exit status 42"} {
		if !strings.Contains(string(contents), want) {
			t.Fatalf("tmux server log %q does not contain %q", contents, want)
		}
	}
}

func TestDedicatedServerFailureFallsBackToSessionScope(t *testing.T) {
	testguard.IsolateTmux(t)
	dir := t.TempDir()
	systemdRun := filepath.Join(dir, "systemd-run")
	if err := os.WriteFile(systemdRun, []byte("#!/bin/sh\nexit 77\n"), 0o700); err != nil {
		t.Fatalf("write systemd-run shim: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(systemdunit.DaemonMarkerEnv, systemdunit.DaemonUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))
	restore := ConfigureDaemonServer(t.TempDir())
	t.Cleanup(restore)

	cmd, scoped := newTmuxServerCommandAfterEnsure(EnsureDaemonServer(), "new-session", "-d", "-s", "af_worker")
	if !scoped {
		t.Fatal("dedicated-scope failure refused the historical per-session fallback")
	}
	want := "systemd-run --user --scope --quiet --collect -- tmux new-session -d -s af_worker"
	if got := strings.Join(cmd.Args, " "); got != want {
		t.Fatalf("fallback command = %q, want %q", got, want)
	}
}

func TestDedicatedServerTimeoutStopsAndReapsLauncher(t *testing.T) {
	testguard.IsolateTmux(t)
	dir := t.TempDir()
	systemdRun := filepath.Join(dir, "systemd-run")
	parentPIDPath := systemdRun + ".parent"
	childPIDPath := systemdRun + ".child"
	script := "#!/bin/sh\nprintf '%s\\n' \"$$\" >\"$0.parent\"\nsleep 60 &\nchild=$!\nprintf '%s\\n' \"$child\" >\"$0.child\"\nwait \"$child\"\n"
	if err := os.WriteFile(systemdRun, []byte(script), 0o700); err != nil {
		t.Fatalf("write blocking systemd-run shim: %v", err)
	}
	systemctl := filepath.Join(dir, "systemctl")
	// The kills land after this shim has answered, which is what systemd does:
	// `systemctl kill` queues the signals and returns, and the kernel finishes
	// the teardown on its own schedule. Delaying them deterministically is the
	// regression for #3382 — the assertion below used to probe the pids once,
	// instantly, so it read a launcher child that was merely on its way out as
	// one that survived. Its fds are redirected so CombinedOutput does not wait
	// on the backgrounded subshell.
	stopScript := "#!/bin/sh\nprintf '%s\\n' \"$*\" >\"$0.args\"\nbase=$(dirname \"$0\")/systemd-run\n(\n  sleep 0.2\n  for suffix in child parent; do\n    pid=$(cat \"$base.$suffix\")\n    kill -KILL \"$pid\" 2>/dev/null || true\n  done\n) >/dev/null 2>&1 &\n"
	if err := os.WriteFile(systemctl, []byte(stopScript), 0o700); err != nil {
		t.Fatalf("write systemctl shim: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(systemdunit.DaemonMarkerEnv, systemdunit.DaemonUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))
	restore := ConfigureDaemonServer(t.TempDir())
	t.Cleanup(restore)

	if err := EnsureDaemonServer(); err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("EnsureDaemonServer error = %v, want readiness timeout", err)
	}
	stopArgs, err := os.ReadFile(systemctl + ".args")
	if err != nil {
		t.Fatalf("read systemctl args: %v", err)
	}
	// The unit is unique per launch (#6180), so check its shape rather than a name.
	gotStop := strings.TrimSpace(string(stopArgs))
	wantStopPrefix := "--user kill --kill-who=all --signal=KILL " + dedicatedServerScopeName() + "-"
	if !strings.HasPrefix(gotStop, wantStopPrefix) || !strings.HasSuffix(gotStop, ".scope") {
		t.Fatalf("systemctl cleanup args = %q, want a per-launch unit starting %q ending .scope", gotStop, wantStopPrefix)
	}
	for label, pidPath := range map[string]string{"launcher": parentPIDPath, "launcher child": childPIDPath} {
		rawPID, err := os.ReadFile(pidPath)
		if err != nil {
			t.Fatalf("read %s pid: %v", label, err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
		if err != nil {
			t.Fatalf("parse %s pid: %v", label, err)
		}
		// An identity, not a bare pid: (pid, StartID) names a process INSTANCE, so
		// nothing below can mistake a recycled number for a survivor or aim a kill
		// at whatever inherited it. Lookup answers three ways and only one of them
		// has anything left to wait for.
		process, err := proctree.Lookup(pid)
		switch {
		case errors.Is(err, proctree.ErrProcessExited), errors.Is(err, os.ErrNotExist):
			// Gone already: reaped, or a corpse nobody has collected yet. A zombie
			// counts, and that is not a concession — it has exited, and who collects
			// its status is somebody else's schedule (#2103).
			continue
		case err != nil:
			t.Errorf("probe timed-out %s %d: %v", label, pid, err)
			continue
		}
		gone, probeErr := waitForProcessTeardown(process, dedicatedServerTeardownWait)
		switch {
		case probeErr != nil:
			t.Errorf("probe timed-out %s %d: %v", label, pid, probeErr)
		case !gone:
			// Only now that the wait has genuinely expired: a survivor is a real
			// failure, and killing it keeps that failure from leaking a process into
			// the next run. Signal re-verifies the identity first, so a PID recycled
			// since the capture above is left alone rather than killed.
			if killErr := proctree.Signal(process, syscall.SIGKILL); killErr != nil &&
				!errors.Is(killErr, proctree.ErrIdentityChanged) {
				t.Errorf("kill surviving %s %d: %v", label, pid, killErr)
			}
			t.Errorf("timed-out dedicated server %s %d is still alive after %s",
				label, pid, dedicatedServerTeardownWait)
		}
	}
}

// dedicatedServerTeardownWait is how long the assertion above watches for the
// killed launcher and its child to go away. Large on purpose: the wait returns
// the instant they are gone, so the budget costs nothing on a passing run, and
// only a genuine survivor — one that is still there after the whole of it —
// spends it. A tight bound would be a statement about the CI scheduler rather
// than about the teardown (#2879).
const dedicatedServerTeardownWait = 30 * time.Second

// waitForProcessTeardown reports whether the captured process instance stopped
// running within timeout. Signal delivery and process teardown are
// asynchronous, so there is always a window after the kill in which the pid is
// still probe-able; the caller's old single instantaneous probe granted that
// window zero time and failed whenever CI contention widened it past nothing
// (#3382).
//
// SameIdentity rather than kill(pid, 0), because an existence probe answers the
// wrong question twice over: it accepts a zombie as running (#2103), and it
// accepts a recycled PID as the process that was supposed to die. It is also
// the reason a read failure is RETURNED rather than retried or folded into
// either verdict — "I cannot look" is neither "it is gone" nor "it survived",
// and this package's standing rule is that those never collapse into each
// other.
func waitForProcessTeardown(process proctree.Process, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		alive, err := proctree.SameIdentity(process)
		if err != nil {
			return false, err
		}
		if !alive {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDedicatedServerTightensExistingLogPermissions(t *testing.T) {
	testguard.IsolateTmux(t)
	dir := t.TempDir()
	systemdRun := filepath.Join(dir, "systemd-run")
	if err := os.WriteFile(systemdRun, []byte("#!/bin/sh\nexit 77\n"), 0o700); err != nil {
		t.Fatalf("write systemd-run shim: %v", err)
	}
	home := t.TempDir()
	logPath := filepath.Join(home, dedicatedServerLogName)
	if err := os.WriteFile(logPath, []byte("preexisting diagnostics\n"), 0o644); err != nil {
		t.Fatalf("write existing tmux server log: %v", err)
	}
	if err := os.Chmod(logPath, 0o644); err != nil {
		t.Fatalf("make existing tmux server log permissive: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(systemdunit.DaemonMarkerEnv, systemdunit.DaemonUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))
	restore := ConfigureDaemonServer(home)
	t.Cleanup(restore)

	if err := EnsureDaemonServer(); err == nil {
		t.Fatal("refusing systemd-run shim unexpectedly launched a server")
	}
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat existing tmux server log: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("existing tmux server log mode = %04o, want 0600", got)
	}
}

func TestDedicatedServerWrapperAppliesLiveRotationPolicy(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	configPath := filepath.Join(home, "config.toml")
	if err := os.WriteFile(configPath, []byte("log_max_size_mb = 2\nlog_max_backups = 1\n"), 0o600); err != nil {
		t.Fatalf("write initial log policy: %v", err)
	}
	payloadPath := filepath.Join(dir, "payload")
	if err := os.WriteFile(payloadPath, bytes.Repeat([]byte("x"), 1200*1024), 0o600); err != nil {
		t.Fatalf("write stderr payload: %v", err)
	}
	readyPath := filepath.Join(dir, "ready")
	releasePath := filepath.Join(dir, "release")
	tmuxShim := filepath.Join(dir, "tmux")
	script := `#!/bin/sh
: >"$AF_TEST_TMUX_READY"
` + testguard.BoundedGateWait(releasePath, 10*time.Millisecond, 5*time.Minute) + `
cat "$AF_TEST_TMUX_PAYLOAD" >&2
exit 42
`
	if err := os.WriteFile(tmuxShim, []byte(script), 0o700); err != nil {
		t.Fatalf("write tmux shim: %v", err)
	}
	logPath := filepath.Join(home, dedicatedServerLogName)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("AF_TEST_TMUX_READY", readyPath)
	t.Setenv("AF_TEST_TMUX_RELEASE", releasePath)
	t.Setenv("AF_TEST_TMUX_PAYLOAD", payloadPath)
	t.Setenv(dedicatedServerLogEnv, logPath)
	done := make(chan error, 1)
	go func() { done <- runDedicatedServer() }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("tmux shim did not reach its write barrier")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.WriteFile(configPath, []byte("log_max_size_mb = 1\nlog_max_backups = 0\n"), 0o600); err != nil {
		t.Fatalf("write live log policy: %v", err)
	}
	if err := os.WriteFile(releasePath, nil, 0o600); err != nil {
		t.Fatalf("release tmux shim: %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("failing tmux shim returned success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tmux wrapper did not return")
	}
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat dynamically rotated tmux server log: %v", err)
	}
	if info.Size() >= 1024*1024 {
		t.Fatalf("tmux server log stayed at %d bytes after live 1MB policy, want below cap", info.Size())
	}
	if _, err := os.Stat(logPath + ".1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("live log_max_backups=0 retained a rotated backup: %v", err)
	}
}

func TestEnsureDaemonServerStartsForegroundServerBeforeSessionClient(t *testing.T) {
	testguard.IsolateTmux(t)
	dir := t.TempDir()
	systemdRun := filepath.Join(dir, "systemd-run")
	script := `#!/bin/sh
set -eu
while [ "$#" -gt 0 ]; do
    case "$1" in
        --user|--scope|--quiet|--collect|--unit=*|--property=*) shift ;;
        --) shift; break ;;
        *) exit 64 ;;
    esac
done
exec "$@"
`
	if err := os.WriteFile(systemdRun, []byte(script), 0o700); err != nil {
		t.Fatalf("write systemd-run shim: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(systemdunit.DaemonMarkerEnv, systemdunit.DaemonUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))
	restore := ConfigureDaemonServer(t.TempDir())
	t.Cleanup(restore)

	if err := EnsureDaemonServer(); err != nil {
		t.Fatalf("ensure dedicated server: %v", err)
	}
	out, err := exec.Command("tmux", "show-options", "-gv", "exit-empty").CombinedOutput()
	if err != nil {
		t.Fatalf("read private server exit-empty: %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "off" {
		t.Fatalf("private server exit-empty = %q, want off", got)
	}

	cmd, scoped := newTmuxServerCommandAfterEnsure(EnsureDaemonServer(), "new-session", "-d", "-s", "af_worker", "sleep", "60")
	if scoped {
		t.Fatal("session spawn still owned a scope after the dedicated server became ready")
	}
	if got := strings.Join(cmd.Args, " "); got != "tmux -N new-session -d -s af_worker sleep 60" {
		t.Fatalf("session client command = %q", got)
	}
}

func containsExact(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

type refusingTrackedPtyFactory struct {
	t       *testing.T
	waitErr error
}

func (f refusingTrackedPtyFactory) Start(*exec.Cmd) (*os.File, error) {
	f.t.Fatal("Start called instead of StartTracked")
	return nil, nil
}

func (f refusingTrackedPtyFactory) StartTracked(*exec.Cmd) (*os.File, <-chan error, error) {
	ptmx, err := os.CreateTemp(f.t.TempDir(), "scope-refusal-pty")
	if err != nil {
		return nil, nil, err
	}
	done := make(chan error, 1)
	done <- f.waitErr
	close(done)
	return ptmx, done, nil
}

// TestSystemdRunRefusalIsActionableButCleanupUnsafe covers the severe failure
// mode where the wrapper binary exists but the user manager refuses the
// transient scope. Pty used to discard that exit status, so Start waited two
// seconds and blamed tmux readiness; the CreateSession RPC now carries
// systemd-run's role and failure. The wait channel cannot prove whether a
// generic non-zero wrapper status came from systemd-run itself or the scoped
// tmux child, though, so it must not authorize fresh-worktree deletion.
func TestSystemdRunRefusalIsActionableButCleanupUnsafe(t *testing.T) {
	t.Setenv(systemdunit.DaemonMarkerEnv, systemdunit.DaemonUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))
	forceNewSessionEnvMarkers(t, false)

	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "has-session") {
				return errors.New("session not found")
			}
			return nil
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return nil, nil },
	}
	ptyFactory := refusingTrackedPtyFactory{
		t:       t,
		waitErr: errors.New("Failed to start transient scope unit: Access denied"),
	}
	ts := newTmuxSession("af_scope-refusal", "sh", ptyFactory, cmdExec)

	err := ts.Start(t.TempDir())
	if err == nil {
		t.Fatal("Start succeeded after systemd-run refused the transient scope")
	}
	if errors.Is(err, ErrSessionNotStarted) {
		t.Fatalf("a systemd-run exit was mistaken for proof that its scoped child never started: %v", err)
	}
	for _, want := range []string{
		"systemd-run --user --scope failed",
		"Failed to start transient scope unit: Access denied",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Start error %q does not contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "timed out waiting for tmux") {
		t.Fatalf("systemd-run refusal was misreported as a tmux readiness timeout: %v", err)
	}
}

// TestDedicatedServerUnitNeverCollidesWithAStaleOne is #6180: the unit name used
// to be deterministic, so a unit the dead server left loaded (any leftover
// process in its cgroup keeps a scope alive) made every relaunch fail with "was
// already loaded or has a fragment file". Two launches must name different units
// that both keep the per-socket prefix.
func TestDedicatedServerUnitNeverCollidesWithAStaleOne(t *testing.T) {
	a := newDedicatedServerUnit()
	time.Sleep(time.Millisecond)
	b := newDedicatedServerUnit()
	if a == b {
		t.Fatalf("two launches got the same unit %q", a)
	}
	for _, unit := range []string{a, b} {
		if !strings.HasPrefix(unit, dedicatedServerScopeName()+"-") {
			t.Fatalf("unit %q lost the per-socket prefix %q", unit, dedicatedServerScopeName())
		}
	}
}

// TestServerDeathMarkerRecordsExitAndSignal pins #5429 ask 1: the wrapper's
// record of how tmux ended.
func TestServerDeathMarkerRecordsExitAndSignal(t *testing.T) {
	home := t.TempDir()
	run := func(script string) error { return exec.Command("sh", "-c", script).Run() }

	recordServerDeath(home, "unit-x", time.Now().Add(-3*time.Second), run("exit 3"))
	d, ok, err := ReadServerDeath(home)
	if err != nil || !ok || d.ExitCode != 3 || d.Signal != "" || d.Unit != "unit-x" || d.UptimeSeconds < 2 {
		t.Fatalf("exit marker = %+v ok=%v err=%v", d, ok, err)
	}
	recordServerDeath(home, "unit-x", time.Now(), run("kill -KILL $$"))
	d, _, _ = ReadServerDeath(home)
	if d.Signal != "killed" || d.ExitCode != -1 {
		t.Fatalf("signal marker = %+v, want signal killed, exit code -1", d)
	}
	recordServerDeath(home, "unit-x", time.Now(), nil)
	d, _, _ = ReadServerDeath(home)
	if d.ExitCode != 0 || d.Signal != "" || d.Error != "" {
		t.Fatalf("clean marker = %+v", d)
	}
}

func TestServerStateRoundTripAndDedupe(t *testing.T) {
	home := t.TempDir()
	lastServerState = ServerState{}
	st := ServerState{Mode: ServerModeFallback, Socket: "/tmp/tmux-1/default", Reason: "boom", At: time.Now()}
	recordServerState(home, st)
	got, ok, err := ReadServerState(home)
	if err != nil || !ok || got.Mode != ServerModeFallback || got.Socket != st.Socket || got.Reason != "boom" {
		t.Fatalf("state = %+v ok=%v err=%v", got, ok, err)
	}
	if err := os.Remove(filepath.Join(home, serverStateFileName)); err != nil {
		t.Fatal(err)
	}
	recordServerState(home, st) // identical decision: no rewrite on every spawn
	if _, ok, _ := ReadServerState(home); ok {
		t.Fatal("an unchanged decision was rewritten")
	}
}

// --- #7557: the dedicated-server presence verdict is passive and read from
// tmux's own diagnostic alone. A live server answers in one bounded attempt;
// ENOENT and ECONNREFUSED are both definitive for a launch decision; a stalled
// or unclassified read is unknown and licenses neither a second server nor a
// death marker. Nothing on this path signals a server or reads a process
// table — the fixtures below never reach NoServerRunning/tmuxSocketClaimed.

// writeShim drops an executable script under a private dir so a test can put a
// fake tmux or systemd-run first on PATH. Paths the script needs are baked
// into its text, not passed through the environment: the dedicated-server
// wrapper inherits a filtered environment in which test-only variables do not
// survive.
func writeShim(t *testing.T, name, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil {
		t.Fatalf("write %s shim: %v", name, err)
	}
	return dir
}

func tmuxShimOnPath(t *testing.T, script string) {
	t.Helper()
	t.Setenv("PATH", writeShim(t, "tmux", script)+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// delegatingTmuxShim logs every invocation's first arg to logPath, runs
// prelude (extra shell lines inserted before delegation), then execs the REAL
// tmux resolved before the shim dir went on PATH — so fixtures get genuine
// tmux diagnostics with invocation counting on top.
func delegatingTmuxShim(t *testing.T, logPath, prelude string) {
	t.Helper()
	real, err := exec.LookPath("tmux")
	if err != nil {
		t.Skipf("tmux not available: %v", err)
	}
	tmuxShimOnPath(t, "#!/bin/sh\nprintf '%s\\n' \"$1\" >>\""+logPath+"\"\n"+prelude+"exec \""+real+"\" \"$@\"\n")
}

func countTmuxCalls(t *testing.T, logPath, arg string) int {
	t.Helper()
	contents, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read tmux call log: %v", err)
	}
	n := 0
	for _, line := range strings.Split(string(contents), "\n") {
		if line == arg {
			n++
		}
	}
	return n
}

func daemonServerTestEnv(t *testing.T, home string) {
	t.Helper()
	t.Setenv(systemdunit.DaemonMarkerEnv, systemdunit.DaemonUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))
	t.Cleanup(ConfigureDaemonServer(home))
}

// decisionCallOrder reads the shared call log and keeps only the entries that
// mark a bootstrap decision: a "show-options" presence read and the "LAUNCH"
// line the systemd-run shim writes when a launch is attempted. Fixture calls
// (new-session, display-message) and probes after the launch are noise for
// the one-read-before-launch assertion, not signal.
func decisionCallOrder(t *testing.T, logPath string) []string {
	t.Helper()
	contents, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read tmux call log: %v", err)
	}
	var seq []string
	for _, line := range strings.Split(string(contents), "\n") {
		if line == "show-options" || line == "LAUNCH" {
			seq = append(seq, line)
		}
	}
	return seq
}

func noServerDeathMarker(t *testing.T, home string) {
	t.Helper()
	if _, ok, err := ReadServerDeath(home); err != nil || ok {
		t.Fatalf("a death marker exists (ok=%v err=%v) on a path that never proved a server death", ok, err)
	}
}

// TestEnsureDaemonServerAbsentSocketLaunchesOnce is the ENOENT fixture: a
// machine with no tmux server answers "error connecting to <socket> (No such
// file or directory)" — definitive for a launch decision — so the bootstrap
// commits after exactly one bounded read and pays no retry and no grace.
func TestEnsureDaemonServerAbsentSocketLaunchesOnce(t *testing.T) {
	testguard.IsolateTmux(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "tmux-calls")
	delegatingTmuxShim(t, logPath, "")
	// A systemd-run shim that refuses at Start records whether a launch was
	// ever attempted — the gate's absent verdict is what should commit to one.
	systemdRun := writeShim(t, "systemd-run", "#!/bin/sh\nprintf '%s\\n' LAUNCH >>\""+logPath+"\"\nexit 77\n")
	t.Setenv("PATH", systemdRun+string(os.PathListSeparator)+os.Getenv("PATH"))
	home := t.TempDir()
	daemonServerTestEnv(t, home)

	err := EnsureDaemonServer()
	if err == nil || !strings.Contains(err.Error(), "dedicated tmux server scope exited before readiness") {
		t.Fatalf("EnsureDaemonServer error = %v, want the refusing launch to surface", err)
	}
	if seq := decisionCallOrder(t, logPath); len(seq) < 2 || seq[0] != "show-options" || seq[1] != "LAUNCH" {
		t.Fatalf("call order = %v, want a single show-options read before the launch attempt", seq)
	}
	if verdict := probeDedicatedServer(); verdict != dedicatedServerAbsent {
		t.Fatalf("ENOENT verdict = %v, want absent", verdict)
	}
	noServerDeathMarker(t, home)
}

// TestEnsureDaemonServerRefusedSocketLaunchesOnce is the ECONNREFUSED
// fixture: a socket file that outlives its server answers "no server running
// on <socket>" — definitive — so the same one-read launch path applies.
func TestEnsureDaemonServerRefusedSocketLaunchesOnce(t *testing.T) {
	testguard.IsolateTmux(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "tmux-calls")
	delegatingTmuxShim(t, logPath, "")

	// A socket file nobody listens on is exactly what a SIGKILLed server
	// leaves behind, but binding and closing one directly skips the teardown
	// window where a real connect can be answered with a transient error
	// instead of the definitive refusal the verdict needs.
	if err := os.MkdirAll(filepath.Dir(DefaultSocketPath()), 0o700); err != nil {
		t.Fatalf("create private socket dir: %v", err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: DefaultSocketPath()})
	if err != nil {
		t.Fatalf("bind stale socket: %v", err)
	}
	ln.SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatalf("close stale socket: %v", err)
	}

	systemdRun := writeShim(t, "systemd-run", "#!/bin/sh\nprintf '%s\\n' LAUNCH >>\""+logPath+"\"\nexit 77\n")
	t.Setenv("PATH", systemdRun+string(os.PathListSeparator)+os.Getenv("PATH"))
	home := t.TempDir()
	daemonServerTestEnv(t, home)

	err = EnsureDaemonServer()
	if err == nil || !strings.Contains(err.Error(), "dedicated tmux server scope exited before readiness") {
		t.Fatalf("EnsureDaemonServer error = %v, want the refusing launch to surface", err)
	}
	if seq := decisionCallOrder(t, logPath); len(seq) < 2 || seq[0] != "show-options" || seq[1] != "LAUNCH" {
		t.Fatalf("call order = %v, want a single show-options read before the launch attempt", seq)
	}
	if verdict := probeDedicatedServer(); verdict != dedicatedServerAbsent {
		t.Fatalf("ECONNREFUSED verdict = %v, want absent", verdict)
	}
	noServerDeathMarker(t, home)
}

// TestEnsureDaemonServerAdoptsASlowLiveServer pins the other half of "a live
// server answers in one attempt": an answer that is merely SLOW — but inside
// the probe bound — is still a live server. It is adopted in that one read,
// with no second wrapper and no death marker.
func TestEnsureDaemonServerAdoptsASlowLiveServer(t *testing.T) {
	testguard.IsolateTmux(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "tmux-calls")
	// The shim delays only the presence read, comfortably inside the bound.
	delegatingTmuxShim(t, logPath, "if [ \"$1\" = \"show-options\" ]; then sleep 0.05; fi\n")
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", "alive", "sleep", "60").CombinedOutput(); err != nil {
		t.Fatalf("start private tmux server: %v: %s", err, out)
	}
	home := t.TempDir()
	daemonServerTestEnv(t, home)

	if err := EnsureDaemonServer(); err != nil {
		t.Fatalf("ensure dedicated server over a slow live one: %v", err)
	}
	st, ok, err := ReadServerState(home)
	if err != nil || !ok || st.Mode != ServerModeAdopted {
		t.Fatalf("server state = %+v ok=%v err=%v, want adopted", st, ok, err)
	}
	if got := countTmuxCalls(t, logPath, "show-options"); got != 1 {
		t.Fatalf("socket probes = %d, want one bounded read", got)
	}
	noServerDeathMarker(t, home)
}

// TestEnsureDaemonServerStalledProbeRefusesLaunch is the wedge case the
// boolean probe got wrong: a read that never answers is UNKNOWN, not absent.
// The bootstrap must refuse to launch inside the bound — no second server, no
// death marker, and the whole thing returns promptly instead of parking on
// the stalled client.
func TestEnsureDaemonServerStalledProbeRefusesLaunch(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "tmux-calls")
	tmuxShimOnPath(t, "#!/bin/sh\nprintf '%s\\n' \"$1\" >>\""+logPath+"\"\nif [ \"$1\" = \"show-options\" ]; then exec sleep 15; fi\nexit 1\n")
	systemdRun := writeShim(t, "systemd-run", "#!/bin/sh\nprintf '%s\\n' LAUNCH >>\""+logPath+"\"\nexit 77\n")
	t.Setenv("PATH", systemdRun+string(os.PathListSeparator)+os.Getenv("PATH"))
	home := t.TempDir()
	daemonServerTestEnv(t, home)

	started := time.Now()
	err := EnsureDaemonServer()
	elapsed := time.Since(started)
	if err == nil || !strings.Contains(err.Error(), "could not be determined") {
		t.Fatalf("EnsureDaemonServer error = %v, want a non-definitive-read refusal", err)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("stalled probe returned after %s — the bound did not engage", elapsed)
	}
	if got := countTmuxCalls(t, logPath, "show-options"); got != 1 {
		t.Fatalf("socket probes = %d, want the one bounded read", got)
	}
	contents, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("read tmux call log: %v", readErr)
	}
	if strings.Contains(string(contents), "LAUNCH") {
		t.Fatal("a second server was launched on a verdict that was never definitive")
	}
	noServerDeathMarker(t, home)
}

// TestDedicatedServerWrapperSkipsMarkerWhenServerStillAnswers is the attach
// case: the socket was definitively absent when the wrapper's pre-check ran,
// but another server bound it before `tmux -D` executed — which is exactly
// how a stale probe once let a launcher overwrite a healthy server's record.
// `tmux -D` exits with the attach failure; the socket still answering means
// nothing this wrapper owned died, so no death marker may be written.
func TestDedicatedServerWrapperSkipsMarkerWhenServerStillAnswers(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, dedicatedServerLogName)
	countPath := filepath.Join(dir, "probe-count")
	tmuxShimOnPath(t, "#!/bin/sh\n"+
		"n=0\nif [ -f \""+countPath+"\" ]; then n=$(cat \""+countPath+"\"); fi\n"+
		"n=$((n+1))\nprintf '%s\\n' \"$n\" > \""+countPath+"\"\n"+
		"if [ \"$1\" = \"show-options\" ]; then\n"+
		"  if [ \"$n\" -eq 1 ]; then\n"+
		"    printf '%s\\n' \"error connecting to /nonexistent (No such file or directory)\" >&2\n    exit 1\n"+
		"  fi\n  echo on\n  exit 0\nfi\n"+
		"if [ \"$1\" = \"-D\" ]; then\n"+
		"  printf '%s\\n' \"open terminal failed: not a terminal\" >&2\n  exit 1\nfi\n"+
		"exit 0\n")
	t.Setenv(dedicatedServerLogEnv, logPath)

	err := runDedicatedServer()
	if err == nil {
		t.Fatal("tmux -D attach failure was reported as success")
	}
	noServerDeathMarker(t, dir)
}

// TestDedicatedServerWrapperSkipsMarkerOnAStalledRead is the long-stall
// requirement: when the post-exit presence read does not answer, the verdict
// is non-definitive and the marker must stay unwritten.
func TestDedicatedServerWrapperSkipsMarkerOnAStalledRead(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, dedicatedServerLogName)
	tmuxShimOnPath(t, "#!/bin/sh\n"+
		"if [ \"$1\" = \"show-options\" ]; then exec sleep 15; fi\n"+
		"if [ \"$1\" = \"-D\" ]; then exit 0; fi\nexit 0\n")
	t.Setenv(dedicatedServerLogEnv, logPath)

	started := time.Now()
	if err := runDedicatedServer(); err != nil {
		t.Fatalf("clean tmux -D exit errored: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 5*time.Second {
		t.Fatalf("stalled presence read returned after %s — the bound did not engage", elapsed)
	}
	noServerDeathMarker(t, dir)
}

// TestDedicatedServerWrapperRecordsDeathOnDefinitiveAbsence pins the marker's
// surviving role: `tmux -D` exited and tmux's own diagnostic proves nobody is
// on the socket — a real death — so the record is written.
func TestDedicatedServerWrapperRecordsDeathOnDefinitiveAbsence(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, dedicatedServerLogName)
	tmuxShimOnPath(t, "#!/bin/sh\n"+
		"if [ \"$1\" = \"show-options\" ]; then\n"+
		"  printf '%s\\n' \"error connecting to /nonexistent (No such file or directory)\" >&2\n  exit 1\nfi\n"+
		"if [ \"$1\" = \"-D\" ]; then\n"+
		"  printf '%s\\n' 'fatal-sentinel-7557' >&2\n  exit 42\nfi\nexit 0\n")
	t.Setenv(dedicatedServerLogEnv, logPath)

	if err := runDedicatedServer(); err == nil {
		t.Fatal("failing tmux -D was reported as success")
	}
	death, ok, err := ReadServerDeath(dir)
	if err != nil || !ok {
		t.Fatalf("death marker not written on a definitive no-server read (ok=%v err=%v)", ok, err)
	}
	if death.ExitCode != 42 {
		t.Fatalf("death marker exit code = %d, want 42", death.ExitCode)
	}
}

// TestDedicatedServerLauncherAdoptsTheSurvivingServer walks the full
// systemd-run path: the gate reads a definitive absence, the wrapper starts,
// but a racer owns the socket so `tmux -D` attach-fails and exits — and the
// server that still answers afterward is recorded as ADOPTED, never credited
// to this scope's already-exited unit, and no death marker is written.
func TestDedicatedServerLauncherAdoptsTheSurvivingServer(t *testing.T) {
	testguard.IsolateTmux(t)
	dir := t.TempDir()
	racerUp := filepath.Join(dir, "racer-answers")
	// The wrapper's presence reads carry AF_TMUX_SERVER_LOG in their
	// environment; the launcher's do not. Until the wrapper's own post-exit
	// read has answered, every launcher-side read sees the racer's socket
	// still refuse — so a `present` answer can only reach the launcher once
	// the wrapper has definitively failed. The answer's short delay is the
	// cushion for the exit to be reaped before it is classified.
	tmuxScript := "#!/bin/sh\n" +
		"if [ \"$1\" = \"show-options\" ]; then\n" +
		"  if [ -n \"$AF_TMUX_SERVER_LOG\" ]; then\n" +
		"    touch \"" + racerUp + "\"\n  echo on\n  exit 0\n  fi\n" +
		"  if [ ! -f \"" + racerUp + "\" ]; then\n" +
		"    printf '%s\\n' \"error connecting to /nonexistent (No such file or directory)\" >&2\n    exit 1\n  fi\n" +
		"  sleep 0.1\n  echo on\n  exit 0\nfi\n" +
		"if [ \"$1\" = \"-D\" ]; then\n" +
		"  printf '%s\\n' \"open terminal failed: not a terminal\" >&2\n  exit 1\nfi\n" +
		"exit 0\n"
	systemdRunScript := `#!/bin/sh
set -eu
while [ "$#" -gt 0 ]; do
    case "$1" in
        --user|--scope|--quiet|--collect|--unit=*|--property=*) shift ;;
        --) shift; break ;;
        *) exit 64 ;;
    esac
done
exec "$@"
`
	shimDir := t.TempDir()
	for name, script := range map[string]string{"tmux": tmuxScript, "systemd-run": systemdRunScript} {
		if err := os.WriteFile(filepath.Join(shimDir, name), []byte(script), 0o700); err != nil {
			t.Fatalf("write %s shim: %v", name, err)
		}
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	home := t.TempDir()
	daemonServerTestEnv(t, home)

	if err := EnsureDaemonServer(); err != nil {
		t.Fatalf("ensure dedicated server under a socket racer: %v", err)
	}
	st, ok, err := ReadServerState(home)
	if err != nil || !ok {
		t.Fatalf("server state not recorded (ok=%v err=%v)", ok, err)
	}
	if st.Mode != ServerModeAdopted || st.Unit != "" {
		t.Fatalf("server state = %+v, want adopted with no owning unit — the answering server is not this scope's", st)
	}
	noServerDeathMarker(t, home)
}
