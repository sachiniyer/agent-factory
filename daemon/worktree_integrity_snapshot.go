package daemon

import "github.com/sachiniyer/agent-factory/session"

type worktreeInspectionEntry struct {
	key      string
	repoID   string
	instance *session.Instance
}

// worktreeInspectionSnapshotCurrent rejects observations whose input changed
// while the asynchronous Git scan was running. Discarding the whole correlated
// scan preserves the last confirmed warning; an archived/remote snapshot must
// never clear a lane that became live/local before reconciliation.
func (m *Manager) worktreeInspectionSnapshotCurrent(entries []worktreeInspectionEntry, rows []session.InstanceData) bool {
	if len(entries) != len(rows) {
		return false
	}
	for index, entry := range entries {
		if !sameWorktreeInspectionIdentity(rows[index], entry.instance.ToInstanceData()) {
			return false
		}
	}

	// Check manager membership last so a lane added or replaced while the
	// per-instance identities were being serialized invalidates the scan too.
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.worktreeInspectionMembershipCurrentLocked(entries)
}

// worktreeInspectionMembershipCurrentLocked checks the non-instance half of a
// scan cohort. The caller holds m.mu, which keeps this roster fixed until it has
// finished applying the correlated result.
func (m *Manager) worktreeInspectionMembershipCurrentLocked(entries []worktreeInspectionEntry) bool {
	if len(m.instances) != len(entries) {
		return false
	}
	for _, entry := range entries {
		if m.instances[entry.key] != entry.instance {
			return false
		}
	}
	return true
}

func sameWorktreeInspectionIdentity(before, after session.InstanceData) bool {
	return session.SameWorktreeInspectionIdentity(before, after)
}
