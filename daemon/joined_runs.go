package daemon

import (
	"sync"
	"sync/atomic"
)

// joinedRuns tracks work a stop must join before it can release the home
// (#7440): cron deliveries, and the sandbox reattach. Each run takes a slot for
// as long as it can write; the stop closes the tracker, which refuses every new
// run, then waits for the ones already running. That wait is a teardown step,
// so the stop's deadline bounds it, and a run still going then is abandoned,
// named, and entered in the stop's ledger (#7443).
type joinedRuns struct {
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

// begin takes a slot, or refuses with ok false once the tracker is closed. done
// releases the slot; on a refusal it is a no-op, so a caller that defers it
// before checking ok cannot panic. A nil tracker, one no daemon installed,
// admits every run untracked.
func (r *joinedRuns) begin() (done func(), ok bool) {
	if r == nil {
		return func() {}, true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return func() {}, false
	}
	r.wg.Add(1)
	var once sync.Once
	return func() { once.Do(r.wg.Done) }, true
}

// closeAndWait refuses every new run, then waits for the running ones.
func (r *joinedRuns) closeAndWait() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.wg.Wait()
}

// joinCronDeliveries is the "cron deliveries" teardown step (#7440). It waits
// first for every job the cron launched before scheduler.Stop, so each one is
// admitted and records its fire marker before the tracker closes (#7468
// review), then closes the tracker and joins every delivery still running. The
// stop's deadline bounds both waits: it bounds the step.
func joinCronDeliveries(scheduler *taskScheduler, deliveries *joinedRuns) {
	scheduler.waitForLaunchedJobs()
	deliveries.closeAndWait()
}

// taskDeliveries is the running daemon's cron delivery tracker, installed by
// runDaemon and cleared on its return. Every cron delivery — a scheduled fire,
// a boot replay, `af tasks trigger` — passes runTaskDelivery, so it is tracked
// there.
var taskDeliveries atomic.Pointer[joinedRuns]

// beginTaskDelivery takes a delivery slot from the running daemon's tracker.
func beginTaskDelivery() (done func(), ok bool) {
	return taskDeliveries.Load().begin()
}
