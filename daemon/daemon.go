package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	sessiontmux "github.com/sachiniyer/agent-factory/session/tmux"
)

// restoreManagerForStartup is the warm-up restore entry point RunDaemon uses.
// Package-level so tests can inject a slow or gated restore and prove the
// control socket binds and serves before the restore completes (#829).
// RunDaemon opens the manager's readiness barrier itself, after the startup
// orphan sweep, so a concurrent create cannot manufacture a sweep candidate.
var restoreManagerForStartup = func(m *Manager) error { return m.restoreInstances() }

// testHookDaemonBeforeHomeLockRelease and testHookDaemonAfterHomeLockRelease
// bracket the home-lock release inside runDaemon's outermost defer (#5188).
// The first fires while the exiting daemon still holds the lock — a test can
// assert daemon.pid still names it then — and the second fires in the
// release→remove window where a successor daemon can win the home and rewrite
// daemon.pid, a rewrite the teardown removal must preserve. No-ops in
// production.
var (
	testHookDaemonBeforeHomeLockRelease = func() {}
	testHookDaemonAfterHomeLockRelease  = func() {}
)

// RunDaemon runs the daemon process: it serves the local control plane,
// evaluates task cron schedules in-process, supervises watch-task scripts,
// and iterates over all sessions each poll to compute their authoritative
// status (Ready/Dead/Running, #935/#960 PR 5).
//
// Startup ordering matters (#829): the control socket binds BEFORE the
// instance restore. Pre-#829 the restore ran first, so every concurrent
// EnsureDaemon found no socket and spawned another daemon that performed a full
// restore before losing the bind race. During the warm-up window Ping and
// Shutdown work and state-dependent RPCs return errDaemonStarting; the scheduler,
// watcher supervisor, and session-status poll loop start only after the restore
// because they act on restored state.
func RunDaemon(cfg *config.Config) error {
	return runDaemon(cfg, "")
}

// RunDaemonForUpgrade runs the daemon as an upgrade CANDIDATE in probation for
// transactionID (#2212 R2). Carrying the id makes the daemon (a) enter
// DaemonPhaseUpgradeProbation — restoring state but refusing mutating RPCs until
// its supervisor validates and releases it — and (b) skip the entrypoint gate,
// which a plain start would trip (the candidate's own live transaction would
// defer it). Only the recovery actor's StartCandidate reaches this path.
func RunDaemonForUpgrade(cfg *config.Config, transactionID string) error {
	if transactionID == "" {
		return fmt.Errorf("upgrade daemon requires a transaction id")
	}
	return runDaemon(cfg, transactionID)
}

// chdirToNeutralHome moves the daemon off whatever cwd the spawning process
// handed it and onto the AF home, so no daemon-spawned process can inherit a
// managed worktree as its cwd. See runDaemon for the full rationale; this is
// the single helper that holds the property "no daemon-spawned process can
// have a worktree as its cwd unless that worktree is the one it's working on"
// for every exec.Command site the daemon forks without setting cmd.Dir.
// Best-effort and non-fatal: a resolution failure leaves the inherited cwd,
// which under systemd is / and under an ad-hoc start is the user's.
func chdirToNeutralHome() {
	dir, ok := configHomeDir()
	if !ok {
		return
	}
	// Record the daemon's launch cwd before chdir'ing so the git runners can
	// resolve a relative persisted path (NewGitWorktreeFromStorage stores paths
	// verbatim) against it rather than the AF home we are about to move into.
	// Without this the chdir would make a relative `-C path` resolve beneath the
	// AF home and break the restored session (see daemonLaunchCwd in
	// session/git/worktree_git.go).
	if cwd, err := os.Getwd(); err == nil {
		sessiongit.SetDaemonLaunchCwd(cwd)
	}
	// Resolve to an absolute path BEFORE chdir'ing. A relative
	// AGENT_FACTORY_HOME (ConfigDirFor preserves a non-empty value verbatim,
	// e.g. "af-home") is resolved against the daemon's cwd. os.Chdir into it
	// would move the daemon's cwd to <launch-cwd>/af-home while leaving the env
	// value relative, so every later config.GetConfigDir() call resolved
	// "af-home" against the NEW cwd and yielded <launch-cwd>/af-home/af-home —
	// a nonexistent nested path that breaks control-socket binding and the home
	// watcher. Absolutize against the current (pre-chdir) cwd — the same frame
	// acquireHomeLock just created the home in — and fix the env to that
	// absolute path, so the home stays stable for the whole daemon lifetime.
	abs, err := filepath.Abs(dir)
	if err != nil {
		return
	}
	if abs != dir {
		os.Setenv("AGENT_FACTORY_HOME", abs)
	}
	_ = os.Chdir(abs)
}

