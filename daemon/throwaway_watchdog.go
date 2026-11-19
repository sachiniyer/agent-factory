package daemon

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/throwawaysandbox"
	"github.com/sachiniyer/agent-factory/log"
)

// armThrowawayWatchdog starts the sandbox's self-enforcement: fire closes
// when the marker's deadline passes or, when an owner was declared, the
// owner is proven dead. The returned func settles the fire decision: once
// it returns the outcome is final — fired, or never will (#7620). It never
// joins on an owner lookup, because darwin's kern.proc reads can block: the
// lookup runs on a worker so the loop can always answer stop, and a read
// still in flight at stop is simply never decided — the same outcome as a
// deadline that had not yet armed. An already-expired marker fires
// immediately — a sandbox discovered past its deadline does not get a
// second start.
func armThrowawayWatchdog(m *throwawaysandbox.Marker, fire chan struct{}) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once
	// Directions are the contract (#7620 review): the watchdog's end can
	// only close fire, the settle func's end can only read it.
	fireOut := (chan<- struct{})(fire)
	fireIn := (<-chan struct{})(fire)
	expire := func(reason string) {
		once.Do(func() {
			log.InfoLog.Printf("throwaway sandbox (kind %s): %s; ending the daemon and tearing the sandbox down", m.Kind, reason)
			close(fireOut)
		})
	}
	// Every function var this goroutine (or a lookup worker it spawns) reads
	// resolves NOW, before it starts: a worker whose lookup is still in
	// flight at the stop outlives this daemon, and it must not read a
	// package var a test may already have restored (#7620).
	now, checkInterval := throwawayNowFn, throwawayCheckInterval
	scopeIDs, ownerLookup := throwawayScopeIDsFn, throwawayLookupFn
	go func() {
		defer close(done)
		wait := m.Deadline().Sub(now())
		if wait <= 0 {
			expire(fmt.Sprintf("creation deadline passed %s ago", (-wait).Round(time.Second)))
			return
		}
		deadline := time.NewTimer(wait)
		defer deadline.Stop()
		var ownerC <-chan time.Time
		var ownerTick *time.Ticker
		if m.OwnerPID > 0 {
			ownerTick = time.NewTicker(checkInterval)
			defer ownerTick.Stop()
			ownerC = ownerTick.C
		}
		// At most one owner read is ever in flight: a tick that finds one
		// pending waits on IT rather than stacking another, so a stuck
		// kern.proc read costs this goroutine nothing beyond its own
		// answer.
		var ownerDead chan bool
		for {
			var ownerResult <-chan bool
			if ownerDead != nil {
				ownerResult = ownerDead
			}
			select {
			case <-stop:
				// A decision already made is still a real expiry and must
				// not be lost at exit: collect a landed deadline or a
				// finished owner read before going (#7620). The checks are
				// sequential rather than one select — a select could let a
				// completed negative owner read win the coin flip over an
				// already-ready deadline (Codex).
				select {
				case <-deadline.C:
					expire(fmt.Sprintf("reached its %ds lifetime", m.MaxLifetimeSeconds))
					return
				default:
				}
				// One LAST look at the owner, bounded and never joined
				// (#7620 R2): an owner already dead when the stop began is
				// still observed when a read can answer inside the bound —
				// a read pending gets it, none pending spawns one — and a
				// wedged kern.proc read still resolves to "never will".
				if ownerDead == nil && m.OwnerPID > 0 {
					ownerDead = make(chan bool, 1)
					go func() { ownerDead <- ownerDeadBy(m, scopeIDs, ownerLookup) }()
				}
				if ownerDead != nil {
					select {
					case dead := <-ownerDead:
						if dead {
							expire(fmt.Sprintf("declared owner pid %d is gone", m.OwnerPID))
							return
						}
					case <-time.After(throwawayOwnerSettleBound):
					}
				}
				// A deadline that landed during the last look is still an
				// already-made decision.
				select {
				case <-deadline.C:
					expire(fmt.Sprintf("reached its %ds lifetime", m.MaxLifetimeSeconds))
				default:
				}
				return
			case <-deadline.C:
				expire(fmt.Sprintf("reached its %ds lifetime", m.MaxLifetimeSeconds))
				return
			case <-ownerC:
				if ownerDead == nil {
					ownerDead = make(chan bool, 1)
					go func() { ownerDead <- ownerDeadBy(m, scopeIDs, ownerLookup) }()
				}
			case dead := <-ownerResult:
				ownerDead = nil
				if dead {
					expire(fmt.Sprintf("declared owner pid %d is gone", m.OwnerPID))
					return
				}
			}
		}
	}()
	return func() {
		close(stop)
		// Final when this returns: fire is closed — and stays observable to
		// every later read — or the goroutine is gone and can never fire.
		// done is the only "never will" proof, and the loop never blocks
		// inside an owner read, so it always answers stop promptly.
		select {
		case <-done:
		case <-fireIn:
		}
	}
}

// throwawayOwnerDead reports whether the marker's declared owner is PROVEN
// gone. The verdicts mirror the orphan-sweep stamp logic: a process the code
// cannot look at, a pid in another namespace, or an unreadable scope is
// "unknown", and unknown is never dead.
func throwawayOwnerDead(m *throwawaysandbox.Marker) bool {
	return ownerDeadBy(m, throwawayScopeIDsFn, throwawayLookupFn)
}

// ownerDeadBy is throwawayOwnerDead with its fn vars already resolved — the
// watchdog hands them to its lookup worker before the worker starts, so a
// read that outlives the daemon never touches the package vars (#7620).
func ownerDeadBy(m *throwawaysandbox.Marker, scopeIDsFn func() (string, string), lookupFn func(int) (proctree.Process, error)) bool {
	if m.OwnerPID <= 0 {
		return false
	}
	curBoot, curNs := scopeIDsFn()
	// A stamp from another pid namespace names a number space this process
	// cannot read — the owner may be alive there.
	if m.OwnerNsID != "" && curNs != "" && m.OwnerNsID != curNs {
		return false
	}
	// Different boots are proof of death only when both ids are the kernel's
	// strong UUID; the pidns fallback can disagree across a shared namespace.
	if m.OwnerBootID != "" && curBoot != "" && m.OwnerBootID != curBoot &&
		!proctree.BootIDIsFallback(m.OwnerBootID) && !proctree.BootIDIsFallback(curBoot) {
		return true
	}
	cur, err := lookupFn(m.OwnerPID)
	switch {
	case errors.Is(err, proctree.ErrProcessExited), errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ESRCH):
		return true
	case err != nil:
		return false // could not look is not dead
	case m.OwnerStartID != 0 && cur.StartID != m.OwnerStartID:
		return true
	default:
		return false
	}
}
