package daemon

import (
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// A daemon stop has one deadline (#7416). It starts when the stop is requested
// — a stop signal's receipt, or the Shutdown RPC's acceptance — because that is
// when the stopper's own clock starts, and it ends exitReserve short of the
// stopper's SIGKILL. The drain, the final save and the teardown all draw on it:
// the drain takes at most drainBudget, the save its reserve, and the teardown
// whatever is left, never less than teardownFloor.
//
// The teardown used to unwind one defer at a time with no bound. Each of its
// steps waits on multi-second graces of its own (a watch script that ignores
// SIGTERM held watchers.Stop for 5s, an editor for 10s, a tmux close against a
// wedged server for about 24s), so one slow step pushed every later step past
// the SIGKILL, and the later ones were the config-agent and login panes that
// nothing reaps after a SIGKILL.

const (
	// teardownFloor is the least the teardown is given, even when the drain and
	// the save have spent everything before it.
	teardownFloor = 100 * time.Millisecond

	// exitReserve is what a stop keeps back for exiting after the teardown:
	// releasing the home latch and the home flock. It also absorbs the
	// signal-delivery latency between the stopper's clock starting and this
	// daemon stamping the stop, which is the kernel's delivery and one
	// goroutine wakeup. removeDaemonPIDFile is not in it: it runs after the
	// flock is released, and a SIGKILL there leaves only a stale daemon.pid,
	// which the stale-PID paths own (#5188).
	exitReserve = 100 * time.Millisecond

	// defaultStopTeardownBy is how long after the stop began the teardown must
	// be done.
	defaultStopTeardownBy = sigtermFallbackGrace - exitReserve
)

// Pin (#7416): the worst stop — the whole drain budget, a floor for each later
// drain step, the save's reserve — still leaves the teardown its floor and the
// exit its reserve inside the stopper's grace. It fails to build when that
// breaks, because a negative constant cannot be converted to uint. The drain's
// own pins are in daemon_shutdown.go.
const _ = uint(sigtermFallbackGrace - (defaultDrainBudget + drainPlaneSteps*drainStepFloor + drainSaveReserve + teardownFloor + exitReserve))

// stopTeardownBy is initialised from defaultStopTeardownBy; only tests shorten
// it, so the pin above holds over what ships.
var stopTeardownBy = defaultStopTeardownBy

// stopClock records when this daemon's stop began: the first of a stop
// signal's receipt, the Shutdown RPC's acceptance, or, for a stop the daemon
// decides itself, the first time anything asks. Later stamps are ignored, so
// the deadline never moves later.
type stopClock struct {
	once sync.Once
	at   time.Time
}

// begin stamps the stop now, unless it was already stamped. nil-safe.
func (c *stopClock) begin() {
	if c == nil {
		return
	}
	c.once.Do(func() { c.at = time.Now() })
}

// began returns when the stop began, stamping it now if nothing has. A nil
// clock (a manager no daemon installed one on) says now.
func (c *stopClock) began() time.Time {
	if c == nil {
		return time.Now()
	}
	c.begin()
	return c.at
}

// relayStopSignals stamps stop as each signal arrives on raw, then hands the
// signal on to out, until done closes. The hand-off never blocks: a signal that
// finds out full is dropped, as signal.Notify itself drops one that finds a
// full channel.
func relayStopSignals(raw <-chan os.Signal, out chan<- os.Signal, stop *stopClock, done <-chan struct{}) {
	for {
		select {
		case sig := <-raw:
			stop.begin()
			select {
			case out <- sig:
			default:
			}
		case <-done:
			return
		}
	}
}

// daemonTeardown stops what this daemon started: watch scripts, tunnels,
// config agents, login panes and editors (#7416). Its steps are independent of
// each other, so they run in parallel, and the whole teardown ends at the
// stop's deadline. A step still running then is abandoned and named in the
// log, and dies with the process: what a SIGKILL at that instant would have
// done to it, except that every other step has run, and the log says which
// one did not finish.
type daemonTeardown struct {
	mu    sync.Mutex
	steps []teardownStep
}

type teardownStep struct {
	what string
	run  func()
}

// add registers a step. runDaemon adds each supervisor's stop as it builds
// the supervisor, so an early return tears down whatever exists by then.
func (t *daemonTeardown) add(what string, run func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.steps = append(t.steps, teardownStep{what: what, run: run})
}

// run starts every step at once and waits for them until deadline, but never
// less than teardownFloor. It returns the steps it abandoned, in the order
// they were added.
func (t *daemonTeardown) run(deadline time.Time) (abandoned []string) {
	t.mu.Lock()
	steps := t.steps
	t.steps = nil
	t.mu.Unlock()
	if len(steps) == 0 {
		return nil
	}
	start := time.Now()
	done := make([]chan struct{}, len(steps))
	for i, st := range steps {
		done[i] = make(chan struct{})
		go func(run func(), done chan<- struct{}) {
			defer close(done)
			run()
		}(st.run, done[i])
	}
	timer := time.NewTimer(max(time.Until(deadline), teardownFloor))
	defer timer.Stop()
	expired := false
	for i, d := range done {
		if !expired {
			select {
			case <-d:
				continue
			case <-timer.C:
				expired = true
			}
		}
		select {
		case <-d:
		default:
			abandoned = append(abandoned, steps[i].what)
		}
	}
	if len(abandoned) > 0 {
		log.WarningLog.Printf("shutdown: the teardown did not finish by the stop deadline; exiting with %s still running, "+
			"as a SIGKILL at this moment would have left it (#7416)", strings.Join(abandoned, ", "))
		return abandoned
	}
	log.InfoLog.Printf("shutdown: teardown finished in %s", time.Since(start).Round(time.Millisecond))
	return nil
}
