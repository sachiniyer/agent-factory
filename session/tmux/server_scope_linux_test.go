//go:build linux

package tmux

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/systemdunit"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

func TestNewTmuxServerCommandScopesDaemonUnitSpawn(t *testing.T) {
	t.Setenv(systemdunit.DaemonMarkerEnv, systemdunit.DaemonUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))

	cmd, scoped := newTmuxServerCommand("new-session", "-d", "-s", "af_worker")
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

	cmd, scoped := newTmuxServerCommand("new-session", "-d", "-s", "af_worker")
	if scoped {
		t.Fatal("session client was put in a transient scope even though the shared server already exists")
	}
	want := "tmux new-session -d -s af_worker"
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

func TestNewTmuxServerCommandDoesNotTrustInheritedSystemdMarker(t *testing.T) {
	t.Setenv(systemdunit.DaemonMarkerEnv, systemdunit.DaemonUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()+1))

	cmd, scoped := newTmuxServerCommand("new-session")
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
	cmd := dedicatedServerScopeCommand(logPath)
	got := strings.Join(cmd.Args, " ")
	for _, want := range []string{
		"systemd-run --user --scope --quiet --collect",
		"--unit=agent-factory-tmux-server-",
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
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >\"$AF_TEST_TMUX_ARGS\"\nprintf '%s\\n' 'fatal-sentinel-3307' >&2\nexit 42\n"
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

	cmd, scoped := newTmuxServerCommand("new-session", "-d", "-s", "af_worker")
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
	stopScript := "#!/bin/sh\nprintf '%s\\n' \"$*\" >\"$0.args\"\nbase=$(dirname \"$0\")/systemd-run\nfor suffix in child parent; do\n  pid=$(cat \"$base.$suffix\")\n  kill -KILL \"$pid\" 2>/dev/null || true\ndone\n"
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
	wantStopArgs := "--user kill --kill-whom=all --signal=KILL " + dedicatedServerScopeName() + ".scope"
	if got := strings.TrimSpace(string(stopArgs)); got != wantStopArgs {
		t.Fatalf("systemctl cleanup args = %q, want %q", got, wantStopArgs)
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
		if err := syscall.Kill(pid, 0); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Errorf("timed-out dedicated server %s %d is still alive", label, pid)
		} else if err != syscall.ESRCH {
			t.Errorf("probe timed-out %s %d: %v", label, pid, err)
		}
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
while [ ! -f "$AF_TEST_TMUX_RELEASE" ]; do sleep 0.01; done
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

	cmd, scoped := newTmuxServerCommand("new-session", "-d", "-s", "af_worker", "sleep", "60")
	if scoped {
		t.Fatal("session spawn still owned a scope after the dedicated server became ready")
	}
	if got := strings.Join(cmd.Args, " "); got != "tmux new-session -d -s af_worker sleep 60" {
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

func (refusingTrackedPtyFactory) Close() {}

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
