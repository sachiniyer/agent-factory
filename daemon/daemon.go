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

// restoreManagerForStartup is the warm-up restore entry point RunDaemon uses.
// Package-level so tests can inject a slow or gated restore and prove the
// control socket binds and serves before the restore completes (#829).
var restoreManagerForStartup = func(m *Manager) error { return m.RestoreInstances() }

// RunDaemon runs the daemon process: it serves the local control plane,
// evaluates task cron schedules in-process, supervises watch-task scripts,
// and iterates over all sessions each poll to compute their authoritative
// status (Ready/Dead/Running, #935/#960 PR 5) and run AutoYes mode on them.
//
// Startup ordering matters (#829): the control socket binds BEFORE the
// instance restore, which can take minutes on remote-hook repos (list_cmd /
// ssh per session). Pre-#829 the restore ran first, so every concurrent
// EnsureDaemon found no socket and spawned another daemon that burned a full
// restore before losing the bind race. During the warm-up window Ping and
// Shutdown work and state-dependent RPCs return errDaemonStarting; the
// scheduler, watcher supervisor, and AutoYes poll loop start only after the
// restore because they act on restored state.
func RunDaemon(cfg *config.Config) error {
	log.InfoLog.Printf("starting daemon")

	// Refuse to run two daemons against the same control socket. EnsureDaemon
	// pings before launching, but a daemon started directly (af --daemon, the
	// autostart unit, or two racing af invocations) would otherwise steal the
	// socket from a live daemon and leave duplicate AutoYes/scheduler loops.
	// Exiting cleanly matters: under the autostart unit a non-zero exit would
	// trip Restart=on-failure into a retry loop against the live daemon.
	if err := pingDaemon(); err == nil {
		log.InfoLog.Printf("another agent-factory daemon is already serving the control socket; exiting")
		return nil
	}

	// Shell only — no restore yet, so the bind below happens within
	// milliseconds of process start.
	manager, err := newManagerShell(cfg)
	if err != nil {
		return err
	}

	scheduler := newTaskScheduler()
	watchers := newWatcherSupervisor()

	shutdownCh := make(chan struct{})
	closeControl, alreadyRunning, err := bindControlServerExclusive(manager, scheduler, watchers, shutdownCh)
	if err != nil {
		return fmt.Errorf("failed to start daemon control server: %w", err)
	}
	if alreadyRunning {
		// A concurrent daemon won the ping→bind race while we were setting
		// up: both of us passed the unsynchronized ping above before either
		// bound (#718). Exit cleanly for the same Restart=on-failure reason
		// as the guard at the top of this function.
		log.InfoLog.Printf("another agent-factory daemon bound the control socket first; exiting")
		return nil
	}
	defer func() {
		if err := closeControl(); err != nil {
			log.WarningLog.Printf("failed to close daemon control socket: %v", err)
		}
	}()

	// Start the HTTP/JSON mirror alongside the control socket (#1029 PR 4). It
	// shares this daemon's live manager, so HTTP is just another thin client of
	// the same core. Only the winner of bindControlServerExclusive reaches this
	// point, so no extra spawn race applies. A bind failure is logged but never
	// fatal: HTTP is auxiliary — the gob control plane every existing client
	// depends on must not regress if the HTTP socket cannot bind.
	if closeHTTP, err := startHTTPServer(manager, scheduler, watchers); err != nil {
		log.WarningLog.Printf("failed to start daemon HTTP server: %v", err)
	} else {
		defer func() {
			if err := closeHTTP(); err != nil {
				log.WarningLog.Printf("failed to close daemon HTTP server: %v", err)
			}
		}()
	}

	// Write our PID as soon as the socket is bound so `af upgrade`'s SIGTERM
	// fallback (#504) and StopDaemon can find a still-warming daemon. Both
	// the SIGTERM and Shutdown-RPC exit paths fall through to the deferred
	// cleanup, so the file is removed on any graceful shutdown. A stale file
	// is harmless — readers verify the live process's cmdline before
	// signaling it.
	if err := writeDaemonPIDFile(); err != nil {
		log.WarningLog.Printf("failed to write daemon PID file: %v", err)
	} else {
		defer removeDaemonPIDFile()
	}

	// Notify on SIGINT (Ctrl+C) and SIGTERM, and watch for a Shutdown RPC.
	// The RPC path is used by `af upgrade` / autoUpdate after writing a new
	// binary so the next RPC respawns the daemon from the fresh image (#498).
	// Registered before the restore so both exit paths work during warm-up.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Run the restore concurrently so a shutdown or signal during warm-up
	// exits promptly instead of hanging behind minutes of list_cmd/ssh. The
	// warm-up exit paths deliberately skip SaveInstances: nothing has been
	// restored, and saving the empty instance map would wipe every persisted
	// session.
	log.InfoLog.Printf("control socket bound; restoring instances")
	restoreDone := make(chan error, 1)
	// Capture the seam on the main flow: reading the package var inside the
	// goroutine would race with tests restoring it after RunDaemon returns.
	restore := restoreManagerForStartup
	go func() { restoreDone <- restore(manager) }()
	select {
	case restoreErr := <-restoreDone:
		if restoreErr != nil {
			// Same outcome as a pre-#829 NewManager failure: exit non-zero
			// and let the autostart unit's Restart=on-failure retry.
			return fmt.Errorf("failed to restore instances: %w", restoreErr)
		}
	case sig := <-sigChan:
		log.InfoLog.Printf("received signal %s during instance restore; exiting", sig.String())
		return nil
	case <-shutdownCh:
		log.InfoLog.Printf("received shutdown request via control socket during instance restore; exiting")
		return nil
	}
	log.InfoLog.Printf("instance restore complete; daemon ready")

	// Remove per-task timer units left behind by pre-#782 versions; the
	// in-process scheduler below replaces them.
	sweepLegacyTaskUnits()

	// Start schedule evaluation only after the control server is up and the
	// restore has finished: a task firing immediately goes through the
	// CreateSession RPC on our own socket, which requires a ready manager.
	if err := scheduler.Reload(); err != nil {
		log.WarningLog.Printf("failed to load task schedules: %v", err)
	}
	scheduler.Start()
	defer scheduler.Stop()

	// Same ordering constraint for the watch-task supervisor: its event
	// deliveries also loop back through our own control socket, so the first
	// watcher spawns only once the server is accepting. The deferred Stop
	// runs before the deferred closeControl (LIFO), so in-flight deliveries
	// during shutdown still find a live socket.
	if err := watchers.Reload(); err != nil {
		log.WarningLog.Printf("failed to start task watchers: %v", err)
	}
	defer watchers.Stop()

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

			// Compute and persist each session's status (Ready/Dead/Running) and
			// run the AutoYes prompt-tap in the same pass. The daemon is the sole
			// owner of status now (#935/#960 PR 5): it computes the liveness here
			// and the TUI renders it from Snapshot instead of computing its own.
			manager.RefreshStatuses()

			// Always-ensure the root agent for repos opted in via root_agents
			// (#1106). Runs after RefreshStatuses so a root whose tmux died is
			// marked Dead and healed in the same tick; the loop body runs once
			// before the first ticker wait, so the first ensure happens right
			// after the restore. A (re-)create blocks this poll briefly while
			// the session starts — acceptable for a rare, backoff-throttled
			// event. root_agents is read from the daemon's startup config;
			// changing it takes effect on the next daemon restart.
			manager.EnsureRootAgents()

			// Best-effort restore of Lost sessions (#1108): the general form
			// of the root self-heal, for every session whose tmux vanished
			// with no kill on record. Runs after RefreshStatuses (which marks
			// them Lost) and after EnsureRootAgents (which owns the reserved
			// root title). Backoff-throttled per session, like root-ensure.
			manager.RestoreLostSessions()

			// Opt-in auto-resume of usage-limit-blocked sessions (#1146 PR3):
			// re-prompt a LiveLimitReached row once its limit window elapsed. A
			// no-op unless limit_auto_resume is set, so a default install keeps
			// a limit surface-only. Runs after RestoreLostSessions because a
			// session must be settled onto its liveness first; it borrows the
			// same per-session op-lock discipline.
			manager.ResumeLimitedSessions()

			// Handle stop before ticker.
			select {
			case <-stopCh:
				return
			case <-ticker.C:
			}
		}
	}()

	// Watch our own AF home directory (the dir holding tasks.json, the
	// control socket, and state). If it is deleted out from under us — an
	// abandoned temp/test home, or a user rm -rf'ing the install — nothing
	// can reach this daemon via the control plane anymore, yet it would keep
	// firing cron schedules forever (#1093: a leaked debug daemon spawned a
	// session nightly for 23 days). Self-terminating is the only safe move.
	homeGoneCh := make(chan struct{})
	if homeDir, homeErr := config.GetConfigDir(); homeErr != nil {
		// Without a resolvable home there is nothing to watch; the daemon
		// could not have started its manager against one either, so this is
		// effectively unreachable — log and run without the self-check.
		log.WarningLog.Printf("cannot resolve agent-factory home for the abandoned-daemon self-check: %v", homeErr)
	} else {
		wg.Add(1)
		go func() {
			defer wg.Done()
			watchDaemonHome(homeDir, stopCh, homeGoneCh)
		}()
	}

	// Block until a signal, a Shutdown RPC, or the home-deleted self-check
	// ends the daemon (sigChan and shutdownCh were armed before the restore
	// above).
	homeGone := false
	select {
	case sig := <-sigChan:
		log.InfoLog.Printf("received signal %s", sig.String())
	case <-shutdownCh:
		log.InfoLog.Printf("received shutdown request via control socket")
	case <-homeGoneCh:
		homeGone = true
	}

	// Stop the goroutines so we don't race.
	close(stopCh)
	wg.Wait()

	if homeGone {
		// Skip the final save: the home directory was deleted out from under
		// us, so there is no installation left to persist into — saving would
		// recreate a skeleton of the deleted home and resurrect the abandoned
		// state the deletion was meant to remove.
		return nil
	}

	if err := manager.SaveInstances(); err != nil {
		log.ErrorLog.Printf("failed to save instances when terminating daemon: %v", err)
	}
	return nil
}

