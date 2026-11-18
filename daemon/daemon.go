package daemon

import (
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
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
	// testHookDaemonBeforeHomeLatchRelease fires just before the home latch is
	// released, so a test can prove a stop that abandoned something exited
	// before it (#7439). A no-op in production.
	testHookDaemonBeforeHomeLatchRelease = func() {}
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

// runDaemon carries the transaction identity used by the probation machinery.
// The public daemon entrypoint deliberately supplies no transaction: only the
// durable transaction layer may eventually select the unexported non-empty
// path, so this stage cannot put an ordinary daemon into probation.
//
// retErr is named so a stop that abandoned something can exit with the status
// this return would have given the process (holdHomeIfAbandoned, #7439).
func runDaemon(cfg *config.Config, upgradeTransactionID string) (retErr error) {
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

	// No process-level auth-posture gate here, deliberately — the #5137 refusal
	// is scoped to the TCP LISTENER, not the daemon. #2090 made a tokenless
	// network listener a FATAL startup refusal at this exact spot, and the
	// autostart unit's Restart=on-failure could not tell a config the daemon
	// rejects on every attempt from a crash, so it restarted every 5s forever
	// (#2168 §1.2). #2168 Phase 0 then went to warn-and-serve, which #5137
	// reversed by owner decision: a non-loopback listen_addr with the token off
	// is refused unless network.allow_unauthenticated_network is set. What is
	// refused is the BIND — webListeners.reconcile declines the socket, logs the
	// reason at ERROR, and records it for `af daemon status` and `af doctor` —
	// while this process keeps starting and keeps serving the unix control
	// socket, so the TUI, CLI, and sessions are never hostages to a network
	// posture. Breaking the install path would be worse than the exposure.
	//
	// The reason it does not simply move up here as a log line is unchanged:
	// the posture is only real at bind time, and the "say it exactly once"
	// requirement is easiest to keep honest at the single site that decides the
	// socket — so both the exposure notice (config.ListenerExposureNotice, on an
	// opted-in bind) and the refusal (config.ListenerBindRefusal) are emitted by
	// the listener owner startHTTPServer reaches below.

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
	defer func() {
		testHookDaemonBeforeHomeLatchRelease()
		releaseHomeLatch()
	}()

	// A home the scaffold marked as a throwaway sandbox (#6844) carries its
	// own lifetime contract: this daemon enforces the deadline and the
	// declared-owner check itself (armThrowawayWatchdog), lists the home in
	// the sandbox registry so the next marked start can reap it, and arms a
	// teardown on unwind that takes the whole sandbox — driver, agents, and
	// the private tmux server — down with it. Registered after the latch and
	// lock defers, it runs before them on unwind, so the teardown still
	// holds the home lock: no successor can win the home and spawn into it
	// mid-teardown.
	//
	// endThrowaway runs that teardown once. A stop that abandoned something
	// exits holding the home before this defer is reached, so it calls
	// endThrowaway itself first (#7439): only this daemon and a later MARKED
	// start reap an expired sandbox's processes and private tmux servers, and a
	// throwaway home's daemon may never start again. The teardown reads the
	// marker and the recorded pids and signals processes; it writes nothing in
	// the home, so a concurrent abandoned writer there cannot disturb it.
	// throwawayExpired is read by the main goroutine (the final-save skip
	// below) and by the expired-container reap teardown step's goroutine, and
	// written by the select arms and the post-drain drains below. An atomic
	// keeps those accesses race-free under -race: the step's read can overlap
	// the endThrowaway drain's write on the abandoned path, and a plain bool
	// would flag there even though the read already happened before the write.
	var throwawayExpired atomic.Bool
	throwawayExpireCh := make(chan struct{})
	var throwawayHome string
	endThrowaway := func() {}
	if homeDir, homeErr := config.GetConfigDir(); homeErr == nil {
		throwawayHome = homeDir
		if marker, marked := loadThrowawayMarker(homeDir); marked {
			registerThrowawaySandbox(homeDir)
			recordThrowawayDaemon(homeDir)
			stopWatch := armThrowawayWatchdog(marker, throwawayExpireCh)
			var endOnce sync.Once
			endThrowaway = func() {
				endOnce.Do(func() {
					stopWatch()
					// Re-drain the channel non-blockingly before the gate. The
					// boolean is only ever set in the select arms that happen to
					// receive from throwawayExpireCh; a fire that lost a co-ready
					// stop case, or that closed the channel during drainDaemon's
					// join after the main select returned, is still observable
					// here. A closed channel is always receivable, so this also
					// makes every later consumer agree. Covers the deferred
					// path and the abandoned-stop path through holdHomeIfAbandoned.
					select {
					case <-throwawayExpireCh:
						throwawayExpired.Store(true)
					default:
					}
					if throwawayExpired.Load() {
						throwawayTeardownFn(homeDir, marker)
					}
				})
			}
			defer endThrowaway()
		}
	}

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
	//
	// The stop's deadline starts when its signal arrives (#7416), not when the
	// main flow next reads sigChan: a stop that lands during a startup step
	// sits buffered until the restore select while the stopper's clock runs.
	// So the signals arrive on rawSigs, and a relay stamps the stop clock
	// before handing each one on.
	stop := &stopClock{}
	rawSigs := make(chan os.Signal, 1)
	signal.Notify(rawSigs, syscall.SIGINT, syscall.SIGTERM)
	sigChan := make(chan os.Signal, 1)
	relayDone := make(chan struct{})
	defer close(relayDone)
	go relayStopSignals(rawSigs, sigChan, stop, relayDone)
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
	// The Shutdown RPC stamps the same clock the signal relay does.
	manager.lifecycle.useStopClock(stop)

	// What in-process code without a handle on this daemon reads
	// (stopped_daemon_state.go), published as soon as the lifecycle exists:
	//   - the drain predicate, for the task-delivery shutdown classifier
	//     (#5503), so a run already in flight when drainDaemon marks quiescing
	//     classifies its transport error as aborted rather than errored;
	//   - the cron delivery tracker (#7440): each delivery, scheduled,
	//     replayed or triggered, holds a slot while it runs, and the teardown
	//     step below closes it and joins them;
	//   - the lifecycle, which the control client asks before it would ensure
	//     a daemon (#7474).
	// Installed before the teardown is deferred, so what its return runs
	// comes after the teardown (defers unwind LIFO): a run arriving during the
	// teardown meets the closed tracker. And they stay closed once the daemon
	// returns (#7474): a goroutine starved through the whole teardown is
	// refused, never admitted untracked (latchStoppedDaemon).
	deliveries := &joinedRuns{}
	defer installInProcessDaemon(manager.lifecycle, deliveries)()

	// The teardown: every supervisor that stops what this daemon started, run
	// in parallel and bounded by the stop's deadline (daemon_teardown.go,
	// #7416). It is deferred HERE — the instant the supervisors exist, before
	// the control socket and HTTP server bind — rather than after the instance
	// restore, so the warm-up exits cannot skip it: both early returns from the
	// restore select below (SIGTERM and the Shutdown RPC) leave RunDaemon
	// without ever reaching the post-restore section, and each step below can
	// already have something to stop by then.
	//
	// After the teardown, a stop that abandoned anything — a teardown step
	// here, a drain step, the background join or the final save before it, or
	// a restore a warm-up exit left running — ends the process while it still
	// holds the home's flock and latch (#7439). The defers registered before
	// this one (the throwaway teardown, the latch and the flock releases, the
	// daemon.pid removal) are exactly what that exit skips, so nothing
	// abandoned can touch the home after its release.
	teardown := &daemonTeardown{}
	var restoreRunning atomic.Bool
	defer func() {
		for _, what := range teardown.run(stop.began().Add(stopTeardownBy)) {
			stop.abandon("teardown: " + what)
		}
		if restoreRunning.Load() {
			stop.abandon("the instance restore")
		}
		if len(stop.abandonments()) == 0 {
			// Nothing abandoned: the unwind goes on, and so does a panic, which
			// then exits 2 with Go's own trace, as it always has.
			return
		}
		// The process ends in holdHomeIfAbandoned, so a panic must be stopped
		// and reported here: os.Exit would end it silently, exit 0, and keep
		// systemd's Restart=on-failure from firing (#7447). recover is called
		// directly by this deferred function, as it must be, and only now that
		// the exit is certain. The stack still holds the panicking frames.
		panicked := recover()
		var stack []byte
		if panicked != nil {
			stack = debug.Stack()
		}
		holdHomeIfAbandoned(stop, endThrowaway, retErr, panicked, stack)
	}()
	// Every daemon-spawned VS Code editor. Without this a shutdown would strand
	// a code-server per session, still holding its loopback port and now
	// reachable by nothing — the leak class this feature must not introduce. (A
	// SIGKILLed daemon still orphans them; ensureServer's worktree-checked reuse
	// means a restarted daemon starts fresh editors rather than adopting the
	// strays.) An editor CAN exist during warm-up: the webtab proxy route is
	// serving from the moment the HTTP server binds, and it resolves its session
	// through refreshLocked, which loads instances from disk on its own, so a
	// stale iframe refresh during a slow restore can spawn one.
	teardown.add("VS Code editors", manager.vscode.Stop)
	// A config agent is a bare tmux session with no Instance, so NOTHING else
	// knows it exists — not instances.json, not the roster, not the restore
	// loop. If this daemon exits without reaping them they are orphans no
	// future daemon can find, which is the #1093/#1104 class this repo has
	// already been bitten by. The web config-assistant stream is torn down
	// first, in the same step, so the clientless capture goroutine ends cleanly
	// before the pane it reads is reaped: stop() closes the streamer only, and
	// configAgents.Stop() owns killing the tmux (#2467).
	teardown.add("config assistant streams, then config-agent sessions", func() {
		manager.configAssistants.stop()
		manager.configAgents.Stop()
	})
	// The login panes, for exactly the same reason (#3384). An account login is
	// a bare tmux session with no Instance, so this is the only thing that
	// knows it exists; a half-finished OAuth flow must die with the daemon that
	// spawned it rather than sit on the tmux server forever.
	teardown.add("account login panes", manager.accountLogins.Stop)
	// The ssh -L tunnels behind ssh/sandbox sessions (#5564). They are children
	// of THIS daemon, not of any session teardown — a session must survive the
	// daemon that hosts it, so nothing else ever stops them on exit, and a
	// clean exit would orphan them exactly like a SIGKILL does. The
	// session-side registry covers tunnels no instance points at yet (a
	// provision still in flight). The seam is read here, on the main flow.
	teardown.add("sandbox tunnels", stopSandboxTunnels)
	// An expired throwaway's session containers (#7447). A teardown step, so
	// the stop's deadline bounds it like every other: on a slow or hung Docker
	// engine it is abandoned at the deadline, named, and the stop exits holding
	// the home (Codex on #7448). Its own daemon never deregisters the home, so
	// a later marked start retries whatever this could not remove. A no-op
	// unless this daemon's sandbox expired. The seam is read here, on the main
	// flow.
	reapExpiredThrowawayContainers := throwawayHomeContainerReapFn
	teardown.add("expired throwaway sandbox containers", func() {
		if throwawayExpired.Load() {
			reapExpiredThrowawayContainers(throwawayHome)
		}
	})
	// Work this daemon starts that writes in the home and was never joined
	// (#7440): its cron deliveries (scheduled fires, the boot replay and its
	// scan, `af tasks trigger`) and the startup sandbox reattach. Each closes
	// to new runs, then waits for the running ones, within the stop's deadline;
	// a run still going then is abandoned, named, and makes the exit hold the
	// home (#7443). A parked delivery has already stopped parking: it watches
	// the drain flag.
	teardown.add("sandbox reattach", manager.reattachRuns.closeAndWait)
	// The systemd job read a SIGTERM started to learn the stop's intent
	// (#7474), cancelled and joined, so it never outlives the daemon (#7480).
	teardown.add("the stop's systemd job read", stop.endIntentRead)

	scheduler := newTaskScheduler()
	// The cron's own deliveries join first: every job the cron launched before
	// scheduler.Stop is admitted before the tracker closes (#7468 review).
	teardown.add("cron deliveries", func() { joinCronDeliveries(scheduler, deliveries) })
	watchers := newWatcherSupervisorWithEventsPerMinute(cfg.WatcherEventsPerMinute)
	watchers.observeTargetLimit = manager.observeTaskTargetLimit
	// The watch-task supervisor's Stop: SIGTERM to each watch group, group
	// SIGKILL after the grace. It is registered before the upgrade-probation
	// select below, not after it: a released upgrade candidate transitions to
	// DaemonPhaseHandoffPending while still parked in that select, and in that
	// window it admits task-mutating RPCs (AddTask/UpdateTask/ReloadTasks) that
	// arm real watcher subprocesses via watchers.reconcile -> go w.run() ->
	// cmd.Start() ($SHELL -c watch_cmd, Setpgid). The select exits via `return
	// nil` on signal or shutdown, and the reliable SIGTERM/SIGKILL group teardown
	// lives only in watchers.Stop; no startup sweep can discover orphaned
	// watch_cmd processes. A no-op if reconcile() was never called. A watcher
	// still stopping at the stop deadline (a script that ignores SIGTERM) is
	// abandoned there, and its lifeline and bound scope kill it after the exit.
	teardown.add("watch tasks", watchers.Stop)

	shutdownCh := make(chan struct{})
	resetFatalAcceptErr() // clear any fatal error from a prior daemon in this process (tests)
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
	// Every exit from here on drains both control planes under the stop's one
	// deadline (#7416). On the graceful path drainDaemon does it and sets
	// controlClosed. This covers every other exit — a signal or Shutdown during
	// the restore or upgrade probation, a failed restore — which used to call
	// closeHTTP and closeControl directly, with no bound, so a handler stuck in
	// either plane held the stop until the SIGKILL and skipped every later
	// defer. drainPlanes latches the watchers before anything in it can block:
	// a released upgrade candidate may already be running watchers armed by RPC
	// (#7392). It runs before the teardown (LIFO).
	var closeHTTP func() error
	controlClosed := false
	defer func() {
		if controlClosed {
			return
		}
		drainPlanes(manager, watchers, closeHTTP, closeControl, &controlClosed, drainDeadline(manager))
	}()

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
	// The deferred plane drain above picks closeHTTP up from here.
	if closeCandidate, err := startHTTPServer(manager, scheduler, watchers); err != nil {
		log.WarningLog.Printf("failed to start daemon HTTP server: %v", err)
	} else {
		closeHTTP = closeCandidate
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
		startupStep("dedicated tmux server", func() {
			if serverErr := sessiontmux.EnsureDaemonServer(); serverErr != nil {
				log.WarningLog.Printf("dedicated tmux server launch failed; session creation will use its fallback: %v", serverErr)
			}
		})
	}
	log.InfoLog.Printf("control socket bound; restoring instances")
	restoreDone := make(chan error, 1)
	// Capture the seam on the main flow: reading the package var inside the
	// goroutine would race with tests restoring it after RunDaemon returns.
	restore := restoreManagerForStartup
	// restoreRunning lets the stop see a restore a warm-up exit leaves behind:
	// it is never joined, so it is abandoned, and the stop exits holding the
	// home rather than let it write there after the release (#7439).
	restoreRunning.Store(true)
	go func() {
		var restoreErr error
		startupStep("restore instances", func() { restoreErr = restore(manager) })
		restoreRunning.Store(false)
		restoreDone <- restoreErr
	}()
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
		return fatalAcceptShutdownErr()
	case <-throwawayExpireCh:
		throwawayExpired.Store(true)
		return nil
	}
	// Upgrade candidates deliberately park before the orphan sweep. Preserve the
	// pre-existing probation contract: restored state is readable while every
	// mutation remains blocked by the lifecycle gate. A candidate parks whether
	// or not it is still in probation: a supervisor release that raced the
	// restore is applied by finishInstanceRestore itself (#5693), and a released
	// candidate (DaemonPhaseHandoffPending) never arms its loops — the post-commit
	// hand-off replaces it with a fresh daemon. Keying the park on probation would
	// let that candidate fall through and start cron, watchers, and the update
	// driver on a daemon that is about to be stopped.
	if manager.lifecycle.snapshot().transactionID != "" {
		manager.finishInstanceRestore()
		log.InfoLog.Printf("instance restore complete; upgrade candidate parked (phase %s)", manager.lifecycle.snapshot().phase)
		select {
		case sig := <-sigChan:
			log.InfoLog.Printf("received signal %s during upgrade probation; exiting", sig.String())
			return nil
		case <-shutdownCh:
			log.InfoLog.Printf("received shutdown request during upgrade probation; exiting")
			return fatalAcceptShutdownErr()
		case <-throwawayExpireCh:
			throwawayExpired.Store(true)
			return nil
		}
	}

	// Remove per-task timer units left behind by pre-#782 versions; the
	// in-process scheduler below replaces them.
	startupStep("legacy task unit sweep", sweepLegacyTaskUnits)

	startupStep("orphan tunnel sweep", sweepStartupOrphanSandboxTunnels)
	// After the tunnel sweep, never before: the sweep would kill the tunnels
	// reattachment opens (#5571 W1.5). The reattach itself runs in the background.
	startupStep("start sandbox reattach", manager.reattachRestoredRuntimes)
	// Arms the orphan-container sweep; it runs after readiness (see
	// sweepStartupOrphanContainers), so a hung Docker engine cannot hold the
	// daemon in "starting" (#5815).
	startupStep("arm orphan container sweep", func() { sweepStartupOrphanContainers(manager) })
	startupStep("tab cleanup sweep", func() { sweepStartupTabCleanup(manager) })
	// Reaps never-recorded sessions a lost create left behind (#6301). Here,
	// before readiness admits any create and before the fire replay below, so
	// every stamped live session it could act on belongs to a previous
	// generation — and a journaled leftover is gone before its fire reconciles.
	startupStep("unrecorded create sweep", sweepUnrecordedCreateSessions)
	// Reaps bare config-agent and account-login panes a previous generation
	// abandoned (#7428). After the restore so instance rows can claim their
	// sessions' names, and before finishInstanceRestore opens readiness: every
	// path that can create a bare pane sits behind requireStateMutationAdmission,
	// so a candidate here can only belong to a previous daemon — or to nobody.
	startupStep("bare pane sweep", manager.sweepOrphanBarePanes)
	// Background and read-only until it proves an orphan; see sandbox_dir_sweep.go.
	startupStep("sandbox orphan dir sweep", manager.sweepStartupOrphanSandboxDirs)
	// Reaps marked sibling sandboxes that are expired or proven orphaned —
	// the "next start of the tooling that creates them" net (#6844). The
	// sweep itself is gated on this daemon's own marker: a real install
	// never reaps, and teardown refuses the default home outright.
	// Their containers are reaped after readiness, on a tracked worker: that is
	// Docker work, which must not hold readiness (#5815, #7447).
	var throwawayContainerReaps []throwawayContainerReap
	startupStep("throwaway sandbox reap", func() { throwawayContainerReaps = sweepThrowawaySandboxes(throwawayHome) })
	manager.finishInstanceRestore()

	// Start task automation only after the control server is up and restore has
	// finished: a task firing immediately loops back through our own socket and
	// needs a ready manager. Revalidate every persisted target first. In
	// particular, root-agent policy takes effect on this daemon start, so a root
	// task accepted under the previous config must not be armed when the new
	// policy can no longer materialize it. One preflight owns both cron and watch:
	// on failure neither starts, and one actionable log replaces a permanent
	// per-fire retry loop (#2646).
	var taskArmErr error
	startupStep("arm task automation", func() { taskArmErr = armTaskAutomation(manager, scheduler, watchers) })
	if taskArmErr != nil {
		log.WarningLog.Printf("task automation not armed: %v", taskArmErr)
	}

	// Replay cron fires a previous daemon recorded intent for but never
	// completed (#5503). Async rather than synchronous: a targeted task's
	// deferred delivery can park until its target detaches (#1586), and boot
	// must not wait on that. RunTask's per-task flock serializes each replay
	// against a due-now cron entry of the same task. A cron entry that loses
	// coalesces, the existing same-task overlap semantics; a replay that loses
	// looks again (replayTaskFire). A held fire's reconcile asks this manager
	// whether anything still settles its Lost row (#7518).
	defer setTaskFireRowSettles(setTaskFireRowSettles(manager.lostRowSettles))
	go replayPendingTaskFires()
	scheduler.Start()
	defer scheduler.Stop()

	wg := &sync.WaitGroup{}
	stopCh := make(chan struct{})
	startInstancePollLoop(manager, time.Duration(cfg.DaemonPollInterval)*time.Millisecond, stopCh, wg)
	startHostReadinessRefresh(manager, stopCh)
	// The account-limit proof reconcile owns a durable scan far too heavy for
	// the 1 Hz poll goroutine; it runs on its own ticker (#6776 1d).
	startAccountLimitReconcileLoop(manager, stopCh, wg)
	// The shadow decision engine runs on its own ~45s ticker for the same
	// reason; it only reads the status snapshot the poll already produced.
	startAccountHandoffEngineLoop(manager, stopCh, wg)
	// The report_to worker owns prompt delivery so sends never block the
	// decision ticker (R15).
	startHandoffReportLoop(manager, stopCh, wg)

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
	// The destructive Docker orphan pass was deferred past the readiness barrier
	// (it can spend 30s per Docker call on an unavailable engine). Launch it now
	// rather than waiting for the first poll tick; createSweepMu keeps a create
	// admitted after this point out of the protected-slug window (#2632).
	launchDeferredOrphanSweepIfReady(manager, stopCh, wg)
	// Finish a prune the previous daemon began (#5189): bounded, logged, off readiness.
	launchPruneIntentResume(manager, stopCh, wg)
	// The containers of the throwaway siblings the startup sweep tore down.
	launchThrowawayContainerReaps(throwawayContainerReaps, stopCh, wg)

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
	// ends the daemon (channels armed before the restore above).
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
	case <-throwawayExpireCh:
		throwawayExpired.Store(true)
	}

	joined := drainDaemon(manager, watchers, closeHTTP, closeControl, &controlClosed, stopCh, wg)

	// A fire that closed throwawayExpireCh while drainDaemon was joining can
	// never have been received by the main select above (the stop case already
	// won, and the join can run for drainBudget). The boolean approximates the
	// channel's state; re-drain the channel non-blockingly before the gates
	// that read it, so a close that lost the race is still observed. The
	// expired-container reap step reads the flag in a teardown defer that runs
	// after this return, so the Store here is seen by that step's goroutine
	// (the spawn happens after this write). endThrowaway re-drains again to
	// cover the deferred and abandoned-stop paths.
	select {
	case <-throwawayExpireCh:
		throwawayExpired.Store(true)
	default:
	}

	if homeGone {
		// Skip the final save: the home directory was deleted out from under
		// us, so there is no installation left to persist into — saving would
		// recreate a skeleton of the deleted home and resurrect the abandoned
		// state the deletion was meant to remove.
		return nil
	}
	if throwawayExpired.Load() {
		// Same skip, for the same reason: the unwind-time teardown is about
		// to empty this sandbox, so persisting live-session state into it
		// would only be restored by a daemon that expires on the same marker.
		return nil
	}

	// A drain that left a handler or a joined goroutine running writes
	// nothing: whatever it holds, the final save would queue behind it past the
	// SIGKILL grace, and a write taken while admitted writers can still commit
	// is unsound. drainDaemon logged what was left behind (#5846, #7393).
	checkpointAfterDrain(manager, joined)
	return fatalAcceptShutdownErr()
}
