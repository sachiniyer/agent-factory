package daemon

import (
	"fmt"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// RunDaemon runs the daemon process which iterates over all sessions and runs AutoYes mode on them.
// It's expected that the main process kills the daemon when the main process starts.
func RunDaemon(cfg *config.Config) error {
	log.InfoLog.Printf("starting daemon")
	state := config.LoadState()
	storage, err := session.NewStorage(state, "")
	if err != nil {
		return fmt.Errorf("failed to initialize storage: %w", err)
	}

	instances, err := storage.LoadInstances()
	if err != nil {
		return fmt.Errorf("failed to load instances: %w", err)
	}
	for _, instance := range instances {
		// Assume AutoYes is true if the daemon is running.
		instance.AutoYes = true
	}

	pollInterval := time.Duration(cfg.DaemonPollInterval) * time.Millisecond

	wg := &sync.WaitGroup{}
	wg.Add(1)
	stopCh := make(chan struct{})
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			for _, instance := range instances {
				// We only store started instances, but check anyway.
				if instance.Started() {
					if _, hasPrompt := instance.HasUpdated(); hasPrompt {
						instance.TapEnter()
					}
				}
			}

			// Handle stop before ticker.
			select {
			case <-stopCh:
				return
			case <-ticker.C:
			}
		}
	}()

	// Notify on SIGINT (Ctrl+C) and SIGTERM. Save instances before
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigChan
	log.InfoLog.Printf("received signal %s", sig.String())

	// Stop the goroutine so we don't race.
	close(stopCh)
	wg.Wait()

	if err := storage.SaveInstances(instances); err != nil {
		log.ErrorLog.Printf("failed to save instances when terminating daemon: %v", err)
	}
	return nil
}

// LaunchDaemon launches the daemon process.
func LaunchDaemon() error {
	// Stop any existing daemon first to prevent duplicates.
	if err := StopDaemon(); err != nil {
		log.ErrorLog.Printf("failed to stop existing daemon: %v", err)
		// Continue anyway — best effort
	}

	// Find the agent-factory binary.
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}

	cmd := exec.Command(execPath, "--daemon")

	// Detach the process from the parent
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	// Set process group to prevent signals from propagating
	cmd.SysProcAttr = getSysProcAttr()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start child process: %w", err)
	}

	log.InfoLog.Printf("started daemon child process with PID: %d", cmd.Process.Pid)

	// Save PID to a file for later management
	pidDir, err := config.GetConfigDir()
	if err != nil {
		return fmt.Errorf("failed to get config directory: %w", err)
	}

	pidFile := filepath.Join(pidDir, "daemon.pid")
	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", cmd.Process.Pid)), 0644); err != nil {
		return fmt.Errorf("failed to write PID file: %w", err)
	}

	// Don't wait for the child to exit, it's detached
	return nil
}

// StopDaemon attempts to stop a running daemon process if it exists. Returns no error if the daemon is not found
// (assumes the daemon does not exist). It verifies the PID actually belongs to an agent-factory daemon before
// sending a kill signal, so a stale or reused PID in the PID file can't take down an unrelated process.
func StopDaemon() error {
	pidDir, err := config.GetConfigDir()
	if err != nil {
		return fmt.Errorf("failed to get config directory: %w", err)
	}

	pidFile := filepath.Join(pidDir, "daemon.pid")
	data, err := os.ReadFile(pidFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read PID file: %w", err)
	}

	var pid int
	if _, err := fmt.Sscanf(string(data), "%d", &pid); err != nil {
		return fmt.Errorf("invalid PID file format: %w", err)
	}

	// Defensively refuse to kill our own process or obviously invalid PIDs.
	if pid <= 1 || pid == os.Getpid() {
		log.InfoLog.Printf("daemon PID file contained invalid PID %d; removing stale file", pid)
		_ = os.Remove(pidFile)
		return nil
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		// On unix, FindProcess never returns an error, but handle it defensively anyway.
		log.InfoLog.Printf("daemon process (PID: %d) not found; removing stale PID file", pid)
		_ = os.Remove(pidFile)
		return nil
	}

	// Check the process exists at all. Signal 0 is a no-op that just validates permissions/existence.
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		log.InfoLog.Printf("daemon process (PID: %d) is not running (%v); removing stale PID file", pid, err)
		_ = os.Remove(pidFile)
		return nil
	}

	// Verify the process is actually an agent-factory daemon before killing it. If we can't verify,
	// err on the side of caution and treat the PID file as stale rather than killing a random process.
	if !isAgentFactoryDaemon(pid) {
		log.InfoLog.Printf("PID %d does not look like an agent-factory daemon; removing stale PID file", pid)
		_ = os.Remove(pidFile)
		return nil
	}

	if err := proc.Kill(); err != nil {
		return fmt.Errorf("failed to stop daemon process: %w", err)
	}

	// Clean up PID file
	if err := os.Remove(pidFile); err != nil {
		return fmt.Errorf("failed to remove PID file: %w", err)
	}

	log.InfoLog.Printf("daemon process (PID: %d) stopped successfully", pid)
	return nil
}

// isAgentFactoryDaemon checks whether the process at pid looks like an agent-factory daemon
// (i.e. its command line contains the --daemon flag as a discrete argument). It tries
// /proc/<pid>/cmdline first (Linux) and falls back to `ps -p <pid> -o args=` (macOS and other
// unixes). If neither source yields a readable command line, returns false so callers treat the
// PID as unverified.
//
// We split the command line on whitespace and require an exact "--daemon" token (or a
// "--daemon=..." form), so flags like --daemonize don't get matched as substrings.
func isAgentFactoryDaemon(pid int) bool {
	cmdline := readProcCmdline(pid)
	if cmdline == "" {
		cmdline = readPsArgs(pid)
	}
	if cmdline == "" {
		return false
	}
	return cmdlineHasDaemonFlag(cmdline)
}

// cmdlineHasDaemonFlag reports whether cmdline contains "--daemon" as a discrete argument
// (either bare or in the "--daemon=value" form). It deliberately rejects substring matches like
// "--daemonize" or "--daemon-mode".
func cmdlineHasDaemonFlag(cmdline string) bool {
	for _, field := range strings.Fields(cmdline) {
		if field == "--daemon" || strings.HasPrefix(field, "--daemon=") {
			return true
		}
	}
	return false
}

// readProcCmdline returns the full command line for pid via /proc/<pid>/cmdline (Linux).
// Returns "" if /proc is unavailable or the file cannot be read.
func readProcCmdline(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return ""
	}
	// /proc/<pid>/cmdline separates args with NUL bytes.
	return strings.ReplaceAll(strings.TrimRight(string(data), "\x00"), "\x00", " ")
}

// readPsArgs returns the full command line for pid via `ps -p <pid> -o args=`. This flag set is
// portable across Linux and macOS.
func readPsArgs(pid int) string {
	psPath, err := exec.LookPath("ps")
	if err != nil {
		return ""
	}
	out, err := exec.Command(psPath, "-p", fmt.Sprintf("%d", pid), "-o", "args=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