// homeCheckInterval is how often watchDaemonHome verifies the daemon's own AF
// home directory still exists. A package var so tests can shorten it.
var homeCheckInterval = 60 * time.Second

// homeMissingChecksToExit is how many consecutive missing observations
// watchDaemonHome requires before declaring the home deleted. Requiring two
// keeps a single transient stat blip from taking down a healthy daemon.
const homeMissingChecksToExit = 2

// watchDaemonHome periodically stats homeDir and closes homeGone once the
// directory has been missing for homeMissingChecksToExit consecutive checks,
// signaling RunDaemon to shut down (#1093). Only a definite ENOENT counts as
// missing: any other stat error (EACCES, EIO) leaves the directory's fate
// unknown, and a false-positive shutdown of a healthy daemon is worse than
// letting an abandoned one linger until the next check. The daemon's binary
// path is deliberately NOT checked — upgrades replace the binary while the
// daemon legitimately keeps running.
func watchDaemonHome(homeDir string, stopCh <-chan struct{}, homeGone chan<- struct{}) {
	ticker := time.NewTicker(homeCheckInterval)
	defer ticker.Stop()
	misses := 0
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
		}
		var exit bool
		if misses, exit = applyHomeCheck(homeDir, misses); exit {
			log.WarningLog.Printf("agent-factory home %s no longer exists; shutting down abandoned daemon", homeDir)
			close(homeGone)
			return
		}
	}
}

