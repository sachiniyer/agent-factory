package daemon

import (
	"encoding/json"
	"fmt"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// RunDaemon runs the daemon process, serves the local control plane, and
// iterates over all sessions to run AutoYes mode on them.
func RunDaemon(cfg *config.Config) error {
	log.InfoLog.Printf("starting daemon")
	manager, err := NewManager(cfg)
	if err != nil {
		return err
	}
	shutdownCh := make(chan struct{})
	closeControl, err := startControlServer(manager, shutdownCh)
	if err != nil {
		return fmt.Errorf("failed to start daemon control server: %w", err)
	}
	defer func() {
		if err := closeControl(); err != nil {
			log.WarningLog.Printf("failed to close daemon control socket: %v", err)
		}
	}()

	// Write our PID so `af upgrade`'s SIGTERM fallback (#504) can find the
	// running daemon. Both the SIGTERM and Shutdown-RPC exit paths fall
	// through to the deferred cleanup, so the file is removed on any graceful
	// shutdown. A stale file is harmless — readers verify the live process's
	// cmdline before signaling it.
	if err := writeDaemonPIDFile(); err != nil {
		log.WarningLog.Printf("failed to write daemon PID file: %v", err)
	} else {
		defer removeDaemonPIDFile()
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
			if err := manager.RefreshInstances(); err != nil {
				log.WarningLog.Printf("failed to refresh daemon instances: %v", err)
			}

			for _, instance := range manager.InstancesSnapshot() {
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

	// Notify on SIGINT (Ctrl+C) and SIGTERM, and watch for a Shutdown RPC.
	// The RPC path is used by `af upgrade` / autoUpdate after writing a new
	// binary so the next RPC respawns the daemon from the fresh image (#498).
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-sigChan:
		log.InfoLog.Printf("received signal %s", sig.String())
	case <-shutdownCh:
		log.InfoLog.Printf("received shutdown request via control socket")
	}

	// Stop the goroutine so we don't race.
	close(stopCh)
	wg.Wait()

	if err := manager.SaveInstances(); err != nil {
		log.ErrorLog.Printf("failed to save instances when terminating daemon: %v", err)
	}
	return nil
}

// fromInstanceDataForRefresh is the entry point refreshDaemonInstances uses
// to materialize a session.Instance from a persisted on-disk entry. It is a
// package-level variable so tests can observe (or substitute) the call —
// see TestManagerCreateSessionAtomicWithRefresh, which uses it to detect
// whether refresh ever raced CreateSession and tried to construct a
// duplicate Instance from disk.
var fromInstanceDataForRefresh = session.FromInstanceData

func refreshDaemonInstances(existing map[string]*session.Instance) (map[string]*session.Instance, error) {
	allInstances, err := config.LoadAllRepoInstances()
	if err != nil {
		return existing, err
	}

	next := make(map[string]*session.Instance)
	for repoID, raw := range allInstances {
		if raw == nil || string(raw) == "[]" || string(raw) == "null" {
			continue
		}

		var data []session.InstanceData
		if err := json.Unmarshal(raw, &data); err != nil {
			return existing, fmt.Errorf("failed to parse instances for repo %s: %w", repoID, err)
		}

		for _, item := range data {
			key := daemonInstanceKey(repoID, item.Title)
			if existing != nil {
				if instance := existing[key]; instance != nil {
					next[key] = instance
					continue
				}
			}

			instance, err := fromInstanceDataForRefresh(item)
			if err != nil {
				log.WarningLog.Printf("daemon skipping instance %q: %v", item.Title, err)
				continue
			}
			// Assume AutoYes is true if the daemon is running.
			instance.SetAutoYes(true)
			next[key] = instance
		}
	}

	return next, nil
}

func daemonInstanceKey(repoID, title string) string {
	return repoID + "\x00" + title
}

func daemonInstances(instanceMap map[string]*session.Instance) []*session.Instance {
	keys := make([]string, 0, len(instanceMap))
	for key := range instanceMap {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	instances := make([]*session.Instance, 0, len(keys))
	for _, key := range keys {
		instances = append(instances, instanceMap[key])
	}
	return instances
}

// LaunchDaemon launches the daemon process if it is not already serving the
// local control plane.
func LaunchDaemon() error {
	return EnsureDaemon()
}

func launchDaemonProcess() error {
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

	// The child writes its own PID file from RunDaemon (#504). Don't wait for
	// the child to exit, it's detached.
	return nil
}

// daemonPIDFilePath returns the path to the daemon PID file, or "" if the
// config dir cannot be resolved.
func daemonPIDFilePath() (string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.pid"), nil
}

// writeDaemonPIDFile atomically writes the current process's PID to the daemon
// PID file with mode 0600. Used by RunDaemon so callers (StopDaemon, the
// SIGTERM fallback in RequestShutdown) can locate and signal this daemon.
func writeDaemonPIDFile() error {
	path, err := daemonPIDFilePath()
	if err != nil {
		return err
	}
	return config.AtomicWriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0600)
}

// removeDaemonPIDFile deletes the daemon PID file. Best-effort: an ENOENT is
// already harmless (a stale file is fine — readers verify cmdline) and
// permission errors only occur in pathological setups. Logs at warning level
// rather than failing the daemon teardown.
func removeDaemonPIDFile() {
	path, err := daemonPIDFilePath()
	if err != nil {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.WarningLog.Printf("failed to remove daemon PID file: %v", err)
	}
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
	if socketPath, socketErr := DaemonSocketPath(); socketErr == nil {
		_ = os.Remove(socketPath)
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
