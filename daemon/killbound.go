package daemon

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// Bounding the kill path (#1917).
//
// KillSession registers a session in killsInFlight and only ever removes it on
// return, so ANY unbounded wait between those two points does not merely stall
// one kill: every later kill is rejected with "kill already in progress", every
// other action with "session is being deleted", and the session stays on screen,
// undeletable, for the daemon's whole lifetime. In the field it took a daemon
// restart to reap it.
//
// The rule this file enforces is that no step of a kill may wait forever. Two
// distinct bounds, because the two halves of the kill fail differently:
//
//   - BEFORE the kill-intent tombstone is committed, a timeout is a clean no-op:
//     nothing durable has changed, so we can refuse with a retryable error and
//     leave the session exactly as it was. That is opLockTimeout below.
//   - AFTER the tombstone, the kill is committed and must not be abandoned
//     silently. The teardown's own leaf steps are individually bounded (tmux and
//     git subprocesses, the instances flock), so it terminates on its own; the
//     watchdog here exists to make a slow one EXPLAIN itself rather than to
//     interrupt it.
//
// Why a tripped teardown does not strand a tombstoned record: refreshInstanceStatus
// routes any record carrying the tombstone to finishUserKill on EVERY poll
// (manager_status.go), and that finisher completes the teardown and deletes the
// record. It is skipped for exactly one reason — killsInFlight still being held
// (rootkill.go). So the wedge starved the very mechanism built to heal it. Once
// KillSession is guaranteed to RETURN, the guard is released and the next poll
// finishes the kill with no daemon restart. Bounding the wait is what re-arms
// the self-heal; the two are the same fix.

// opLockTimeout bounds how long a kill, archive, or manual restore waits for a
// session's operation lock before giving up. The lock serializes these actions
// against an in-flight Recover (#1108) and that exclusion is load-bearing — they
// must never interleave with a respawn — so this stays a real mutual exclusion
// and NOT a race. What changes is only the failure mode: an unbounded Lock()
// inside killsInFlight turns a peer's slow operation into a permanent wedge of
// this session, while a bounded wait releases the guard with a retryable error.
//
// Generous on purpose: a healthy Recover holds this lock for as long as a tmux
// respawn takes, and a kill that waits a few seconds behind one is CORRECT
// behavior, not a bug. Exceeding this means the holder is wedged, not busy.
//
// A var so tests can shorten it; production never reassigns.
var opLockTimeout = 30 * time.Second

// opLockPollInterval is how often lockWithin re-attempts a contended op lock.
var opLockPollInterval = 5 * time.Millisecond

// lockWithin acquires mu, giving up after d and reporting whether it got the
// lock. It preserves mutual exclusion exactly — a true result means the caller
// holds the lock and must Unlock it, the same contract as mu.Lock().
//
// It also reports how long the acquisition actually WAITED, and zero is a
// distinct answer rather than a rounded one: the first TryLock either succeeds —
// uncontended, nobody was ahead of us — or the lock was genuinely held by a peer
// and the wait is at least one poll interval. #3600's restore refusals are
// decided after this wait, so they need to say whether there was one; a caller
// that does not care ignores it.
//
// It polls TryLock because sync.Mutex has no timed acquire. The cost is a wakeup
// every opLockPollInterval while contended, and the loss of the mutex's
// starvation-avoidance fairness (TryLock never queues), so a caller can in
// principle lose several races to a lock that is handed off rapidly. That is
// acceptable here and nowhere near the hot path: the bounded acquirers are
// user-initiated lifecycle operations, the poll cost lasts only as long as the
// contention, and the alternative it replaces is waiting forever.
func lockWithin(mu *sync.Mutex, d time.Duration) (bool, time.Duration) {
	if mu.TryLock() {
		return true, 0
	}
	start := time.Now()
	deadline := start.Add(d)
	for {
		wait := opLockPollInterval
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining
		}
		if wait > 0 {
			time.Sleep(wait)
		}
		if mu.TryLock() {
			return true, time.Since(start)
		}
		if !time.Now().Before(deadline) {
			return false, time.Since(start)
		}
	}
}

// lockSessionOperationWithin takes the per-session operation lock with the same
// bound for archive and both manual restore paths. No caller has mutated the
// session yet, so timeout is a known no-op: the requested action did not start
// and a later kill or retry remains possible (#2641).
//
// The two kinds of caller differ in what they hold across this wait, and that is
// #3600. Archive registers in killsInFlight first, so its timeout also has to
// release that guard — which its deferred cleanup does. Both restore paths take
// this lock BEFORE claiming anything, so their wait holds nothing at all: the row
// is unclaimed and unfenced for its whole duration, which is what keeps the Kill
// it advertises admissible. It returns how long it waited so those callers can
// say so in a refusal that is now decided at the END of the wait.
func (m *Manager) lockSessionOperationWithin(key, operation, title string) (*sync.Mutex, time.Duration, error) {
	opLock := m.opLockFor(key)
	acquired, waited := lockWithin(opLock, opLockTimeout)
	if !acquired {
		log.WarningLog.Printf("%s of session %q could not acquire its operation lock within %s; another operation on this session is not releasing it", operation, title, opLockTimeout)
		return nil, waited, fmt.Errorf("%s of session %q timed out after %s waiting for another operation on it to finish; the requested %s did not start and made no changes — retry", operation, title, opLockTimeout, operation)
	}
	return opLock, waited, nil
}