// chdirToNeutralHomeFn is the injection point runDaemon calls. Tests that run
// RunDaemon in-process stub it to a no-op so the process-wide os.Chdir does
// not leak into later tests' cwd assumptions (a temp AF home a test set via
// t.Setenv is removed on cleanup, leaving the process cwd pointing at a
// deleted directory — the "getwd: no such file or directory" failure). The
// daemon_cwd_test.go tests call chdirToNeutralHome directly to exercise the
// real behaviour.
var chdirToNeutralHomeFn = chdirToNeutralHome

// runDaemon carries the transaction identity used by the probation machinery.
// The public daemon entrypoint deliberately supplies no transaction: only the
// durable transaction layer may eventually select the unexported non-empty
// path, so this stage cannot put an ordinary daemon into probation.
func runDaemon(cfg *config.Config, upgradeTransactionID string) error {
	log.InfoLog.Printf("starting daemon")

	// #2212 R1: on an ordinary daemon start (not the transaction's own probation
	// daemon, which carries a non-empty transaction id), defer to a genuinely
	// in-progress FORWARD upgrade instead of serving a rival, and exit cleanly so
	// the autostart unit's Restart=on-failure does not loop against the recovery
	// actor. But a rollback restoring the previous daemon must PROCEED to bind:
	// during that phase THIS daemon is the previous daemon the actor started and
	// is waiting to validate — deferring would deadlock the rollback into
	// rollback_failed with no daemon running. Uses the non-waking gate so it never
	// re-runs the client's wake/wait and overruns the bind budget. Fail-open: a
	// stale or corrupt journal proceeds, so a bad journal can never wedge the
	// daemon into the #2168 crash loop.
	if upgradeTransactionID == "" {
		if homeDir, ok := configHomeDir(); ok {
			if decision, _ := checkUpgradeGate(homeDir, true); decision == upgradeGateInProgress {
				log.InfoLog.Printf("a forward daemon upgrade is in progress; deferring to its recovery actor and exiting cleanly")
				return nil
			}
		}
	}

	// No auth-posture gate here, deliberately (#2168 Phase 0). #2090 made a
	// tokenless network listener a FATAL startup refusal at this exact spot; the
	// owner reversed that: binding 0.0.0.0 with no token is allowed, and the
	// exposure is surfaced as a warning instead of decided for the user.
	//
	// Two reasons it does not simply move up here as a log line. The exposure is
	// only real once the listener actually binds — a warning emitted here would
	// still fire when the web port is taken and nothing gets served — and the
	// "say it exactly once" requirement is easiest to keep honest at the single
	// site that opens the port. So the notice
	// (config.ListenerExposureNotice) is emitted by startHTTPServer, which
	// RunDaemon calls once, below.
	//
	// The refusal's other effect was the #2168 incident: a config rejected on
	// every attempt is not transient, but the autostart unit's
	// Restart=on-failure could not tell that apart from a crash, so the unit
	// restarted every 5s forever. Nothing here exits non-zero on config posture
	// any more.

	// Refuse to run two daemons against the same control socket. EnsureDaemon
	// pings before launching, but a daemon started directly (af --daemon, the
	// autostart unit, or two racing af invocations) would otherwise steal the
	// socket from a live daemon and leave duplicate status/scheduler loops.
	// Exiting cleanly matters: under the autostart unit a non-zero exit would
	// trip Restart=on-failure into a retry loop against the live daemon.
	if err := pingDaemon(); err == nil {
		log.InfoLog.Printf("another agent-factory daemon is already serving the control socket; exiting")
		return nil
	}

	// Acquire the exclusive per-home lock BEFORE touching the socket, PID file,
	// or any state. This is the singleton guarantee (#960 split-brain): only the
	// lock holder ever binds the control socket or writes state, so a second
	// daemon can never clobber a live one — even if the ping above transiently
	// failed under load and let it slip past. The flock is held for the whole
	// daemon lifetime and auto-releases if the process dies, so a crashed
	// daemon's lock frees automatically with no stale-lock pid guessing.
	//
	// A held lock means another live daemon owns this home (the top ping only
	// misses it during the sub-millisecond window between its flock and its
	// socket bind, or if it is wedged): fail fast, non-zero, WITHOUT removing
	// the socket / rewriting the PID file, so the live daemon is left intact.
	lock, err := acquireHomeLock()
	if err != nil {
		if isDaemonLockHeldErr(err) {
			log.InfoLog.Printf("%v", err)
		}
		return err
	}
	// daemon.pid names this home's daemon for exactly the span it is alive
	// AND holding the home lock (#5188): the file therefore survives the
	// whole teardown tail and is removed only here, in the LAST deferred
	// action — after the lock is released, never while it is still held.
	// The removal re-reads the file under daemon.pid.lock and unlinks only
	// when it still names this process (removeDaemonPIDFile), so a successor
	// that wins the home in the release→remove window keeps the file it
	// wrote. Identity-guarded removal is also what makes calling it
	// unconditional safe on the early exits below: a file naming another
	// daemon is never ours to delete.
	defer func() {
		testHookDaemonBeforeHomeLockRelease()
		lock.release()
		testHookDaemonAfterHomeLockRelease()
		removeDaemonPIDFile()
	}()

	// Move the daemon off whatever cwd the spawning `af` invocation handed it
	// and onto the AF home, so no daemon-spawned process can inherit a managed
	// worktree as its cwd. The daemon is routinely auto-started from inside a
	// worktree and never chdirs on its own, so without this every exec.Command
	// it forks that does not set cmd.Dir (tmux, gh, hooks, watch tasks, and the
	// 41+ sites outside session/git) would inherit that worktree and be a false
	// positive for the worktree writer-reaper's cwd match — an unrelated process
	// SIGTERM'd during a concurrent reap of the inherited worktree. The git
	// runners keep their own cmd.Dir as defence in depth, but the class of
	// children without it is the source the reaper must not see a worktree for.
	// acquireHomeLock just created the home, so the chdir target exists. Under
	// systemd the daemon already starts in /; this covers the ad-hoc and launchd
	// paths that inherit the spawner's cwd. Best-effort: a resolution failure
	// leaves the inherited cwd, which under systemd is / and under an ad-hoc
	// start is the user's (rarely a managed worktree, and the reaper excludes
	// the scanning process itself).
	chdirToNeutralHomeFn()

	// The home exists now — acquireHomeLock just created it — so latch it, and no
	// write this daemon makes can re-create the directory once it is deleted
	// (#3845). The other half of the same self-check, watchDaemonHome, is started
	// after the restore below. Released on the way out so an in-process daemon
	// (the tests in this package) leaves no refusal behind for the next one.
	releaseHomeLatch := latchDaemonHomePresent()
	defer releaseHomeLatch()

	// Notify on SIGINT (Ctrl+C) and SIGTERM, and watch for a Shutdown RPC.
	// The RPC path is used by `af upgrade` / autoUpdate after writing a new
	// binary so the next RPC respawns the daemon from the fresh image (#498).
	// Registered HERE — before the PID file is published inside
	// bindControlServerExclusive below, not merely before the restore: once
	// daemon.pid names this process the daemon is discoverable and
	// signalable, so a supervisor stop or a `kill` landing anywhere in the
	// startup tail must reach a select on this channel and run the deferred
	// teardown. Go's default SIGTERM termination would skip every defer —
	// leaving the published PID file behind and abandoning whatever the
	// manager and listeners already created.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	// Notify has already disabled the default disposition for these
	// signals, but nothing RECEIVES on sigChan until the restore select far
	// below — the manager storage load, the unbounded daemon.spawn lock
	// wait inside bindControlServerExclusive, and the socket binds all sit
	// in the window. A stop landing there would sit buffered while the
	// daemon holds the home lock indefinitely; a second is dropped
	// outright. Bridge the gap with a one-shot watcher: if a signal
	// arrives before the serve path takes over, re-raise the default
	// disposition so the process dies the way it would have without
	// Notify. The PID file can be left naming a now-dead process — the
	// stale-PID paths own that well-defined case; a wedged daemon that
	// ignores supervisor stops while holding the lock is the worse
	// failure.
	startupSignalWatch := make(chan struct{})
	startupSignalWatcherDone := make(chan struct{})
	var startupSignalWatchOnce sync.Once
	stopStartupSignalWatch := func() {
		startupSignalWatchOnce.Do(func() {
			close(startupSignalWatch)
			// Join, not just close: only after the watcher goroutine has
			// exited is the select below provably the sole sigChan
			// receiver. A signal arriving during a close-without-join
			// handoff could be won by the still-live watcher and hard-kill
			// the process past deferred listener/manager/PID cleanup, even
			// though the graceful receiver is already armed.
			<-startupSignalWatcherDone
		})
	}
	defer stopStartupSignalWatch()
	go func() {
		defer close(startupSignalWatcherDone)
		select {
		case sig := <-sigChan:
			// A signal landing while stopStartupSignalWatch's close is in
			// flight makes both cases ready, and Go picks one at random.
			// Re-check so the stand-down ALWAYS wins once it has closed —
			// and replay the consumed signal back into the buffered
			// channel so the first real consumer drains it through the
			// armed cleanup defers instead of this watcher dropping it or
			// hard-killing past them.
			select {
			case <-startupSignalWatch:
				// The replay must not block: a second signal can refill
				// the capacity-one channel in the gap between the
				// dequeue and this send, and a blocked watcher would
				// deadlock the joining stand-down while the daemon holds
				// the home lock. If the slot is already taken, a shutdown
				// signal is already queued for the graceful consumer —
				// dropping this duplicate loses nothing.
				select {
				case sigChan <- sig:
				default:
				}
				return
			default:
			}
			signal.Reset(syscall.SIGINT, syscall.SIGTERM)
			if sysSig, ok := sig.(syscall.Signal); ok {
				_ = syscall.Kill(syscall.Getpid(), sysSig)
			}
		case <-startupSignalWatch:
		}
	}()

	// daemon.pid is published inside bindControlServerExclusive, under the
	// daemon.spawn lock between the under-lock ping and the socket bind.
	// That is the only point that satisfies both halves of the #5188
	// contract at once: the ping proves no daemon is answering — including
	// a pre-home-lock legacy daemon the top ping missed, whose PID file
	// this start must not overwrite and then remove — and the file exists
	// before the socket can ever serve, so a Ping can never succeed for an
	// unpublished daemon.

	// Shell only — no restore yet, so the bind below happens within
	// milliseconds of process start.
	manager, err := newManagerShellForDaemon(cfg, upgradeTransactionID)
	if err != nil {
		return err
	}

	// Stop every daemon-spawned VS Code editor on the way out. Without this a
	// shutdown would strand a code-server per session, still holding its loopback
	// port and now reachable by nothing — the leak class this feature must not
	// introduce. (A SIGKILLed daemon still orphans them; ensureServer's
	// worktree-checked reuse means a restarted daemon starts fresh editors rather
	// than adopting the strays.)
	//
	// Registered HERE — the instant the supervisor exists, before the control
	// socket and HTTP server bind — rather than after the instance restore, so the
	// warm-up exit paths cannot skip it. Both early returns from the restore select
	// below (SIGTERM and the Shutdown RPC) leave RunDaemon without ever reaching
	// the post-restore section, and an editor CAN exist by then: the webtab proxy
	// route is serving from the moment the HTTP server binds, and it resolves its
	// session through refreshLocked, which loads instances from disk on its own. So
	// a stale iframe refresh during a slow restore can drive its own restore, spawn
	// an editor, and a SIGTERM moments later would orphan it. Deferring at the
	// point of construction makes the stop unconditional on how far startup got.
	defer manager.vscode.Stop()
	// Same reasoning, same place: a config agent is a bare tmux session with no
	// Instance, so NOTHING else knows it exists — not instances.json, not the
	// roster, not the restore loop. If this daemon exits without reaping them
	// they are orphans no future daemon can find, which is the #1093/#1104 class
	// this repo has already been bitten by. Registered at construction so the
	// warm-up exit paths (SIGTERM, the Shutdown RPC) cannot skip it.
	defer manager.configAgents.Stop()
	// And the login panes, for exactly the same reason (#3384). An account login
	// is a bare tmux session with no Instance, so this is the only thing that
	// knows it exists; a half-finished OAuth flow must die with the daemon that
	// spawned it rather than sit on the tmux server forever.
	defer manager.accountLogins.Stop()
	// Tear the web config-assistant stream down before its tmux session is reaped
	// above, so the clientless capture goroutine ends cleanly. stop() closes the
	// streamer only; configAgents.Stop() owns killing the tmux (#2467). Deferred
	// after configAgents.Stop() so it runs BEFORE it (LIFO): the capture stops, then
	// the pane it was reading is torn down.
	defer manager.configAssistants.stop()

	scheduler := newTaskScheduler()
	watchers := newWatcherSupervisorWithEventsPerMinute(cfg.WatcherEventsPerMinute)
	watchers.observeTargetLimit = manager.observeTaskTargetLimit

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
	controlClosed := false
	defer func() {
		if controlClosed {
			return
		}
		if err := closeControl(); err != nil {
			log.WarningLog.Printf("failed to close daemon control socket: %v", err)
		}
	}()

	// Hoist the watch-task supervisor's Stop above the upgrade-probation
	// select below: a released upgrade candidate transitions to
	// DaemonPhaseHandoffPending while still parked in that select, and in that
	// window it admits task-mutating RPCs (AddTask/UpdateTask/ReloadTasks) that
	// arm real watcher subprocesses via watchers.reconcile -> go w.run() ->
	// cmd.Start() ($SHELL -c watch_cmd, Setpgid). The select exits via
	// `return nil` on signal or shutdown, which would skip a defer placed after
	// it and leak those subprocesses: the reliable SIGTERM/SIGKILL group
	// teardown lives only in watchers.Stop, and no startup sweep can discover
	// orphaned watch_cmd processes. Registered after the closeControl defer so
	// it runs BEFORE closeControl on LIFO, leaving the control socket live for
	// any in-flight watch-event deliveries during teardown. No-op if reconcile()
	// was never called — it iterates an empty map and Stop is idempotent.
	defer watchers.Stop()

	// Stand the startup watcher down BEFORE the HTTP listener starts serving:
	// once the webtab proxy is reachable it can spawn editors, and a signal
	// caught by the hard-kill watcher would strand them along with the runtime
	// files the defers above are registered to clean. From here a signal just
	// buffers in sigChan until the restore select — the channel's first real
	// receiver — drains it into the graceful return, where every deferred
	// cleanup runs.
	stopStartupSignalWatch()

	// Start the HTTP/JSON mirror alongside the control socket (#1029 PR 4). It
	// shares this daemon's live manager, so HTTP is just another thin client of
	// the same core. Only the winner of bindControlServerExclusive reaches this
	// point, so no extra spawn race applies. A bind failure is logged but never
	// fatal: HTTP is auxiliary — the gob control plane every existing client
	// depends on must not regress if the HTTP socket cannot bind.
	var closeHTTP func() error
	httpClosed := false
	if closeCandidate, err := startHTTPServer(manager, scheduler, watchers); err != nil {
		log.WarningLog.Printf("failed to start daemon HTTP server: %v", err)
	} else {
		closeHTTP = closeCandidate
		defer func() {
			if httpClosed {
				return
			}
			if err := closeHTTP(); err != nil {
				log.WarningLog.Printf("failed to close daemon HTTP server: %v", err)
			}
		}()
	}

	// Run the restore concurrently so a shutdown or signal during warm-up exits
	// promptly instead of waiting for the restore to finish. The
	// warm-up exit paths deliberately skip SaveInstances: nothing has been
	// restored, and saving the empty instance map would wipe every persisted
	// session.
	//
	// Establish shared tmux infrastructure at this last pre-restore barrier. The
	// control socket remains early (#829), while warming rejects every concurrent
	// mutation, so no session can spawn before this finishes. The launch is
	// additive and fail-open: an unavailable user systemd manager leaves the
	// historical per-session scope fallback in place.
	if home, homeErr := config.GetConfigDir(); homeErr != nil {
		log.WarningLog.Printf("cannot configure dedicated tmux server launch; session creation will use its fallback: %v", homeErr)
	} else {
		restoreTmuxServerConfig := sessiontmux.ConfigureDaemonServer(home)
		defer restoreTmuxServerConfig()
		if serverErr := sessiontmux.EnsureDaemonServer(); serverErr != nil {
			log.WarningLog.Printf("dedicated tmux server launch failed; session creation will use its fallback: %v", serverErr)
		}
	}
	log.InfoLog.Printf("control socket bound; restoring instances")
	restoreDone := make(chan error, 1)
	// Capture the seam on the main flow: reading the package var inside the
	// goroutine would race with tests restoring it after RunDaemon returns.
	restore := restoreManagerForStartup
	go func() { restoreDone <- restore(manager) }()
	// The startup watcher was retired once the control-socket cleanup was
	// armed (above); a signal landing since then sits buffered in sigChan for
	// this select.
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
	// Upgrade candidates deliberately park before the orphan sweep. Preserve the
	// pre-existing probation contract: restored state is readable while every
	// mutation remains blocked by the lifecycle gate.
	if manager.lifecycle.snapshot().transactionID != "" {
		manager.finishInstanceRestore()
	}
	if manager.lifecycle.isUpgradeProbation() {
		log.InfoLog.Printf("instance restore complete; daemon is in upgrade probation")
		select {
		case sig := <-sigChan:
			log.InfoLog.Printf("received signal %s during upgrade probation; exiting", sig.String())
			return nil
		case <-shutdownCh:
			log.InfoLog.Printf("received shutdown request during upgrade probation; exiting")
			return nil
		}
	}

	// Remove per-task timer units left behind by pre-#782 versions; the
	// in-process scheduler below replaces them.
	sweepLegacyTaskUnits()

	sweepStartupOrphanContainers(manager)
	sweepStartupTabCleanup(manager)
	manager.finishInstanceRestore()

	// Start task automation only after the control server is up and restore has
	// finished: a task firing immediately loops back through our own socket and
	// needs a ready manager. Revalidate every persisted target first. In
	// particular, root-agent policy takes effect on this daemon start, so a root
	// task accepted under the previous config must not be armed when the new
	// policy can no longer materialize it. One preflight owns both cron and watch:
	// on failure neither starts, and one actionable log replaces a permanent
	// per-fire retry loop (#2646).
	taskArmErr := armTaskAutomation(manager, scheduler, watchers)
	if taskArmErr != nil {
		log.WarningLog.Printf("task automation not armed: %v", taskArmErr)
	}
	scheduler.Start()
	defer scheduler.Stop()

	wg := &sync.WaitGroup{}
	stopCh := make(chan struct{})
	startInstancePollLoop(manager, time.Duration(cfg.DaemonPollInterval)*time.Millisecond, stopCh, wg)

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

	// This is the full operational barrier, later than Manager.Ready: the
	// scheduler, watcher supervisor, status poll, and home watcher are
	// all armed. Ping answering before here is liveness, never proof of health.
	if err := manager.lifecycle.markReady(); err != nil {
		close(stopCh)
		wg.Wait()
		manager.waitRootAgentCreatesForShutdown()
		return fmt.Errorf("failed to mark daemon ready: %w", err)
	}
	log.InfoLog.Printf("daemon ready")

	// Start the daemon-owned release check only now, past the readiness barrier:
	// a box that never opens the TUI never reaches the launch-path updater, so
	// the daemon does its own checking (#2212). It reports what it finds and
	// installs nothing — see daemon/update_driver.go for why activation is a
	// separate slice. Started after markReady so it can never compete with the
	// restore or delay readiness, and registered on the same stopCh/wg as the
	// other loops so shutdown stops it.
	// The upgrade hand-off needs to end this daemon, and it must not race the
	// Shutdown RPC's own close of shutdownCh — two independent closers of one
	// channel is a panic. So the driver gets its own channel and the main select
	// below treats it as one more way to stop, which also keeps the graceful
	// path intact: the same teardown, the same final SaveInstances.
	upgradeExitCh := make(chan struct{})
	var upgradeExitOnce sync.Once
	startUpdateDriver(manager, func() { upgradeExitOnce.Do(func() { close(upgradeExitCh) }) }, stopCh, wg)

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
	case <-upgradeExitCh:
		log.InfoLog.Printf("quiescing for a daemon-owned upgrade hand-off")
	}

	drainDaemon(manager, closeHTTP, closeControl, &httpClosed, &controlClosed, stopCh, wg)

	if homeGone {
		// Skip the final save: the home directory was deleted out from under
		// us, so there is no installation left to persist into — saving would
		// recreate a skeleton of the deleted home and resurrect the abandoned
		// state the deletion was meant to remove.
		return nil
	}

	if err := manager.SaveInstancesForShutdown(); err != nil {
		log.ErrorLog.Printf("failed to save instances when terminating daemon: %v", err)
	}
	return nil
}

