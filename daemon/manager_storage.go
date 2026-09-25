package daemon

import "github.com/sachiniyer/agent-factory/session"

func (m *Manager) RefreshInstances() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.refreshLocked()
}

func (m *Manager) InstancesSnapshot() []*session.Instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	return daemonInstances(m.instances)
}

// SaveInstancesForShutdown writes the daemon's terminal checkpoint. A
// stand-down discharge whose durable marker-CLEAR failed is restored in memory
// and retried by the poll's FlushOwedSettlements (#4730); drainDaemon stops the
// poll before this checkpoint runs, so without draining that retry the snapshot
// would serialize the restored marker and a restart would re-arm a teardown the
// worker already stood down. Flush the pending clears first so the checkpoint
// reflects the stand-down decision rather than the marker the failed clear left
// behind.
func (m *Manager) SaveInstancesForShutdown() error {
	m.FlushOwedSettlements()
	return m.storage.SaveInstancesForShutdown(m.InstancesSnapshot())
}
