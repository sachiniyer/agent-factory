package daemon

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sachiniyer/agent-factory/log"
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

// opLockTimeout bounds how long KillSession waits for a session's operation lock
// before giving up. The lock serializes a kill against an in-flight Recover
// (#1108) and that exclusion is load-bearing — a kill must never interleave with
// a respawn — so this stays a real mutual exclusion and NOT a race. What changes
// is only the failure mode: an unbounded Lock() turns a peer's slow operation
// into a permanent wedge of this session, while a bounded wait turns it into a
// retryable error the user can act on.
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
// It polls TryLock because sync.Mutex has no timed acquire. The cost is a wakeup
// every opLockPollInterval while contended, and the loss of the mutex's
// starvation-avoidance fairness (TryLock never queues), so a caller can in
// principle lose several races to a lock that is handed off rapidly. That is
// acceptable here and nowhere near the hot path: the only bounded acquirer is a
// user-initiated kill, the poll cost lasts only as long as the contention, and
// the alternative it replaces is waiting forever.
func lockWithin(mu *sync.Mutex, d time.Duration) bool {
	if mu.TryLock() {
		return true
	}
	deadline := time.Now().Add(d)
	for {
		wait := opLockPollInterval
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining
		}
		if wait > 0 {
			time.Sleep(wait)
		}
		if mu.TryLock() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
	}
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
//	× maxTabs (9)                                                          = 387s
//	Cleanup   5 bounded git commands × 60s (remove, list, prune,
//	          branch -D, prune)                                            = 300s
//	vscode    2 × (5s SIGTERM grace + 5s SIGKILL grace)                    =  20s
//	flocks    tombstone write 10s + record delete 10s                      =  20s
//	                                                                        ------
//	worst-case LEGITIMATE teardown                                          ≈ 727s
//
// 45s — the old value — fired on any wedged-tmux teardown with two or more tabs,
// all of it bounded and correct. 15 minutes clears the sum with headroom, and that
// is the right shape for what this actually watches: every KNOWN wait is now
// bounded, so the watchdog exists for the unknown-unknowns. Exceeding every bound
// we know of is precisely the wedge worth dumping stacks for. Firing late costs
// only a later log; firing early costs the signal itself.
//
// If any bound above grows, this must grow with it. A var so tests can shorten it.
var killWatchdogDelay = 15 * time.Minute

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

// watchKill arms a watchdog that reports the in-flight stage — and, by default,
// goroutine stacks — if the kill has not finished within killWatchdogDelay. It
// returns a stop function the caller must defer.
//
// It only OBSERVES: it never cancels the kill and never touches its locks.
// Bounding is the leaf steps' job; this exists so that if a wedge outlives them
// anyway, the next field report carries the wedge point instead of the guesswork
// #1917 had to work from. Off the hot path by construction — a kill that
// finishes normally stops the timer having done nothing.
func watchKill(title string, stage *killStage) (stop func()) {
	done := make(chan struct{})
	var once sync.Once
	go func() {
		select {
		case <-done:
			return
		case <-time.After(killWatchdogDelay):
		}
		log.WarningLog.Printf("kill of session %q has been running for over %s, stuck in stage %q; this is the #1917 wedge shape — the session cannot be killed or acted on until this returns", title, killWatchdogDelay, stage.get())
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