// applyHomeCheck folds one stat of homeDir into the running consecutive-miss
// counter and reports whether the exit threshold was reached. A present home
// (or an indeterminate stat error) resets the counter — only an unbroken run
// of definite ENOENTs counts as a deletion.
func applyHomeCheck(homeDir string, misses int) (int, bool) {
	if _, err := os.Stat(homeDir); err == nil || !os.IsNotExist(err) {
		return 0, false
	}
	misses++
	return misses, misses >= homeMissingChecksToExit
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
			// Skip corrupted per-repo JSON instead of failing the whole
			// refresh (#603). At startup (existing==nil) a single corrupt
			// file used to abort NewManager and orphan every AutoYes
			// session across every repo. On the polling path we also
			// re-hydrate this repo's prior in-memory instances so a
			// transient/persistent corruption doesn't silently drop
			// already-running sessions — matching the pre-fix semantics
			// of returning `existing` on parse failure.
			log.WarningLog.Printf("daemon skipping repo %s: corrupted instances.json: %v", repoID, err)
			if existing != nil {
				keyPrefix := repoID + "\x00"
				for key, inst := range existing {
					if strings.HasPrefix(key, keyPrefix) {
						next[key] = inst
					}
				}
			}
			continue
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

	// Preserve in-memory instances whose repo directory vanished from disk
	// entirely (#736). LoadAllRepoInstances only returns repos that still have
	// an on-disk instances directory, so an externally-deleted repo dir is
	// simply absent from allInstances and would otherwise be dropped from
	// `next`. This is a recoverable disk inconsistency — SaveInstances recreates
	// missing repo directories — so we re-hydrate the prior instances and log
	// loudly rather than silently abandoning running AutoYes sessions. This
	// parallels the corrupted-JSON handling above, which also re-hydrates from
	// `existing`. On startup (existing == nil) there is nothing to preserve.
	if existing != nil {
		warnedRepos := make(map[string]bool)
		for key, inst := range existing {
			repoID, _ := splitDaemonInstanceKey(key)
			if _, ok := allInstances[repoID]; ok {
				continue
			}
			if !warnedRepos[repoID] {
				log.WarningLog.Printf("daemon preserving in-memory instances for missing repo directory: %s", repoID)
				warnedRepos[repoID] = true
			}
			next[key] = inst
		}
	}

	return next, nil
}