// killWatchdogDelay is how long a kill may run before the watchdog reports the
// stage it is stuck on. It must sit BEYOND every bound a legitimate teardown may
// legally spend, or it cries wolf and its diagnostics become noise — a watchdog
// that fires on healthy work is worse than none, because the next real wedge is
// read as another false positive (#1917 round 8).
//
// The budget, summed from the bounds this PR put in place:
//
//	per tab   10s panePID + 10s list-panes + 10s kill-session + 10s has-session
//	          + 3s pane-exit wait                                          =  43s
//	× the session's OWN tab count            (9 tabs, the old cap: 387s)
//	Cleanup   5 bounded git commands × 60s (remove, list, prune,
//	          branch -D, prune)                                            = 300s
//	vscode    2 × (5s SIGTERM grace + 5s SIGKILL grace)                    =  20s
//	flocks    tombstone write 10s + record delete 10s                      =  20s
//	                                                                        ------
//	fixed terms                                                             = 340s
//	worst-case LEGITIMATE teardown                              340s + 43s × tabs
//
// The per-tab term used to be multiplied by a constant, because tabs were capped
// at 9 (#930). #3023 removed that cap — the keyboard should not decide how many
// tabs a session may hold — so this had to stop being a constant too. A fixed 15
// minutes would have been correct for 9 tabs and a false alarm for 30: the
// watchdog would fire on a teardown that was entirely legitimate and bounded,
// which is exactly the cry-wolf failure the paragraph above rejects.
//
// 45s — the old value — fired on any wedged-tmux teardown with two or more tabs,
// all of it bounded and correct. 15 minutes clears the sum with headroom, and that
// is the right shape for what this actually watches: every KNOWN wait is now
// bounded, so the watchdog exists for the unknown-unknowns. Exceeding every bound
// we know of is precisely the wedge worth dumping stacks for. Firing late costs
// only a later log; firing early costs the signal itself.
//
// If any bound above grows, these must grow with them.
const (
	// killWatchdogPerTab is the summed per-tab bound from the table above.
	killWatchdogPerTab = 43 * time.Second
	// killWatchdogFixed is every term that does not scale with the roster.
	killWatchdogFixed = 340 * time.Second
	// killWatchdogCountAttempts and killWatchdogCountRetryDelay bound how long the
	// arming path will try to read a contended roster. The total is deliberately
	// tiny — this runs while the kill holds its locks — and exists only to ride out
	// ordinary contention, never to wait out a wedge.
	killWatchdogCountAttempts   = 4
	killWatchdogCountRetryDelay = 250 * time.Microsecond
)

// killWatchdogFloor is the shortest this watchdog will ever wait, and it is the
// value the constant used to hold. Keeping it as a floor means no session that was
// under the old 9-tab cap changes behaviour at all. A var so tests can shorten it.
var killWatchdogFloor = 15 * time.Minute

// killWatchdogDelayFor bounds a legitimate teardown of a session with this many
// tabs, plus the same ~25% headroom the old flat 15 minutes gave the 9-tab sum
// (727s → 900s). Firing late costs a later log; firing early costs the signal.
func killWatchdogDelayFor(tabs int) time.Duration {
	if tabs < 1 {
		tabs = 1 // a session always has its agent tab, even if the roster is unread
	}
	budget := killWatchdogFixed + time.Duration(tabs)*killWatchdogPerTab
	budget += budget / 4
	if budget < killWatchdogFloor {
		return killWatchdogFloor
	}
	return budget
}

// killStageDumpsStacks controls whether the watchdog appends goroutine stacks to
// its report. The stacks are the evidence #1917 could not get from the field —
// two occurrences, and no way to tell WHICH teardown step was wedged — so they
// are on by default. A var so tests can disable them and keep output readable.
var killStageDumpsStacks = true

// killStage tracks which teardown step a kill is currently in, so a watchdog
// firing later can name it. Written by the kill goroutine, read by the watchdog:
// an atomic, not a mutex, so the tracking itself can never become one more thing
// that blocks the path it is supposed to be diagnosing.
type killStage struct {
	current atomic.Value // string
}

func (s *killStage) set(stage string) { s.current.Store(stage) }

func (s *killStage) get() string {
	if v, ok := s.current.Load().(string); ok {
		return v
	}
	return "starting"
}

