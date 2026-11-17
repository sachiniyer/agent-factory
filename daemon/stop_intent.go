package daemon

import (
	"time"

	"github.com/sachiniyer/agent-factory/internal/systemdunit"
)

// stopIntentBound is the hard bound on settling a stop's intent (#7474). A
// stop never waits longer than this to learn whether its stopper starts a
// successor, and an answer still missing then is a final stop.
const stopIntentBound = 500 * time.Millisecond

// stopSignalIsRestartFn answers, for a SIGTERM, whether the stopper starts the
// daemon's successor. Only systemd can say so: a daemon running as the
// autostart unit asks for the unit's pending job, and a restart job means a
// restart. Anything else, including a daemon outside the unit, an absent
// systemctl, an error or a timeout, is a final stop (fail-closed). A hand-run
// `launchctl kickstart -k` on macOS is a bare SIGTERM with no such job to read,
// so it reads as a final stop too. A seam for tests.
var stopSignalIsRestartFn = func() bool {
	if !systemdunit.RunningDaemonProcess() {
		return false
	}
	return systemdunit.RestartJobPending(systemdunit.DaemonUnitName, stopIntentBound)
}

// declareIntent settles the stop's intent now, unless something named it
// first: the Shutdown RPC's own field, or a SIGINT's final stop. nil-safe.
func (c *stopClock) declareIntent(restart bool) {
	settled := c.claimIntent()
	if settled == nil {
		return
	}
	c.settleIntent(settled, restart)
}

// settleIntentAsync settles the stop's intent with resolve's answer, read on
// its own goroutine, unless something named it first. The answer is taken
// only within stopIntentBound: one still missing then settles a final stop,
// whatever resolve does afterwards. nil-safe.
func (c *stopClock) settleIntentAsync(resolve func() bool) {
	settled := c.claimIntent()
	if settled == nil {
		return
	}
	go func() {
		answer := make(chan bool, 1)
		go func() { answer <- resolve() }()
		timer := time.NewTimer(stopIntentBound)
		defer timer.Stop()
		restart := false
		select {
		case restart = <-answer:
		case <-timer.C:
		}
		c.settleIntent(settled, restart)
	}()
}

// claimIntent returns the channel that announces the intent's settling, or nil
// when something already named the intent. nil-safe.
func (c *stopClock) claimIntent() chan struct{} {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.intentSettled != nil {
		return nil
	}
	c.intentSettled = make(chan struct{})
	return c.intentSettled
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