func daemonInstanceKey(repoID, title string) string {
	return repoID + "\x00" + title
}

// splitDaemonInstanceKey is the inverse of daemonInstanceKey: it splits a
// "<repoID>\x00<title>" key back into (repoID, title). A key with no NUL
// separator (unexpected) is returned as ("", key).
func splitDaemonInstanceKey(key string) (string, string) {
	for i := 0; i < len(key); i++ {
		if key[i] == 0 {
			return key[:i], key[i+1:]
		}
	}
	return "", key
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

// launchDaemonProcessFn is the spawn entry point EnsureDaemon uses.
// Package-level so tests can record or suppress real daemon spawns and prove
// a bound-but-warming daemon is treated as running, never respawned (#829).
var launchDaemonProcessFn = launchDaemonProcess

func launchDaemonProcess() error {
	// Find the agent-factory binary.
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %w", err)
	}

	pid, err := startDaemonChild(execPath)
	if err != nil {
		return err
	}

	log.InfoLog.Printf("started daemon child process with PID: %d", pid)

	// The child writes its own PID file from RunDaemon (#504).
	return nil
}

// startDaemonChild starts execPath --daemon detached from the parent and
// returns its PID. Split from launchDaemonProcess so tests can spawn a
// short-lived stub instead of re-executing the real binary with --daemon.
func startDaemonChild(execPath string) (int, error) {
	cmd := exec.Command(execPath, "--daemon")

	// Detach the process from the parent
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	// Set process group to prevent signals from propagating
	cmd.SysProcAttr = getSysProcAttr()

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("failed to start child process: %w", err)
	}

	// Setsid detaches the child's session but the kernel still parents it
	// here, so it must be reaped or each exited daemon lingers as a zombie
	// for the life of the TUI — one per upgrade/respawn cycle (#816). Same
	// pattern as session/tmux/pty.go.
	go func() {
		_ = cmd.Wait()
	}()

	return cmd.Process.Pid, nil
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

// stopDaemonGrace bounds how long StopDaemon waits for a SIGTERM'd daemon to
// exit before escalating to SIGKILL. stopDaemonPoll is the polling cadence.
// Package vars rather than constants so tests can shorten them. Production
// defaults mirror sigtermFallbackGrace / sigtermFallbackPoll — the same
// timings already used by signalAndWait on the upgrade fallback path.
var (
	stopDaemonGrace = sigtermFallbackGrace
	stopDaemonPoll  = sigtermFallbackPoll
)

