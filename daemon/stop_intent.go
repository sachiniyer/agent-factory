package daemon

import (
	"context"
	"os"
	"time"

	"github.com/sachiniyer/agent-factory/internal/systemdunit"
	"github.com/sachiniyer/agent-factory/log"
)

// stopIntentBound is the hard bound on settling a stop's intent (#7474). A
// stop never waits longer than this to learn whether its stopper starts a
// successor, and an answer still missing then is a final stop. Only tests
// lengthen it.
var stopIntentBound = 500 * time.Millisecond

// stopSignalIsRestartFn answers, for a SIGTERM, whether the stopper starts the
// daemon's successor. Only systemd can say so: a daemon that is the autostart
// unit's main process asks for the unit's pending job, and a restart job means
// a restart. Anything else, including a daemon outside the unit, a process in
// its cgroup that is not its MainPID (#7480), an absent systemctl, an error or
// the end of ctx, is a final stop (fail-closed). A hand-run
// `launchctl kickstart -k` on macOS is a bare SIGTERM with no such job to read,
// so it reads as a final stop too. A seam for tests.
var stopSignalIsRestartFn = func(ctx context.Context) bool {
	if !systemdunit.RunningDaemonProcess() {
		return false
	}
	return systemdunit.RestartJobPending(ctx, systemdunit.DaemonUnitName, os.Getpid())
}

// declareIntent settles the stop's intent now, unless something named it
// first: the Shutdown RPC's own field, or a SIGINT's final stop. nil-safe.
func (c *stopClock) declareIntent(restart bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.intentSettled != nil {
		c.mu.Unlock()
		return
	}
	settled := make(chan struct{})
	c.intentSettled = settled
	c.mu.Unlock()
	c.settleIntent(settled, restart)
}

// settleIntentAsync settles the stop's intent with resolve's answer, read on
// its own goroutine, unless something named it first. The read runs under a
// context that ends at stopIntentBound, and the answer is taken only within
// that bound: one still missing then settles a final stop. The stop owns the
// read and ends it before the daemon exits (endIntentRead, #7480). A read the
// stop has already ended never starts, and settles a final stop. nil-safe.
func (c *stopClock) settleIntentAsync(resolve func(context.Context) bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.intentSettled != nil {
		c.mu.Unlock()
		return
	}
	settled := make(chan struct{})
	c.intentSettled = settled
	if c.intentReadsEnded {
		c.mu.Unlock()
		c.settleIntent(settled, false)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), stopIntentBound)
	readDone := make(chan struct{})
	c.intentReadCancel, c.intentReadDone = cancel, readDone
	c.mu.Unlock()

	answer := make(chan bool, 1)
	go func() {
		defer close(readDone)
		answer <- resolve(ctx)
	}()
	go func() {
		restart := false
		select {
		case restart = <-answer:
		case <-ctx.Done():
		}
		c.settleIntent(settled, restart)
	}()
}

// endIntentRead ends the stop's systemd job read, if one is still running
// (#7480): it cancels the read's context, which kills its systemctl, and waits
// for the read within stopIntentBound. A stop that finished faster than the
// read would otherwise exit with systemctl still running, and on a wedged
// systemd that lingers until systemctl's own D-Bus timeout. Any read asked for
// after this never starts. A teardown step, so it runs on the ordinary return
// and before the exit that holds the home. nil-safe.
func (c *stopClock) endIntentRead() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.intentReadsEnded = true
	cancel, done := c.intentReadCancel, c.intentReadDone
	c.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	timer := time.NewTimer(stopIntentBound)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		log.WarningLog.Printf("shutdown: the stop's systemd job read did not end within %s of its cancellation", stopIntentBound)
	}
}

func (c *stopClock) settleIntent(settled chan struct{}, restart bool) {
	c.mu.Lock()
	c.restart = restart
	c.mu.Unlock()
	close(settled)
}

// restartIntended reports whether the stopper starts this daemon's successor.
// It waits for an intent still settling, which stopIntentBound bounds; a stop
// whose intent nothing named is a final stop. nil-safe.
func (c *stopClock) restartIntended() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	settled := c.intentSettled
	c.mu.Unlock()
	if settled == nil {
		return false
	}
	<-settled
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.restart
}
