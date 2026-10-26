package daemon

import (
	"fmt"
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

// Manager.worktreeMissingUndurable holds rows (by stableSessionKey) whose
// in-memory worktree-missing flag transition has not reached disk yet because
// the write ceded to an in-flight operation or failed. The poll retries them;
// the flag itself is sticky, so the probe alone would never report the
// transition again. Entries are added only for a row the manager still tracks
// (settleWorktreeMissingWrite) and removed with the row
// (forgetSessionRuntimeStateLocked). The field's doc lives here because
// manager.go sits at its file-length limit.

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
	m.persistWorktreeMissing(repoID, instance)
}

// worktreeMissingRemedy names the resolution that actually exists for a row
// whose tracked worktree vanished, which depends on who owns the checkout and
// where the row is in its lifecycle (#5102). An external worktree (--here, the
// root session, a legacy adopted checkout) is the user's own working tree:
// archive refuses to relocate it and rebuild refuses to recreate it. Putting
// the checkout back does not resume the row either — the flag is sticky, and
// every path that clears it is one af cannot take for an external worktree. So
// the remedy is ending the row and creating a fresh one. Naming kill is safe
// here, unlike for an af-owned row: kill's cleanup returns before touching an
// external worktree, so neither the path (absent or reappeared) nor the branch
// is removed. An archived row already
// is archived; restoring it rebuilds the worktree from the kept branch. A live
// af-owned worktree resolves by archive, which keeps the branch.
func worktreeMissingRemedy(instance *session.Instance) string {
	switch {
	case instance.IsExternalWorktree():
		return fmt.Sprintf("it is your own checkout, not one af created, so af cannot archive, rebuild or re-adopt it; end this session with 'af sessions kill %q' (it removes nothing for a checkout af did not create — neither the path nor the branch) and, once the checkout is back, start a new one in it with 'af sessions create --here'", instance.Title)
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
// ceded: the flag stays in memory and the transition is queued for a later
// pass. A failed write is queued the same way, rather than left to the next
// unrelated checkpoint. It reports whether the write landed.
//
// The probe ran on a snapshot of the instance, outside every lock, so the
// session may have been killed while it ran: a bounded lstat on a stalled
// mount outlives a KillSession easily. The kill deletes the map entry and
// forgets the session's runtime state in one m.mu critical section, and then
// publishes its own removal. So whether the row is still tracked is decided
// under m.mu, and the publish and the retry queue happen in that SAME critical
// section: either they land before the kill's delete, and its removal event
// follows them, or they see the row gone and do nothing. Checked any earlier,
// a kill landing between the check and the publish would resurrect the
// session in every event-driven client, and the retry entry
// forgetSessionRuntimeStateLocked just cleared would be re-added with nothing
// left to ever take it (#5102).
func (m *Manager) persistWorktreeMissing(repoID string, instance *session.Instance) bool {
	if instance.GetInFlightOp() != session.OpNone {
		m.settleWorktreeMissingWrite(repoID, instance, nil, false)
		return false
	}
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	defer repoStartLock.Unlock()
	data := instance.ToInstanceData()
	if data.InFlightOp != session.OpNone {
		m.settleWorktreeMissingWrite(repoID, instance, nil, false)
		return false
	}
	err := persistInstanceData(repoID, data)
	// Memory has changed either way; clients converge on it, as with every
	// other poll publish.
	if !m.settleWorktreeMissingWrite(repoID, instance, &data, err == nil) {
		return false
	}
	if err != nil {
		m.warn().Printf("failed to persist worktree-missing state for %q (will retry): %v", instance.Title, err)
		return false
	}
	return true
}

// settleWorktreeMissingWrite is persistWorktreeMissing's tail, under m.mu: if
// instance is still the tracked row it publishes data (when there is a write to
// announce) and queues a retry for a write that did not land. It reports
// whether the row was still tracked; a killed row gets neither.
func (m *Manager) settleWorktreeMissingWrite(repoID string, instance *session.Instance, data *session.InstanceData, durable bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.tracksInstanceLocked(repoID, instance) {
		return false
	}
	if data != nil {
		m.publishEvent(agentproto.EventSessionUpdated, *data)
	}
	if !durable {
		if m.worktreeMissingUndurable == nil {
			m.worktreeMissingUndurable = make(map[string]struct{})
		}
		m.worktreeMissingUndurable[stableSessionKey(repoID, instance)] = struct{}{}
	}
	return true
}

// tracksInstanceLocked reports whether instance is still a row the manager
// holds for repoID. Identity, not title: a kill followed by a create under the
// same title installs a different instance at the same key. The keyed lookup
// covers the common case; the scan covers a row re-keyed by an archived-name
// rename since the snapshot was taken. Caller holds m.mu.
func (m *Manager) tracksInstanceLocked(repoID string, instance *session.Instance) bool {
	if m.instances[daemonInstanceKey(repoID, instance.Title)] == instance {
		return true
	}
	for _, current := range m.instances {
		if current == instance {
			return true
		}
	}
	return false
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