// StopDaemon attempts to stop a running daemon process if it exists. The bool
// return reports whether a live agent-factory daemon was actually signaled: it
// is false (with a nil error) when there was nothing to stop — no PID file, an
// invalid/stale PID, a dead process, or a PID that doesn't look like an
// agent-factory daemon. Callers that surface a user-facing "stopped" message
// must gate on it (#937): a daemon predating the PID file (pre-1.0.69) leaves
// no daemon.pid, so a true success line here would be a lie. It verifies the
// PID actually belongs to an agent-factory daemon before signaling it, so a
// stale or reused PID in the PID file can't take down an unrelated process.
//
// Shutdown is graceful by default: SIGTERM gives the daemon's signal handler a
// chance to run SaveInstances() and clean up the PID file (see RunDaemon). We
// only escalate to SIGKILL if the daemon does not exit within stopDaemonGrace,
// matching the SIGTERM-first pattern in signalAndWait (#571).
func StopDaemon() (bool, error) {
	pidDir, err := config.GetConfigDir()
	if err != nil {
		return false, fmt.Errorf("failed to get config directory: %w", err)
	}

	pidFile := filepath.Join(pidDir, "daemon.pid")
	data, err := os.ReadFile(pidFile)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to read PID file: %w", err)
	}

	var pid int
	if _, err := fmt.Sscanf(string(data), "%d", &pid); err != nil {
		return false, fmt.Errorf("invalid PID file format: %w", err)
	}

	// Defensively refuse to kill our own process or obviously invalid PIDs.
	if pid <= 1 || pid == os.Getpid() {
		log.InfoLog.Printf("daemon PID file contained invalid PID %d; removing stale file", pid)
		_ = os.Remove(pidFile)
		return false, nil
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		// On unix, FindProcess never returns an error, but handle it defensively anyway.
		log.InfoLog.Printf("daemon process (PID: %d) not found; removing stale PID file", pid)
		_ = os.Remove(pidFile)
		return false, nil
	}

	// Check the process exists at all. Signal 0 is a no-op that just validates permissions/existence.
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		log.InfoLog.Printf("daemon process (PID: %d) is not running (%v); removing stale PID file", pid, err)
		_ = os.Remove(pidFile)
		return false, nil
	}

	// Verify the process is actually an agent-factory daemon before signaling it. If we can't verify,
	// err on the side of caution and treat the PID file as stale rather than signaling a random process.
	if !isAgentFactoryDaemon(pid) {
		log.InfoLog.Printf("PID %d does not look like an agent-factory daemon; removing stale PID file", pid)
		_ = os.Remove(pidFile)
		return false, nil
	}

	// Send SIGTERM so the daemon's signal handler can SaveInstances() before
	// exit (#571). A race where the daemon exits between the signal-0 probe
	// above and this call is benign: errIsProcessGone covers both ESRCH and
	// the os.ErrProcessDone surface.
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		if errIsProcessGone(err) {
			log.InfoLog.Printf("daemon process (PID: %d) exited before SIGTERM landed; cleaning up", pid)
			cleanupDaemonRuntimeFiles(pidFile)
			return true, nil
		}
		return false, fmt.Errorf("failed to signal daemon process: %w", err)
	}

	// Poll for graceful exit.
	gracefulDeadline := time.Now().Add(stopDaemonGrace)
	exited := false
	for time.Now().Before(gracefulDeadline) {
		if !pidLooksAlive(pid) {
			exited = true
			break
		}
		time.Sleep(stopDaemonPoll)
	}

	if exited {
		log.InfoLog.Printf("daemon process (PID: %d) exited gracefully after SIGTERM", pid)
	} else {
		log.WarningLog.Printf("daemon process (PID: %d) did not exit within %s of SIGTERM; escalating to SIGKILL", pid, stopDaemonGrace)
		if err := proc.Signal(syscall.SIGKILL); err != nil && !errIsProcessGone(err) {
			return false, fmt.Errorf("failed to stop daemon process: %w", err)
		}
	}

	cleanupDaemonRuntimeFiles(pidFile)
	log.InfoLog.Printf("daemon process (PID: %d) stopped successfully", pid)
	return true, nil
}

// cleanupDaemonRuntimeFiles removes the PID file and (best-effort) the control
// socket left behind by a stopped daemon. The PID file is tolerated as
// already-gone because the daemon's own SIGTERM handler removes it via
// removeDaemonPIDFile() before exiting — so on the SIGTERM-success path we
// race with the daemon's own cleanup.
//
// A NEW daemon can also start during StopDaemon's signal/poll window (the
// autostart unit racing `af daemon install`, or an upgrade respawn) and bind
// the control socket before this cleanup runs. Removing the socket then would
// unlink the live daemon's socket file: the daemon keeps serving the
// unreachable inode, pings against the path fail, and the next EnsureDaemon
// spawns yet another daemon while the first leaks (#767). So if anything
// ANSWERS on the socket, the runtime files belong to a live daemon — leave
// them all in place. The daemon we just stopped cannot answer: its listener
// died with the process. The worst false positive (a ping answered by a
// process still mid-SIGKILL) merely leaves a stale socket behind, which the
// next spawn's bind path replaces.
func cleanupDaemonRuntimeFiles(pidFile string) {
	if err := pingDaemon(); err == nil {
		log.InfoLog.Printf("a live daemon answered on the control socket after stop; leaving its runtime files in place")
		return
	}
	if err := os.Remove(pidFile); err != nil && !os.IsNotExist(err) {
		log.WarningLog.Printf("failed to remove daemon PID file: %v", err)
	}
	if socketPath, socketErr := DaemonSocketPath(); socketErr == nil {
		_ = os.Remove(socketPath)
	}
}

