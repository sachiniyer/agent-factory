package daemon

import (
	"sync"

	"github.com/sachiniyer/agent-factory/agentproto"
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
//
// A transition whose write had to be abandoned (see persistWorktreeMissing) is
// remembered and retried on later passes: the flag is sticky, so the probe will
// never report that transition a second time.
func (m *Manager) refreshWorktreeMissing(repoID string, instance *session.Instance) {
	missing, changed := instance.RefreshWorktreeMissing()
	key := stableSessionKey(repoID, instance)
	if !changed && !m.takeWorktreeMissingUndurable(key) {
		return
	}
	if changed && missing {
		m.warn().Printf("session %q: tracked worktree %s is gone (deleted outside af); flagging it — %s; if a directory reappears at that path, inspect it before acting, since af cannot verify it is the session's", instance.Title, instance.GetWorktreePath(), worktreeMissingRemedy(instance))
	}
	if !m.persistWorktreeMissing(repoID, instance) {
		m.markWorktreeMissingUndurable(key)
	}
}

// worktreeMissingRemedy names the resolution that actually exists for a row
// whose tracked worktree vanished, which depends on who owns the checkout and
// where the row is in its lifecycle (#5102). An external worktree (--here, the
// root session, a legacy adopted checkout) is the user's own working tree:
// archive refuses to relocate it and rebuild refuses to recreate it, so the
// only remedy is the user putting their checkout back. An archived row already
// is archived; restoring it rebuilds the worktree from the kept branch. A live
// af-owned worktree resolves by archive, which keeps the branch.
func worktreeMissingRemedy(instance *session.Instance) string {
	switch {
	case instance.IsExternalWorktree():
		return "it is your own checkout, not one af created, so af cannot archive or rebuild it; put the checkout back at that path to resume the session"
	case instance.GetLiveness() == session.LiveArchived:
		return "restore it to rebuild the worktree from its kept branch"
	default:
		return "archive it to resolve (its branch is kept)"
	}
}

// persistWorktreeMissing writes and publishes the row for a flag transition
// with the ordering the ordinary poll writer uses
// (persistPollChangeWithIdleEvidence): a lock-free gate, then the WHOLE
// projection re-read and the op axis re-checked under the repo ordering lock.
// An operation that began after the probe — a handoff's OpReplacing, say —
// may have rewritten the row to an intermediate state its own executor will
// settle, and persistInstanceData strips the op axis, so writing that snapshot
// would let a crash reload the intermediate payload as settled. Such a write is
// ceded: it returns false, the flag stays in memory, and a later pass retries.
// A failed write also returns false, so the transition is retried rather than
// left to the next unrelated checkpoint.
func (m *Manager) persistWorktreeMissing(repoID string, instance *session.Instance) bool {
	if instance.GetInFlightOp() != session.OpNone {
		return false
	}
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	defer repoStartLock.Unlock()
	data := instance.ToInstanceData()
	if data.InFlightOp != session.OpNone {
		return false
	}
	err := persistInstanceData(repoID, data)
	// Memory has changed either way; clients converge on it, as with every
	// other poll publish.
	m.publishEvent(agentproto.EventSessionUpdated, data)
	if err != nil {
		m.warn().Printf("failed to persist worktree-missing state for %q (will retry): %v", instance.Title, err)
		return false
	}
	return true
}

func (m *Manager) markWorktreeMissingUndurable(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.worktreeMissingUndurable == nil {
		m.worktreeMissingUndurable = make(map[string]struct{})
	}
	m.worktreeMissingUndurable[key] = struct{}{}
}

func (m *Manager) takeWorktreeMissingUndurable(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.worktreeMissingUndurable[key]; !ok {
		return false
	}
	delete(m.worktreeMissingUndurable, key)
	return true
}
