package daemon

import (
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// remoteProbeConcurrency bounds how many off-box sessions the status sweep
// probes at once. The probes wait on the network, not the CPU, so the bound
// exists to cap open connections rather than to ration work (W4.1).
var remoteProbeConcurrency = 8

// remoteProbeSweepWait is how long one sweep waits for its off-box probes
// before moving on. A probe that outlives it keeps running and applies its own
// result when it lands; the sweep just stops being held hostage by it, so a
// wedged host cannot delay the next tick for any other session.
var remoteProbeSweepWait = 10 * time.Second

// remoteProbesActive holds the instances whose off-box probe is still running.
// A later sweep skips them instead of stacking a second probe behind the
// wedged one.
var remoteProbesActive sync.Map

// refreshStatusProbes runs refreshInstanceStatus for every target. Local
// sessions keep the serial pass they always had; off-box sessions run on a
// bounded fan-out with a per-sweep wait, so one slow or unreachable host
// delays only its own rows. Results apply independently: each probe writes
// through the same single-writer path as before, and none holds an instance
// lock across the remote call from this layer.
func (m *Manager) refreshStatusProbes(targets []worktreeProbeTarget) {
	var remote []worktreeProbeTarget
	for _, t := range targets {
		if t.instance == nil {
			continue
		}
		if t.instance.Capabilities().Workspace == session.WorkspaceRemote {
			remote = append(remote, t)
			continue
		}
		m.refreshInstanceStatusPublishingOutage(t.repoID, t.instance)
	}
	fanOutRemoteProbes(remote, m.refreshInstanceStatusPublishingOutage, func() (func(), bool) {
		if _, ok := m.beginBackgroundMutation(); !ok {
			return nil, false
		}
		return m.backgroundMutationWG.Done, true
	})
}

// fanOutRemoteProbes dispatches every target within one sweep budget. Targets
// beyond the concurrency bound wait for a slot rather than being dropped, so a
// healthy fleet larger than remoteProbeConcurrency is fully covered each
// sweep; only when the budget expires is the undispatched remainder discarded
// (they were never marked active, so wedged hosts cannot starve them).
func fanOutRemoteProbes(targets []worktreeProbeTarget, probe func(string, *session.Instance), admit func() (func(), bool)) {
	if len(targets) == 0 {
		return
	}
	sem := make(chan struct{}, max(remoteProbeConcurrency, 1))
	var wg sync.WaitGroup
	timer := time.NewTimer(remoteProbeSweepWait)
	defer timer.Stop()
dispatch:
	for _, t := range targets {
		select {
		case sem <- struct{}{}:
		case <-timer.C:
			// Re-arm so the join below also sees the expired budget.
			timer.Reset(0)
			break dispatch
		}
		if _, busy := remoteProbesActive.LoadOrStore(t.instance, struct{}{}); busy {
			<-sem
			continue
		}
		// Admission joins the probe to the manager's background-mutation
		// lifecycle, so shutdown's terminal checkpoint cannot be overwritten by a
		// probe that outlived its sweep.
		done, ok := admit()
		if !ok {
			remoteProbesActive.Delete(t.instance)
			<-sem
			continue
		}
		wg.Add(1)
		go func(t worktreeProbeTarget) {
			defer done()
			defer wg.Done()
			defer remoteProbesActive.Delete(t.instance)
			defer func() { <-sem }()
			probe(t.repoID, t.instance)
		}(t)
	}
	finished := make(chan struct{})
	go func() {
		wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-timer.C:
	}
}