// killWatchdogTabCount reports how many tabs this kill may have to tear down,
// taken from whichever record actually exists.
//
// It counts TMUX SESSIONS a teardown must close, not roster entries, and the two
// differ in both directions. teardownTabs kills exactly the live tabs whose
// tab.tmux is non-nil — web and vscode tabs own none, and the shared vscode
// process is already in the fixed term — but it also appends every PENDING CLEANUP
// handle to the same sequential loop (#2669).
//
// Both directions matter now that the roster is unbounded. Charging the whole
// roster would push the watchdog out ten minutes for a session of iframe tabs that
// tears down nothing, delaying the wedge diagnostics this exists to produce;
// ignoring the pending handles would under-budget a session that has accumulated
// them and fire the watchdog on a teardown still inside its documented bounds.
//
// It must not dereference the instance. A title-based kill can resolve a persisted
// session that could NOT be reconstructed — resolveActionSession deliberately
// returns a nil instance with non-nil data, and the ghost branch later cleans the
// record up — so reading the count off the instance would panic the daemon on
// exactly the path that exists to tidy up after a session that no longer runs.
// Zero is the honest answer when neither record survives: the delay floor still
// applies, so the watchdog is never armed shorter than it used to be.
func killWatchdogTabCount(instance *session.Instance, data *session.InstanceData) int {
	// Never BLOCKS, but does retry briefly, and the two together are the point. The
	// argument to a deferred call is evaluated AT the defer statement, so this runs
	// while the kill already holds killsInFlight and the operation lock: waiting on
	// the instance lock would stall the arming on exactly the stuck-lock wedge the
	// watchdog exists to report. But giving up on the first contended read loses the
	// roster scale for a TRACKED session — resolveActionSession returns no persisted
	// record for one — and a >9-tab teardown would then be judged against the bare
	// floor and reported as a false wedge.
	//
	// A bounded retry separates those without having to tell them apart: ordinary
	// contention clears in microseconds, and a wedge never clears at all.
	if instance != nil {
		for attempt := 0; attempt < killWatchdogCountAttempts; attempt++ {
			if n, ok := instance.TryTmuxTeardownCount(); ok {
				return n
			}
			if attempt < killWatchdogCountAttempts-1 {
				time.Sleep(killWatchdogCountRetryDelay)
			}
		}
		// Genuinely stuck. Arm on the floor rather than wait: a watchdog that fires
		// somewhat early on a huge roster is still a watchdog, and one that never
		// arms at all is not.
		return 0
	}
	if data != nil {
		// Asked of the SAME function the ghost teardown iterates, rather than
		// reconstructed from the same fields. ghostCleanup loops over
		// ghostTmuxNames(data), which collects the session's own tmux session plus
		// each live tab's and each pending-cleanup handle's, then DEDUPLICATES them.
		// Every property has to hold here too: the watchdog must budget every tmux
		// kill ghostCleanup will actually attempt, without double-charging a
		// post-#953 agent name stored in both data.TmuxName and data.Tabs[0]. Calling
		// the real enumerator keeps the budget from drifting from the teardown.
		return len(ghostTmuxNames(data))
	}
	return 0
}

// watchKill arms a watchdog that reports the in-flight stage — and, by default,
// goroutine stacks — if the kill has not finished within killWatchdogDelay. It
// returns a stop function the caller must defer.
//
// It only OBSERVES: it never cancels the kill and never touches its locks.
// Bounding is the leaf steps' job; this exists so that if a wedge outlives them
// anyway, the next field report carries the wedge point instead of the guesswork
// #1917 had to work from. Off the hot path by construction — a kill that
// finishes normally stops the timer having done nothing.
func watchKill(title string, tabs int, stage *killStage) (stop func()) {
	done := make(chan struct{})
	var once sync.Once
	delay := killWatchdogDelayFor(tabs)
	go func() {
		select {
		case <-done:
			return
		case <-time.After(delay):
		}
		log.WarningLog.Printf("kill of session %q has been running for over %s, stuck in stage %q; this is the #1917 wedge shape — the session cannot be killed or acted on until this returns", title, delay, stage.get())
		if killStageDumpsStacks {
			log.WarningLog.Printf("kill of session %q: goroutine stacks at the wedge:\n%s", title, goroutineStacks())
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

// goroutineStacks renders all goroutine stacks, capped so a daemon with many
// sessions cannot emit an unbounded log record. Only ever called from the
// watchdog, after a kill has already exceeded its budget.
func goroutineStacks() string {
	const maxStacks = 1 << 20 // 1 MiB — enough for the blocked goroutines that matter
	buf := make([]byte, maxStacks)
	n := runtime.Stack(buf, true)
	if n == maxStacks {
		return string(buf[:n]) + "\n… (truncated)"
	}
	return string(buf[:n])
}

// errKillBusy builds the actionable error a bounded op-lock acquisition returns.
// It has to answer the two questions the old wedge left the user guessing at:
// whether anything was destroyed (no — this fires before the tombstone, so the
// session is untouched), and what to do next (retry; the holder is named so a
// genuinely stuck restore can be killed rather than waited on).
func errKillBusy(title string, d time.Duration) error {
	return fmt.Errorf("kill of session %q timed out after %s waiting for another operation on it to finish (most likely a restore/recovery re-spawning it); nothing was torn down and the session is unchanged — retry the kill", title, d)
}
