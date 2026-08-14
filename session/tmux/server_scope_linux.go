//go:build linux

package tmux

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/internal/systemdunit"
	"github.com/sachiniyer/agent-factory/log"
)

const (
	dedicatedServerExecMarker = "__af-tmux-server"
	dedicatedServerLogEnv     = "AF_TMUX_SERVER_LOG"
	dedicatedServerLogName    = "tmux-server.log"
	serverProbeTimeout        = 250 * time.Millisecond
	serverStartupTimeout      = 2 * time.Second
)

var (
	daemonServerConfigMu sync.RWMutex
	daemonServerHome     string
	serverBootstrapMu    sync.Mutex
)

// ConfigureDaemonServer gives the tmux launch path the AF home where the
// dedicated server wrapper keeps its bounded stderr log. RunDaemon installs
// this before restore and removes it on exit; ordinary CLI processes never
// configure a bootstrapper and retain their historical direct launch.
func ConfigureDaemonServer(home string) func() {
	if absolute, err := filepath.Abs(home); err == nil {
		home = absolute
	}
	daemonServerConfigMu.Lock()
	previous := daemonServerHome
	daemonServerHome = home
	daemonServerConfigMu.Unlock()
	return func() {
		daemonServerConfigMu.Lock()
		daemonServerHome = previous
		daemonServerConfigMu.Unlock()
	}
}

func configuredDaemonServerHome() (string, bool) {
	daemonServerConfigMu.RLock()
	defer daemonServerConfigMu.RUnlock()
	return daemonServerHome, daemonServerHome != ""
}

// EnsureDaemonServer starts tmux before any daemon-owned session spawn. The
// foreground server lives in its own named systemd scope, independent of every
// session launch and of the daemon service itself. tmux -D both preserves its
// stderr and disables exit-empty; the explicit option write below pins that
// zero-session lifetime as an observable postcondition.
func EnsureDaemonServer() error {
	if !systemdunit.RunningDaemonProcess() {
		return nil
	}
	home, configured := configuredDaemonServerHome()
	if !configured {
		return fmt.Errorf("dedicated tmux server bootstrap is not configured")
	}
	return ensureDedicatedServer(home)
}

func ensureDedicatedServer(home string) error {
	serverBootstrapMu.Lock()
	defer serverBootstrapMu.Unlock()

	if tmuxServerRunning() {
		return keepTmuxServerAlive()
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("create AF home for tmux server log: %w", err)
	}
	logPath := filepath.Join(home, dedicatedServerLogName)
	// Apply the configured bound before systemd-run gets a direct append fd. The
	// long-lived helper reopens the same path through NewRotatingFile and owns
	// write-time rotation after startup.
	if writer, err := log.NewRotatingFile(logPath, 0o600); err != nil {
		return fmt.Errorf("open bounded tmux server log: %w", err)
	} else if err := writer.Close(); err != nil {
		return fmt.Errorf("prepare bounded tmux server log: %w", err)
	}
	launchLog, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open tmux server launch log: %w", err)
	}
	cmd := dedicatedServerScopeCommand(logPath)
	cmd.Stdout = launchLog
	cmd.Stderr = launchLog
	if err := cmd.Start(); err != nil {
		_ = launchLog.Close()
		return fmt.Errorf("start dedicated tmux server scope: %w", err)
	}
	_ = launchLog.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	deadline := time.NewTimer(serverStartupTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if tmuxServerRunning() {
			return keepTmuxServerAlive()
		}
		select {
		case launchErr := <-done:
			done = nil
			if launchErr != nil {
				// A concurrent AF home may have won the same socket after this
				// launcher failed. Prefer the observed healthy server to a false
				// startup refusal.
				if tmuxServerRunning() {
					return keepTmuxServerAlive()
				}
				return fmt.Errorf("dedicated tmux server scope exited before readiness: %w", launchErr)
			}
		case <-ticker.C:
		case <-deadline.C:
			return fmt.Errorf("dedicated tmux server did not become ready within %s", serverStartupTimeout)
		}
	}
}

func tmuxServerRunning() bool {
	ctx, cancel := context.WithTimeout(context.Background(), serverProbeTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "tmux", "show-options", "-gv", "exit-empty").Run() == nil
}

func keepTmuxServerAlive() error {
	ctx, cancel := context.WithTimeout(context.Background(), serverProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", "set-option", "-g", "exit-empty", "off").CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail != "" {
			return fmt.Errorf("set tmux exit-empty off: %w: %s", err, detail)
		}
		return fmt.Errorf("set tmux exit-empty off: %w", err)
	}
	return nil
}

