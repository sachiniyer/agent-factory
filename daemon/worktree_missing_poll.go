package daemon

import (
	"sync"

	"github.com/sachiniyer/agent-factory/session"
)

// worktreeProbeConcurrency bounds how many worktree presence probes one status
// pass runs at once (#5102). Each probe is already individually bounded
// (BoundedLstat's deadline), so a pass costs roughly
// ceil(stalled/worktreeProbeConcurrency) deadlines rather than one per session
// on a stalled mount — and BoundedLstat's per-path latch makes a path that
// already timed out answer immediately on the next pass. Small, because a
// healthy pass is a handful of cheap lstats and nothing gains from more.
const worktreeProbeConcurrency = 8

type worktreeProbeTarget struct {
	repoID   string
	instance *session.Instance
}

// refreshWorktreesMissing probes every eligible row's tracked worktree with
// bounded parallelism and returns when all have answered. It runs ahead of the
// serial tmux pass, and for live AND archived rows: an archive directory deleted
// outside af is as much a fact about the row as a live worktree that vanished.
//
// Eligibility mirrors the poll's own early returns for rows whose state belongs
// to someone else — a kill being finished, a startup af could not confirm, an
// operation in flight (whose own move would read as ENOENT). The op gate here is
// only a cheap pre-filter; RefreshWorktreeMissing re-checks it under the
// instance lock before it writes anything.
func (m *Manager) refreshWorktreesMissing(targets []worktreeProbeTarget) {
	sem := make(chan struct{}, worktreeProbeConcurrency)
	var wg sync.WaitGroup
	for _, target := range targets {
		inst := target.instance
		if inst == nil || inst.UserKilled() || inst.StartupStateUnknown() ||
			inst.GetInFlightOp() != session.OpNone {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(repoID string, inst *session.Instance) {
			defer wg.Done()
			defer func() { <-sem }()
			m.refreshWorktreeMissing(repoID, inst)
		}(target.repoID, inst)
	}
	wg.Wait()
}

// refreshWorktreeMissing folds one presence probe of the tracked worktree into
// the row's worktree-missing flag and, on a transition only, persists and
// publishes it so every listing surface picks it up (#5102). Steady state costs
// one bounded lstat and no write.
func (m *Manager) refreshWorktreeMissing(repoID string, instance *session.Instance) {
	missing, changed := instance.RefreshWorktreeMissing()
	if !changed {
		return
	}
	if missing {
		m.warn().Printf("session %q: tracked worktree %s is gone (deleted outside af); flagging it — archive it (branch kept) or kill it to resolve", instance.Title, instance.GetWorktreePath())
	}
	m.persistAndPublishInstance(repoID, instance)
}