// refreshDaemonInstances materializes the daemon's in-memory instance map from
// disk. It ALSO returns the ghost task runs: persisted rows whose task run is
// still in flight but which could not be turned into an Instance (#1892).
//
// The second return exists because m.instances is NOT the universe. A row that
// fails to materialize is skipped here and is invisible to anything that walks the
// map — but the agent it describes may well still be running, and the run it
// belongs to is still in flight by the only definition that matters (the
// persisted marker says so). The watch-task concurrency cap counts by walking
// m.instances, so without this a task whose sessions failed to load would admit
// replacements past max_concurrent_runs on every daemon restart, which is exactly
// the restart-survival guarantee the cap is built on.
//
// Keyed by taskRunReservationKey(repoID, taskID) → count, so the cap can add them
// to its projection without re-reading disk. Recomputed on every refresh, so a
// row that starts loading again stops being a ghost — the same self-healing
// projection discipline as the rest of the count.
//
// The fifth return, reread, names every repo whose instances.json this call
// read AND parsed INTO a fully loadable row set. It is the only evidence that
// clears a repo from the skip set (retainStillSkipped): a repo the loader
// could not read, that is absent from disk, or that parses-but-yields-any-
// unloadable-row (retracted below) is not in it, so neither an omission, a
// zero-rows file, nor a partial loss is a repair (#4783, #4812, #4876).
func refreshDaemonInstances(existing map[string]*session.Instance) (map[string]*session.Instance, map[string]int, []SkippedRepo, map[string]bool, error) {
	load, err := loadAllRepoInstancesForRefresh()
	if err != nil {
		return existing, nil, nil, nil, err
	}
	allInstances, unreadable, missing, sigs := load.Instances, load.Skipped, load.Missing, load.Signatures

	next := make(map[string]*session.Instance)
	ghostTaskRuns := make(map[string]int)
	// skipped collects repos whose instances.json failed to read or parse, so the
	// Snapshot RPC can carry the drop to clients instead of silently serving a
	// partial list (#603 closed over the wire). Collected on every refresh, but
	// only the startup call (existing==nil) genuinely drops rows — the polling
	// path re-hydrates a corrupted repo's prior in-memory rows, so its sessions
	// stay in the snapshot and the caller (restoreInstances vs refreshLocked)
	// decides whether the set is authoritative for the snapshot: seed at
	// startup, then trim repaired repos on poll without ever adding a
	// mid-life-corrupted one (see retainStillSkipped).
	var skipped []SkippedRepo
	// An unreadable repo is skipped exactly like a corrupt one, with its own
	// reason so the refusal can say "could not be read" rather than "corrupted"
	// (#4783). Its rows, like a corrupt repo's, are re-hydrated from existing on
	// the poll by the absent-repo pass below, since the loader left it out of
	// allInstances.
	unreadableRepos := make(map[string]bool, len(unreadable))
	for _, skip := range unreadable {
		log.WarningLog.Printf("daemon skipping repo %s: unreadable instances.json: %s", skip.RepoID, skip)
		skipped = append(skipped, SkippedRepo{RepoID: skip.RepoID, Reason: skippedRepoReasonForReadError(skip.Err)})
		unreadableRepos[skip.RepoID] = true
	}
	reread := make(map[string]bool, len(allInstances))
	for repoID, raw := range allInstances {
		sig := sigs[repoID]
		var data []session.InstanceData
		var keys []string
		if outcome, ok := refreshRowOutcomes.get(repoID, sig); ok {
			// The file's signature is unchanged, so its parse verdict and row
			// set are the ones recorded when it was read — replay them and let
			// the row loop below re-run its per-tick materialization against
			// `existing` exactly as it would on freshly decoded rows (#5169).
			switch {
			case outcome.corruptErr != nil:
				skipped = recordCorruptedRepo(repoID, outcome.corruptErr, existing, next, skipped)
				continue
			case outcome.empty:
				reread[repoID] = !missing[repoID]
				continue
			default:
				data, keys = outcome.rows, outcome.keys
				reread[repoID] = true
			}
		} else {
			if raw == nil || string(raw) == "[]" || string(raw) == "null" {
				// A missing file loads as "[]" too, but nothing was read, so it
				// clears nothing (#4783).
				reread[repoID] = !missing[repoID]
				refreshRowOutcomes.store(repoID, repoRowsOutcome{sig: sig, empty: true})
				continue
			}

			if err := json.Unmarshal(raw, &data); err != nil {
				// Skip corrupted per-repo JSON instead of failing the whole
				// refresh (#603). At startup (existing==nil) a single corrupt
				// file used to abort NewManager and orphan every live session
				// across every repo. On the polling path we also
				// re-hydrate this repo's prior in-memory instances so a
				// transient/persistent corruption doesn't silently drop
				// already-running sessions — matching the pre-fix semantics
				// of returning `existing` on parse failure.
				//
				// A corrupt file yields NO rows, so its task runs cannot be counted as
				// ghosts either — there is nothing to read a task_id out of. On the poll
				// path the re-hydrated instances above keep counting; at startup
				// (existing==nil) this repo's runs are genuinely unknowable until the file
				// is repaired, and a capped task there may over-admit. That is the one hole
				// the ghost count cannot close, and it is bounded by the same corruption
				// that already costs the repo its whole session list.
				refreshRowOutcomes.store(repoID, repoRowsOutcome{sig: sig, corruptErr: err})
				skipped = recordCorruptedRepo(repoID, err, existing, next, skipped)
				continue
			}
			reread[repoID] = true
			keys = make([]string, len(data))
			for i := range data {
				keys[i] = daemonInstanceKey(repoID, data[i].Title)
			}
			refreshRowOutcomes.store(repoID, repoRowsOutcome{sig: sig, rows: data, keys: keys})
		}
		// Retractable below: a parses-but-any-row-unloadable file is not a
		// repair, or list/get/whoami silently serve a partial list as the
		// complete answer the skip set exists to prevent — a partial loss is
		// the same lie as a total one (#4812, #4876, the "read AND parsed" trim
		// left open here).
		materialized, failedRows := 0, 0
		for i, item := range data {
			key := keys[i]
			if item.ID == "" && !isLegacyTransientGhost(item) {
				item.ID = session.NewInstanceID()
				if existing != nil {
					if prior := existing[key]; prior != nil && prior.ID != "" {
						item.ID = prior.ID
					}
				}
				if err := persistLegacyInstanceID(repoID, item); err != nil {
					log.WarningLog.Printf("daemon skipping legacy instance %q: could not durably assign a stable identity: %v", item.Title, err)
					if existing != nil {
						if prior := existing[key]; prior != nil {
							next[key] = prior
							continue
						}
					}
					if rawTaskRunHoldsSlot(item) {
						ghostTaskRuns[taskRunReservationKey(repoID, item.TaskID)]++
					}
					continue
				}
			}
			if existing != nil {
				if instance := existing[key]; instance != nil {
					next[key] = instance
					continue
				}
			}

			instance, err := fromInstanceDataForRefresh(item)
			if err != nil {
				failedRows++
				log.WarningLog.Printf("daemon skipping instance %q: %v", item.Title, err)
				// A marked row that cannot materialize still owes its teardown
				// (#4162) — the obligation is durable but nothing in memory can
				// drain it. Name the session so the leak is a visible diagnosis,
				// not the silent disappearance the marker exists to fix.
				if item.PendingOnComplete != nil {
					log.WarningLog.Printf("daemon: session %q is owed an on_complete teardown for task %s (filed %s) but its record failed to load; it stays on disk for repair or manual cleanup: %v",
						item.Title, item.PendingOnComplete.TaskID, item.PendingOnComplete.FiledAt.Format(time.RFC3339), err)
				}
				// The row is invisible to everything that walks m.instances from here on
				// — but its agent may still be running, and its task run is still in
				// flight if the persisted marker says so. Keep it counted against the
				// task's cap (#1892), or a session we merely failed to LOAD would let the
				// task admit a replacement beyond its limit.
				// StartupStateUnknown is terminal by definition. New writers clear
				// TaskRunActive on that transition, but keep this raw-row projection
				// defensive for a record written by an intermediate/older build: a
				// contradictory stale bit must not wedge max_concurrent_runs forever.
				// UserKilled is terminal in the same way and matters more here, because
				// the ghost path is the one place its slot can never be released
				// otherwise: holdsTaskRunSlot already frees a tombstoned INSTANCE (via
				// canAutoRestoreLostSession — "finish-this-kill, never restore-this"),
				// but the bit is only cleared by finishing the kill, and finishUserKill
				// runs against m.instances, which is exactly where a ghost is absent. A
				// tombstoned row that stops loading would therefore hold its slot for
				// good and park every later event for that task (#2418).
				// LostRestoreFailure is likewise terminal: the retry loop deliberately
				// released the slot after giving up, so an unloadable copy cannot reclaim
				// it as a ghost on the next daemon start (#3310).
				if rawTaskRunHoldsSlot(item) {
					ghostTaskRuns[taskRunReservationKey(repoID, item.TaskID)]++
					log.WarningLog.Printf("watch task %s: session %q failed to load but its run is still counted against max_concurrent_runs (#1892); kill or repair the session to release its slot", item.TaskID, item.Title)
				}
				continue
			}
			next[key] = instance
			materialized++
		}
		// Parsed but not fully loadable is NOT a repair: retract the "repaired"
		// signal so the repo stays skipped, and on poll record a rows-failed
		// skip entry whose reason rewrites the stale one (#4812, #4876). The
		// partial-loss and self-healing rationale lives in the helper.
		skipped = retractRereadOnUnloadableRows(reread, repoID, len(data), materialized, failedRows, existing != nil, skipped)
	}
	refreshRowOutcomes.prune(allInstances)

	// Preserve in-memory instances whose repo directory vanished from disk
	// entirely (#736). LoadAllRepoInstances only returns repos that still have
	// an on-disk instances directory, so an externally-deleted repo dir is
	// simply absent from allInstances and would otherwise be dropped from
	// `next`. This is a recoverable disk inconsistency — SaveInstances recreates
	// missing repo directories — so we re-hydrate the prior instances and log
	// loudly rather than silently abandoning running sessions. This
	// parallels the corrupted-JSON handling above, which also re-hydrates from
	// `existing`. On startup (existing == nil) there is nothing to preserve.
	if existing != nil {
		warnedRepos := make(map[string]bool)
		for key, inst := range existing {
			repoID, _ := splitDaemonInstanceKey(key)
			if _, ok := allInstances[repoID]; ok {
				continue
			}
			if !warnedRepos[repoID] && !unreadableRepos[repoID] {
				log.WarningLog.Printf("daemon preserving in-memory instances for missing repo directory: %s", repoID)
				warnedRepos[repoID] = true
			}
			next[key] = inst
		}
	}

	return next, ghostTaskRuns, skipped, reread, nil
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