func dedicatedServerScopeCommand(logPath string) *exec.Cmd {
	// /proc/<pid>/exe avoids systemd-run's ${NAME} argv expansion without
	// requiring a new-systemd-only flag. The daemon remains alive throughout
	// this readiness barrier, so the proc link is stable until the helper execs.
	executable := fmt.Sprintf("/proc/%d/exe", os.Getpid())
	args := []string{
		"--user", "--scope", "--quiet", "--collect",
		"--unit=" + dedicatedServerScopeName(),
		"--property=KillMode=control-group",
		"--", executable, dedicatedServerExecMarker,
	}
	cmd := exec.Command("systemd-run", args...)
	// Match the old first-session launch's credential boundary: a new tmux
	// server must not snapshot the daemon's ambient secrets into its global
	// environment. Later session clients import only their approved names.
	serverEnv := sessionenv.Filter(os.Environ(), "", nil)
	cmd.Env = replaceEnvironment(serverEnv, dedicatedServerLogEnv, logPath)
	return cmd
}

func dedicatedServerScopeName() string {
	identity := os.Getenv("TMUX")
	if socket, _, ok := strings.Cut(identity, ","); ok {
		identity = socket
	}
	if identity == "" {
		base := os.Getenv("TMUX_TMPDIR")
		if base == "" {
			base = "/tmp"
		}
		identity = fmt.Sprintf("%s/tmux-%d/default", base, os.Getuid())
	}
	sum := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("agent-factory-tmux-server-%x", sum[:6])
}

func replaceEnvironment(environ []string, name, value string) []string {
	prefix := name + "="
	out := make([]string, 0, len(environ)+1)
	for _, entry := range environ {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return append(out, prefix+value)
}

func withoutEnvironment(environ []string, name string) []string {
	prefix := name + "="
	out := make([]string, 0, len(environ))
	for _, entry := range environ {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return out
}

// HandleDedicatedServerExec consumes the private wrapper invocation before
// Cobra. It stays alive beside foreground tmux, feeding stdout/stderr into the
// same bounded rotation policy as Agent Factory's other long-lived logs.
func HandleDedicatedServerExec() {
	if len(os.Args) != 2 || os.Args[1] != dedicatedServerExecMarker {
		return
	}
	if err := runDedicatedServer(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "af: dedicated tmux server exited: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func runDedicatedServer() error {
	logPath := os.Getenv(dedicatedServerLogEnv)
	if logPath == "" {
		return fmt.Errorf("tmux server log path is missing")
	}
	writer, err := log.NewRotatingFile(logPath, 0o600)
	if err != nil {
		return fmt.Errorf("open bounded tmux server log: %w", err)
	}
	defer writer.Close()
	_, _ = fmt.Fprintf(writer, "%s af tmux server starting in foreground\n", time.Now().Format(time.RFC3339))

	cmd := exec.Command("tmux", "-D")
	cmd.Env = withoutEnvironment(os.Environ(), dedicatedServerLogEnv)
	cmd.Stdout = writer
	cmd.Stderr = writer
	err = cmd.Run()
	if err != nil {
		_, _ = fmt.Fprintf(writer, "%s af tmux server exited: %v\n", time.Now().Format(time.RFC3339), err)
		return err
	}
	_, _ = fmt.Fprintf(writer, "%s af tmux server exited cleanly\n", time.Now().Format(time.RFC3339))
	return nil
}

// newTmuxServerCommand keeps the historical per-session scope as a fail-open
// fallback only. Once the daemon observes or creates the shared server, this
// new-session is a plain client and therefore cannot own the server's cgroup.
func newTmuxServerCommand(args ...string) (*exec.Cmd, bool) {
	if !systemdunit.RunningDaemonProcess() {
		return exec.Command("tmux", args...), false
	}
	if _, configured := configuredDaemonServerHome(); configured {
		if err := EnsureDaemonServer(); err == nil {
			return exec.Command("tmux", args...), false
		} else {
			log.WarningLog.Printf("dedicated tmux server launch failed; falling back to a per-session systemd scope: %v", err)
		}
	}

	scopeArgs := []string{"--user", "--scope", "--quiet", "--collect", "--", "tmux"}
	scopeArgs = append(scopeArgs, args...)
	return exec.Command("systemd-run", scopeArgs...), true
}