// isAgentFactoryDaemon checks whether the process at pid looks like an agent-factory daemon:
// its argv must carry the --daemon flag as a discrete argument AND its executable must be an
// agent-factory binary ("af" or "agent-factory"). It reads the process argv with argument
// boundaries preserved (see daemonArgs); if no readable argv is available, returns false so
// callers treat the PID as unverified.
//
// Both checks are required so that a stale PID file whose PID has been reused by an unrelated
// process carrying a "--daemon" token (e.g. "sleep --daemon af-test") is not mistaken for our
// daemon and signaled by StopDaemon/locateDaemonPID. This mirrors the host-wide pgrep scan in
// sigterm_fallback.go, which also requires both argsHaveDaemonFlag and argsAreDaemonBinary;
// the two PID-validation paths must agree (#1004).
//
// Detection operates on real argv elements (not a space-joined string), so a binary installed
// under a path containing spaces — e.g. "/home/John Smith/.local/bin/af" — is classified
// correctly instead of having its path shredded across argv boundaries (#1214). We still require
// an exact "--daemon" token (or the "--daemon=..." form), so flags like --daemonize never match.
func isAgentFactoryDaemon(pid int) bool {
	args := daemonArgs(pid)
	if len(args) == 0 {
		return false
	}
	return argsHaveDaemonFlag(args) && argsAreDaemonBinary(args)
}

// argsHaveDaemonFlag reports whether argv contains "--daemon" as a discrete argument (either bare
// or in the "--daemon=value" form). It deliberately rejects substring matches like "--daemonize"
// or "--daemon-mode". Because it scans real argv elements, spaces inside another argument (such as
// a spaced binary path in argv[0]) can never fabricate or hide a "--daemon" token (#1214).
func argsHaveDaemonFlag(args []string) bool {
	for _, a := range args {
		if a == "--daemon" || strings.HasPrefix(a, "--daemon=") {
			return true
		}
	}
	return false
}

// argsAreDaemonBinary reports whether argv[0] is an agent-factory daemon binary: installed as "af"
// or built from source (`go build .`) as "agent-factory". The host-wide pgrep scan in
// sigterm_fallback.go matches any process carrying a "--daemon" token, so this restores the
// binary-name specificity that the old "af --daemon" substring pattern provided — while still
// catching source-built `agent-factory --daemon` daemons that the old pattern missed (#937).
//
// argv[0] is a single argv element, so filepath.Base sees the whole executable path even when it
// contains spaces (e.g. "/home/John Smith/.local/bin/af" → base "af"). The previous
// implementation space-joined the argv and re-split on whitespace, which turned that same path
// into base "John" and made every spaced-install daemon undetectable (#1214).
func argsAreDaemonBinary(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch filepath.Base(args[0]) {
	case "af", "agent-factory":
		return true
	default:
		return false
	}
}

// daemonArgs returns the argv of pid with argument boundaries preserved. It prefers
// /proc/<pid>/cmdline (NUL-separated argv, Linux), the only source that keeps spaces inside an
// individual argument intact. When /proc is unavailable (macOS and other unixes) it falls back to
// `ps -p <pid> -o args=`, whose output is already space-joined and therefore CANNOT recover the
// original argv boundaries for a spaced binary path — the fallback splits on whitespace and is
// best-effort, so spaced-install detection (#1214) is only fully reliable where /proc exists.
func daemonArgs(pid int) []string {
	if args := readProcArgv(pid); args != nil {
		return args
	}
	ps := readPsArgs(pid)
	if ps == "" {
		return nil
	}
	return strings.Fields(ps)
}

// readProcArgv reads /proc/<pid>/cmdline (Linux) and splits it into argv on the NUL separators,
// preserving spaces within an individual argument. Returns nil when /proc is unavailable or the
// process has no cmdline (zombies, kernel threads).
func readProcArgv(pid int) []string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil
	}
	return splitNULArgv(data)
}

// splitNULArgv splits a /proc cmdline blob on its NUL separators into argv, dropping the trailing
// empty element left by the final NUL terminator. Returns nil for an empty/whitespace-only blob.
func splitNULArgv(data []byte) []string {
	parts := strings.Split(string(data), "\x00")
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) == 0 {
		return nil
	}
	return parts
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
